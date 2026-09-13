package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/config"
	"github.com/docflow/docflow/internal/files"
	httpapi "github.com/docflow/docflow/internal/http"
	"github.com/docflow/docflow/internal/janitor"
	"github.com/docflow/docflow/internal/onlyoffice"
	"github.com/docflow/docflow/internal/settings"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/tasks"
	"github.com/docflow/docflow/internal/team"
	"github.com/docflow/docflow/internal/upload"
	"github.com/docflow/docflow/internal/webpkg"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	// 优雅退出：SIGINT/SIGTERM 取消 ctx（janitor 等后台任务随之停止），
	// HTTP server 与 asynq worker 在 ListenAndServe 返回后依次 Shutdown。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := gorm.Open(postgres.Open(cfg.DatabaseURL), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	auditStore := audit.NewStore(db)
	fileStore := files.NewStore(db)
	storage, err := newStorage(cfg)
	if err != nil {
		log.Fatal(err)
	}
	// 团队：GormStore 提供成员/写权限查询（后续可替换为 Casbin 适配器）。
	teamStore := team.NewGormStore(db)
	teamService := team.NewService(teamStore)
	fileStore.SetTeamWriter(teamStore.CanWrite)
	// 版本管理：每文件保留版本数上限（MAX_VERSIONS_PER_FILE，默认 5）为回退值；
	// 运行时经 system_settings 的 upload.max_versions_per_file 热读取覆盖
	//（读失败回退 config 值）。
	fileStore.SetMaxVersions(cfg.MaxVersionsPerFile)
	settingsStore := settings.NewStore(db)
	settingsStore.SetAuditRecorder(auditStore)
	fileStore.SetMaxVersionsProvider(func() int {
		n, err := settingsStore.GetInt(settings.KeyUploadMaxVersionsPerFile)
		if err != nil || n < 1 {
			return cfg.MaxVersionsPerFile
		}
		return n
	})
	// 团队文件读判定：任意在册成员（owner/editor/viewer）可读，成员变动实时生效。
	fileStore.SetTeamReader(func(userID, teamID uuid.UUID) (bool, error) {
		return teamStore.UserInAnyTeam(userID, []uuid.UUID{teamID})
	})
	userStore := auth.NewUserStore(db)
	uploadService := upload.NewService(upload.NewGormStore(db), storage, cfg.UploadSessionTTL, cfg.MaxFileSize, cfg.ScanEnabled, fileStore.ValidateFolder, fileStore.CreateUploadedFile)
	// 扫描结果与 scan 阶段耗时计入 Prometheus 指标（docflow_scan_results_total、
	// docflow_upload_processing_duration_seconds{stage=scan}）。
	uploadService.SetScanner(upload.NewCountingScanner(upload.SelectScanner(cfg.ScanEnabled, cfg.ClamAVAddr, cfg.ClamAVRequired, cfg.ClamAVTimeout)))
	// 上传链路「覆盖为新版本」：会话校验目标文件（CanWrite），Complete 时
	// AddVersion + PruneVersions（版本保留数经 settings 热读取）。
	uploadService.SetVersionTarget(fileStore.ValidateReplaceTarget, fileStore.ReplaceFileVersion)
	// 网页包（zip）安全预览：上传完成（文件落库）后自动尝试解包
	//（WEBPKG_ENABLED；失败置 blocked，不影响文件本身可用性），解包对象
	// 存于 webpkg/<public_id>/ 前缀，经 /content/<public_id>/<path> 提供。
	webpkgService := webpkg.NewService(webpkg.NewGormRepo(db), fileStore, storage, webpkg.Limits{
		MaxEntries:   cfg.WebpkgMaxEntries,
		MaxFileSize:  cfg.WebpkgMaxFileSize,
		MaxTotalSize: cfg.WebpkgMaxTotalSize,
		MaxDepth:     cfg.WebpkgMaxDepth,
	})
	webpkgService.SetEnabled(cfg.WebpkgEnabled)
	// 后台任务队列（多实例横向扩展：无本地状态、任意实例可处理）：
	// 默认 inprocess（进程内 goroutine，零依赖，行为与原内联实现一致）；
	// QUEUE_DRIVER=redis 时经 asynq 入队（启动 ping 校验失败即 fatal），
	// 由任意实例的 worker 消费。两种驱动共用同一组处理函数。
	completeUpload := tasks.CompleteUploadHandler(uploadService, auditStore)
	extractWebpkg := tasks.ExtractWebpkgHandler(webpkgService)
	var enqueuer tasks.Enqueuer
	var shutdownQueue func()
	switch cfg.QueueDriver {
	case "redis":
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := tasks.PingRedis(pingCtx, cfg.RedisAddr, cfg.RedisPassword); err != nil {
			cancel()
			log.Fatalf("queue driver redis: ping %s: %v", cfg.RedisAddr, err)
		}
		cancel()
		enqueuer = tasks.NewRedisAsynq(cfg.RedisAddr, cfg.RedisPassword)
		// 本实例同时作为 worker 消费任务（任意实例入队的任务均可处理）。
		server := tasks.NewAsynqServer(cfg.RedisAddr, cfg.RedisPassword, cfg.QueueConcurrency)
		if err := server.Start(tasks.NewMux(completeUpload, extractWebpkg)); err != nil {
			log.Fatalf("asynq worker: %v", err)
		}
		shutdownQueue = func() { server.Shutdown() }
		log.Printf("queue driver: redis (addr=%s concurrency=%d)", cfg.RedisAddr, cfg.QueueConcurrency)
	default:
		enqueuer = tasks.NewInProcess(completeUpload, extractWebpkg)
		log.Print("queue driver: inprocess")
	}
	// 上传完成钩子：网页包自动解包统一经队列派发（inprocess 时即原
	//「goroutine 内 AutoExtract」行为；zip 候选判定在处理侧）。
	uploadService.SetFileCompleteHook(func(fileID uuid.UUID) {
		if err := enqueuer.EnqueueExtractWebpkg(fileID); err != nil {
			log.Printf("[tasks] enqueue extract-webpkg %s: %v", fileID, err)
		}
	})
	shareService := share.NewService(share.NewGormStore(db), fileStore)
	shareService.SetTeamMembership(teamStore)
	shareService.SetUserDirectory(userStore)
	service := auth.NewService(auth.NewGormSessionStore(db), cfg.JWTSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	router := gin.Default()
	handler := httpapi.NewHandler(service, userStore, fileStore, shareService, teamService, uploadService, storage, cfg.CookieSecure, cfg.CookieDomain, cfg.RefreshTokenTTL)
	handler.SetAuditRecorder(auditStore)
	// 后台补完任务经队列派发（tus PATCH 写满后入队；inprocess 行为不变）。
	handler.SetTaskEnqueuer(enqueuer)
	// 管理端：系统设置（system_settings）、基础统计与 admin 角色查询。
	handler.SetSettingsService(settingsStore)
	handler.SetStatsSource(httpapi.NewAdminStats(db))
	handler.SetRoleLookup(userStore)
	// ONLYOFFICE 集成：启用时注入服务（挂载 /api/v1/onlyoffice 路由；
	// 编辑配置/下载 token 用 ONLYOFFICE_JWT_SECRET 签名，回调保存复用
	// AddVersion+Prune 的版本链路，下载大小上限沿用 MAX_FILE_SIZE）。
	if cfg.OnlyOfficeEnabled {
		onlyofficeSvc := onlyoffice.New(onlyoffice.Config{
			ServerURL:        cfg.OnlyOfficeServerURL,
			PublicURL:        cfg.OnlyOfficePublicURL,
			DownloadBase:     cfg.OnlyOfficeDownloadURLBase,
			JWTSecret:        cfg.OnlyOfficeJWTSecret,
			TokenTTL:         onlyoffice.DefaultTokenTTL,
			DownloadMaxBytes: cfg.MaxFileSize,
		}, fileStore, storage, userStore.Username, auditStore)
		// 回调幂等持久化：onlyoffice_callbacks 表（migration 012），
		// UNIQUE (file_id, document_key, callback_url) 即设计幂等键，
		// 跨重启/多实例去重，失败回滚记录允许 DocumentServer 重试。
		onlyofficeSvc.SetCallbackStore(onlyoffice.NewGormCallbackStore(db))
		handler.SetOnlyOffice(onlyofficeSvc, cfg.OnlyOfficeRateLimitPerMinute)
		log.Printf("onlyoffice integration enabled (server=%s public=%s)", cfg.OnlyOfficeServerURL, onlyofficeSvc.PublicServerURL())
	}
	// Prometheus 指标：全局 HTTP 中间件在 Register 内挂载；/metrics 端点由
	// METRICS_ENABLED 控制（默认启用，无认证，生产由 Caddy/网络层限制访问）。
	handler.SetMetricsEnabled(cfg.MetricsEnabled)
	// 网页包内容端点 /content/:pid/*filepath（独立按 IP 轻限流）与手动解包入口。
	handler.SetWebpkg(webpkgService, cfg.WebpkgRateLimitPerMinute)
	handler.Register(router, cfg.JWTSecret, cfg.RateLimitPerMinute, cfg.LoginRateLimitPerMinute, cfg.PublicRateLimitPerMinute)
	// 后台清理任务（janitor）：过期上传会话、deleting blob 回收与回收站超期清理。
	if cfg.JanitorEnabled {
		j := janitor.New(janitor.NewGormRepo(db), fileStore, storage, settingsStore, auditStore)
		j.SetInterval(cfg.JanitorInterval)
		go j.RunForever(ctx)
		log.Printf("janitor enabled (interval=%s)", cfg.JanitorInterval)
	} else {
		log.Print("janitor disabled by JANITOR_ENABLED")
	}
	server := &http.Server{Addr: ":" + cfg.Port, Handler: router}
	go func() {
		<-ctx.Done()
		log.Print("shutdown signal received, draining ...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}()
	log.Printf("DocFlow backend listening on :%s", cfg.Port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	// HTTP 停止后依次收尾：asynq worker（等待在处理任务）与入队客户端。
	if shutdownQueue != nil {
		shutdownQueue()
	}
	if closer, ok := enqueuer.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// newStorage 按 STORAGE_DRIVER 选择存储实现（local | s3）。
func newStorage(cfg config.Config) (upload.Storage, error) {
	switch cfg.StorageDriver {
	case "s3":
		log.Printf("storage driver: s3 (bucket=%s endpoint=%s path_style=%t)", cfg.S3Bucket, cfg.S3Endpoint, cfg.S3PathStyle)
		return upload.NewS3Storage(cfg.S3Endpoint, cfg.S3Bucket, cfg.S3Region, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3PathStyle)
	default:
		log.Printf("storage driver: local (root=%s)", cfg.StorageRoot)
		return upload.NewLocalStorage(cfg.StorageRoot)
	}
}
