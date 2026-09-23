package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/acl"
	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/caddytls"
	"github.com/docflow/docflow/internal/collab"
	"github.com/docflow/docflow/internal/config"
	"github.com/docflow/docflow/internal/contenturl"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/group"
	httpapi "github.com/docflow/docflow/internal/http"
	"github.com/docflow/docflow/internal/invite"
	"github.com/docflow/docflow/internal/janitor"
	"github.com/docflow/docflow/internal/mail"
	"github.com/docflow/docflow/internal/notify"
	"github.com/docflow/docflow/internal/oidc"
	"github.com/docflow/docflow/internal/onlyoffice"
	"github.com/docflow/docflow/internal/readiness"
	"github.com/docflow/docflow/internal/realtime"
	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/settings"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/space"
	"github.com/docflow/docflow/internal/tagging"
	"github.com/docflow/docflow/internal/tasks"
	"github.com/docflow/docflow/internal/upload"
	"github.com/docflow/docflow/internal/webhook"
	"github.com/docflow/docflow/internal/webpkg"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	// 优雅退出：SIGINT/SIGTERM 取消 ctx（janitor 等后台任务随之停止）；
	// ListenAndServe 返回后按序收尾：在途后台任务（inprocess Close /
	// asynq Shutdown，均带超时）→ 入队客户端。
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
	// 空间（统一空间模型，migration 040）：GormStore 提供成员/用户组与角色
	// 查询（直接成员 ∪ 组成员取最高），Service 按五级内置角色矩阵求值权限。
	spaceStore := space.NewGormStore(db)
	spaceService := space.NewService(spaceStore)
	// 空间写权限（上传/建目录/追加版本/重命名/恢复）：角色矩阵含 write。
	fileStore.SetSpaceWriter(spaceService.CanWrite)
	// 空间删除权限（软删除/彻底删除）：角色矩阵含 delete。
	fileStore.SetSpaceDeleter(spaceService.CanDelete)
	// 路径级 ACL：空间作用域文件/目录的 read/write/delete/share 判定先
	// 求值 folder_acl 链（近覆盖远、同节点 deny 优先、user 覆盖 space），
	// 链上无适用条目回退空间角色判定（无 ACL 行为完全不变）。
	aclService := acl.NewService(acl.NewGormRepo(db))
	fileStore.SetACLResolver(aclService.ResolveForFile)
	// 版本管理：每文件保留版本数上限（MAX_VERSIONS_PER_FILE，默认 5）为回退值；
	// 运行时经 system_settings 的 upload.max_versions_per_file 热读取覆盖
	//（读失败回退 config 值）。
	fileStore.SetMaxVersions(cfg.MaxVersionsPerFile)
	settingsStore := settings.NewStore(db)
	settingsStore.SetAuditRecorder(auditStore)
	// 空间配额运行时配置（settings 热读取）：新空间默认配额/配额上限。
	spaceService.SetQuotaDefaultsProvider(func() (int64, int64) {
		def, derr := settingsStore.GetInt(settings.KeySpaceDefaultQuota)
		if derr != nil || def < 0 {
			def = int(space.DefaultDefaultQuota)
		}
		max, merr := settingsStore.GetInt(settings.KeySpaceMaxQuota)
		if merr != nil || max < 0 {
			max = int(space.DefaultMaxQuota)
		}
		return int64(def), int64(max)
	})
	// 每用户空间数上限（settings 热读取）。
	spaceService.SetMaxSpacesProvider(func() int {
		n, err := settingsStore.GetInt(settings.KeySpaceMaxPerUser)
		if err != nil || n < 1 {
			return space.DefaultMaxSpaces
		}
		return n
	})
	fileStore.SetMaxVersionsProvider(func() int {
		n, err := settingsStore.GetInt(settings.KeyUploadMaxVersionsPerFile)
		if err != nil || n < 1 {
			return cfg.MaxVersionsPerFile
		}
		return n
	})
	// 版本保留时间窗（G6）：upload.version_retention_days 热读取；0 = 不启用
	//（仅按数量上限裁剪，与既有行为一致），读取失败回退 0。
	fileStore.SetVersionRetentionDaysProvider(func() int {
		n, err := settingsStore.GetInt(settings.KeyUploadVersionRetentionDays)
		if err != nil || n < 0 {
			return 0
		}
		return n
	})
	// 目录最大深度（G6）：folder.max_depth 热读取（根为 1），读取失败回退 32。
	fileStore.SetMaxFolderDepthProvider(func() int {
		n, err := settingsStore.GetInt(settings.KeyFolderMaxDepth)
		if err != nil || n < 1 {
			return 32
		}
		return n
	})
	// 空间文件读判定：CanRead——任意在册成员（直接成员或经用户组）可读，
	// 实时生效（成员/组变动即时反映）。
	fileStore.SetSpaceReader(spaceService.CanRead)
	userStore := auth.NewUserStore(db)
	// 新用户开户默认配额（C3）：system_settings 的 upload.default_quota 热读取
	//（邀请注册 / OIDC 自动开户共用 CreateUser 回填；读失败回退 10GiB 常量）。
	userStore.SetDefaultQuotaProvider(func() int64 {
		n, err := settingsStore.GetInt(settings.KeyUploadDefaultQuota)
		if err != nil || n < 1 {
			return auth.DefaultStorageQuota
		}
		return int64(n)
	})
	// 站内通知与通知偏好（migration 017）：Dispatcher 检查偏好
	//（无记录=默认开启）后落库；HTTP 通知端点与各业务回调共用。
	notifyService := notify.NewService(notify.NewGormStore(db), notify.NewGormPreferenceRepo(db))
	realtimeHub := realtime.NewHub()
	notifyService.SetRealtimeSink(func(n notify.Notification) { realtimeHub.Broadcast(n.UserID, n) })
	// 空间文件版本更新通知（file.updated）：AddVersion 事务提交后回调
	//（files.dispatchVersionAdded 已过滤——actor≠owner）；异步通知空间
	// 全部参与者（直接成员 ∪ 组成员，文件 owner 除外，避免噪音）。
	fileStore.SetNotifyDispatcher(func(f files.File, actor uuid.UUID, version files.FileVersion) {
		spaceID := f.SpaceID
		go func() {
			members, err := spaceService.MemberUserIDs(spaceID)
			if err != nil {
				log.Printf("[notify] list space %s members: %v", spaceID, err)
				return
			}
			recipients := make([]uuid.UUID, 0, len(members))
			for _, m := range members {
				if m != f.OwnerID {
					recipients = append(recipients, m)
				}
			}
			if len(recipients) == 0 {
				return
			}
			actorName := "空间成员"
			if name, err := userStore.Username(actor); err == nil && name != "" {
				actorName = name
			}
			body := fmt.Sprintf("%s 更新了共享文件「%s」（新版本 v%d）。", actorName, f.Name, version.Version)
			if err := notifyService.NotifyMany(recipients, notify.EventFileUpdated, "共享文件已更新："+f.Name, body, f.ID); err != nil {
				log.Printf("[notify] file.updated %s: %v", f.ID, err)
			}
		}()
	})
	// files.CreateUploadedFile 返回 (fileID, newBlob, err)（blob 内容去重）：
	// newBlob=false 表示同 sha256 复用既有 blob，Complete 侧据 !newBlob
	// 清理冗余物理对象（upload.Service.createFile 同签名直传）。
	uploadService := upload.NewService(upload.NewGormStore(db), storage, cfg.UploadSessionTTL, cfg.MaxFileSize, cfg.ScanEnabled, fileStore.ValidateFolder, fileStore.CreateUploadedFile)
	// 扫描结果与 scan 阶段耗时计入 Prometheus 指标（docflow_scan_results_total、
	// docflow_upload_processing_duration_seconds{stage=scan}）。
	uploadService.SetScanner(upload.NewCountingScanner(upload.SelectScanner(cfg.ScanEnabled, cfg.ClamAVAddr, cfg.ClamAVRequired, cfg.ClamAVTimeout)))
	// 空间文件版本删除通知（file.version.deleted）：DeleteVersion 事务提交后
	// 回调（files.dispatchVersionDeleted 已过滤——删除者≠owner）；异步通知
	// 文件 owner 该文件的一个历史版本被删除。
	fileStore.SetVersionDeletedDispatcher(func(f files.File, actor uuid.UUID, version files.FileVersion) {
		go func() {
			actorName := "空间成员"
			if name, err := userStore.Username(actor); err == nil && name != "" {
				actorName = name
			}
			title := "文件版本已删除：" + f.Name
			body := fmt.Sprintf("%s 删除了文件「%s」的历史版本 v%d；当前版本不受影响。", actorName, f.Name, version.Version)
			if err := notifyService.Notify(f.OwnerID, notify.EventFileVersionDeleted, title, body, f.ID); err != nil {
				log.Printf("[notify] file.version.deleted %s: %v", f.ID, err)
			}
		}()
	})
	// 上传链路「覆盖为新版本」：会话校验目标文件（CanWrite），Complete 时
	// AddVersion + PruneVersions（版本保留数经 settings 热读取）。
	uploadService.SetVersionTarget(fileStore.ValidateReplaceTarget, fileStore.ReplaceFileVersion)
	// 单文件大小上限热读取：system_settings 的 upload.max_file_size 覆盖
	// MAX_FILE_SIZE（Start/StartReplace 建会话校验与 Tus-Max-Size 头均生效）；
	// 读失败或非法值回退构造值。
	uploadService.SetMaxSizeProvider(func() int64 {
		n, err := settingsStore.GetInt(settings.KeyUploadMaxFileSize)
		if err != nil || n < 1 {
			return cfg.MaxFileSize
		}
		return int64(n)
	})
	// 单次 PATCH 请求体上限（PATCH_MAX_BYTES，默认 64MiB）。
	uploadService.SetPatchMaxBytes(cfg.PatchMaxBytes)
	// 每用户并发上传会话上限（G6）：upload.max_concurrent_uploads_per_user
	// 热读取（GormStore DB COUNT 计数，多实例共享同一计数来源）；读失败
	// 回退 settings 默认 3。
	uploadService.SetMaxConcurrentUploadsProvider(func() int {
		n, err := settingsStore.GetInt(settings.KeyMaxConcurrentUploads)
		if err != nil || n < 1 {
			return 3
		}
		return n
	})
	// 上传扩展名黑名单（G6）：upload.blocked_extensions 热读取（逗号分隔，
	// 默认空 = 不拦截）；建会话与 Complete 双侧校验，命中 400。
	uploadService.SetBlockedExtensionsProvider(func() []string {
		raw, err := settingsStore.GetString(settings.KeyUploadBlockedExtensions)
		if err != nil || strings.TrimSpace(raw) == "" {
			return nil
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			out = append(out, strings.TrimSpace(part))
		}
		return out
	})
	// 站内通知接线（upload.completed / upload.quarantined）：完成路径
	//（新文件/覆盖新版本成功）与隔离终态通知属主，标题含文件名。
	uploadService.SetNotifyDispatcher(notifyDispatch(notifyService, "upload"))
	// 存储配额（C3，设计 3.2.1/6.3.1/6.12.4）：建会话时校验（已用含软删文件，
	// 超限 403 QUOTA_EXCEEDED）；上传成功后用量 >80% 发 quota.warning 站内
	// 通知（异步、每次超过都发——不做阈值去重，取舍见设计 6.12.4）。
	uploadService.SetQuotaCheck(fileStore.CheckUploadQuota)
	uploadService.SetQuotaWarnDispatcher(quotaWarnDispatcher(userStore, fileStore, notifyService))
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
	// Purge 清理网页包对象：files.Store 经回调删除 webpkg/<public_id>/ 前缀
	//（webpkg.Remove 清单驱动，best-effort），不改 upload.Storage 接口；
	// HTTP purge 与 janitor sweepTrash 均经 files.Store.Purge 统一生效。
	fileStore.SetWebpkgCleaner(func(prefix string) error {
		webpkg.Remove(storage, prefix)
		return nil
	})
	// Webhook 通知渠道（v1.1）：用户自助注册回调 URL，站内通知事件转发
	//（HMAC 签名投递经 task:webhook-delivery 队列异步执行，见下方接线）。
	webhookStore := webhook.NewGormStore(db)
	webhookService := webhook.NewService(webhookStore)
	webhookDispatcher := webhook.NewDispatcher(webhookStore)
	// 后台任务队列（多实例横向扩展：无本地状态、任意实例可处理）：
	// 默认 inprocess（进程内 goroutine，零依赖，行为与原内联实现一致）；
	// QUEUE_DRIVER=redis 时经 asynq 入队（启动 ping 校验失败即 fatal），
	// 由任意实例的 worker 消费。两种驱动共用同一组处理函数。
	// 全文检索（SEARCH_DRIVER 装配 pg|meili）：索引构建经 task:search-index
	// 队列异步执行（见下方 fileComplete 钩子接线）。meili 为 v2 可选项：
	// 启动 EnsureIndex 校验连通性（失败即退出）；访问过滤需要用户空间列表，
	// 由 spaceStore.ListForUser 提供（不扩 Repo.QueryDocs 签名，pg/memory
	// 实现与既有调用方零改动）。
	var searchRepo search.Repo = search.NewGormRepo(db)
	if cfg.SearchDriver == "meili" {
		meili := search.NewMeiliRepo(cfg.MeiliURL, cfg.MeiliAPIKey, func(user uuid.UUID) ([]uuid.UUID, error) {
			spaces, err := spaceStore.ListForUser(user)
			if err != nil {
				return nil, err
			}
			ids := make([]uuid.UUID, 0, len(spaces))
			for _, sp := range spaces {
				ids = append(ids, sp.ID)
			}
			return ids, nil
		})
		if err := meili.EnsureIndex(ctx); err != nil {
			log.Fatalf("search driver meili: ensure index: %v", err)
		}
		searchRepo = meili
		log.Printf("search driver: meili (%s)", cfg.MeiliURL)
	} else {
		log.Print("search driver: pg")
	}
	searchStore := search.NewStore(searchRepo)
	searchIndexer := search.NewIndexer(searchStore, db, storage)
	// Vector RAG：embedding Provider/模型热切换（免重启）。AI 启用即装配
	// Qdrant 单例客户端（构造不建连接），embedding 与 collection 派生名
	// 每次索引/检索时按当前热配置解析（见下方 resolveVectors）；RAGMode/
	// vector_enabled 的门槛判断保留在使用时——未启用向量路径则零 Qdrant
	// 请求（与旧行为一致）。collection 按 provider+model 派生：切模型即
	// 换库（旧库保留），切换后用 POST /admin/settings/ai/reindex 重建。
	var vectorClient *ai.QdrantClient
	if cfg.AIEnabled && cfg.RAGQdrantURL != "" {
		vectorClient = ai.NewQdrantClient(cfg.RAGQdrantURL, cfg.RAGCollectionPrefix+"global", 10*time.Second)
		log.Printf("vector RAG armed for hot config (qdrant=%s; embedding resolved per request)", cfg.RAGQdrantURL)
	} else {
		log.Print("vector RAG disabled; using keyword retrieval")
	}
	completeUpload := tasks.CompleteUploadHandler(uploadService, auditStore)
	extractWebpkg := tasks.ExtractWebpkgHandler(webpkgService)
	searchIndex := tasks.SearchIndexHandler(searchIndexer)
	webhookDelivery := webhookDispatcher.DeliverTask
	var enqueuer tasks.Enqueuer
	var inProcess *tasks.InProcess
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
		if err := server.Start(tasks.NewMux(completeUpload, extractWebpkg, searchIndex, webhookDelivery)); err != nil {
			log.Fatalf("asynq worker: %v", err)
		}
		// Shutdown 等待在处理任务结束；包一层超时防止个别任务卡死拖住退出。
		shutdownQueue = func() {
			done := make(chan struct{})
			go func() {
				server.Shutdown()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				log.Print("[tasks] asynq shutdown timed out")
			}
		}
		log.Printf("queue driver: redis (addr=%s concurrency=%d)", cfg.RedisAddr, cfg.QueueConcurrency)
	default:
		inProcess = tasks.NewInProcess(completeUpload, extractWebpkg, searchIndex, webhookDelivery)
		enqueuer = inProcess
		log.Print("queue driver: inprocess")
	}
	// WebSocket 跨实例广播桥：QUEUE_DRIVER=redis 时复用队列同一 Redis
	//（REDIS_ADDR/REDIS_PASSWORD，连通性已由上方 PingRedis 一并校验），
	// 经 docflow:notify Pub/Sub 频道把站内通知扇出到所有实例的本地连接
	//（Hub 本地直发 + 远端订阅分发，msg_id seen 去重回环）；inprocess 下
	// Noop 桥保持单实例纯本地行为。
	var broadcaster realtime.Broadcaster = realtime.NoopBroadcaster{}
	if cfg.QueueDriver == "redis" {
		broadcaster = realtime.NewRedisBroadcaster(cfg.RedisAddr, cfg.RedisPassword)
		log.Printf("ws broadcast: redis (channel=%s)", "docflow:notify")
	}
	realtimeHub.SetBroadcaster(broadcaster)
	// 上传完成钩子（新建与覆盖版本两条成功路径均触发，见 upload.Complete）：
	// 网页包自动解包与全文索引构建统一经队列派发（inprocess 时即原
	//「goroutine 内联执行」行为；候选判定在处理侧）。
	uploadService.SetFileCompleteHook(func(fileID uuid.UUID) {
		if err := enqueuer.EnqueueExtractWebpkg(fileID); err != nil {
			log.Printf("[tasks] enqueue extract-webpkg %s: %v", fileID, err)
		}
		if err := enqueuer.EnqueueSearchIndex(fileID); err != nil {
			log.Printf("[tasks] enqueue search-index %s: %v", fileID, err)
		}
	})
	shareService := share.NewService(share.NewGormStore(db), fileStore)
	shareService.SetSpaceMembership(spaceStore)
	// 目录分享树源（tree/raw-share 子资源解析与清单，生产恒注入）。
	shareService.SetTreeSource(fileStore)
	// 空间文件分享门控：仅 CanShare（owner/admin/member_share）可创建空间
	// 文件分享；文件行 owner 语义由 Get 的授权链保证。
	shareService.SetSpaceSharer(spaceService.CanShare)
	// 分享判定的路径级 ACL：folder_acl 链命中（matched）时优先于空间角色。
	shareService.SetACLResolver(aclService.ResolveForFile)
	shareService.SetUserDirectory(userStore)
	// 站内通知接线（share.accessed）：公开/私有分享下载成功（计数已消耗）
	// 通知分享 owner，标题含文件名（私有分享 owner 本人下载不通知）。
	shareService.SetNotifyDispatcher(notifyDispatch(notifyService, "share"))
	// 分享默认有效期热读取：创建请求未指定有效期（expires_in==0）时采用
	// share.default_expiry_hours（小时）；未设置/读失败回退既有「永久」行为。
	shareService.SetDefaultExpiryProvider(func() int {
		n, err := settingsStore.GetInt(settings.KeyShareDefaultExpiryHours)
		if err != nil || n < 1 {
			return 0
		}
		return n
	})
	// 水印默认值热读取：创建请求未显式指定 watermark_enabled / watermark_text
	// 时采用 share.default_watermark / share.watermark_text；未设置/读失败
	// 回退内置默认（开启 + "{date} {name}"）。
	shareService.SetPublicEnabledProvider(func() bool {
		v, err := settingsStore.GetBool(settings.KeySharePublicEnabled)
		return err != nil || v
	})
	shareService.SetWatermarkDefaultsProvider(func() (bool, string) {
		enabled := true
		if v, err := settingsStore.GetBool(settings.KeyShareDefaultWatermark); err == nil {
			enabled = v
		}
		text := share.DefaultWatermarkTemplate
		if v, err := settingsStore.GetString(settings.KeyShareWatermarkText); err == nil && v != "" {
			text = v
		}
		return enabled, text
	})
	service := auth.NewService(auth.NewGormSessionStore(db), cfg.JWTSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	// 密码管理：改密/重置所需的凭据读写与一次性重置令牌存储
	//（password_reset_tokens，migration 014）。
	service.SetCredentials(userStore)
	service.SetPasswordResetStore(auth.NewGormPasswordResetStore(db))
	// 个人访问令牌（PAT）：api_tokens（migration 016），Bearer dfpat_ 前缀
	// 凭证经 RequireAccessToken 双路径校验（见 internal/auth）。
	service.SetTokenStore(auth.NewGormAPITokenStore(db))
	// 两步验证（TOTP，v2 设计）：user_totp（migration 020）；enabled 用户
	// /login 一律 401 TOTP_REQUIRED，经 /auth/login/totp 二段提交。
	service.SetTOTPStore(auth.NewGormTOTPStore(db))
	// 邀请制注册：邀请生命周期管理 + 邮件通道（邀请/重置链接）。
	// SMTP 投递参数支持运行时调整：每次发送读 system_settings 的 smtp.*
	// 入库覆盖（管理端 /admin/settings/smtp 写入）回退 env 基线；通道禁用
	// 时回退 Noop（仅日志输出链接，不建立任何网络连接）。PUBLIC_BASE_URL
	// 用于拼接邮件中的绝对链接。
	inviteService := invite.NewService(invite.NewGormRepo(db), userStore)
	smtpEnv := settings.SMTPSettings{
		Enabled: cfg.SMTPEnabled,
		Host:    cfg.SMTPHost,
		Port:    cfg.SMTPPort,
		User:    cfg.SMTPUser,
		Pass:    cfg.SMTPPass,
		From:    cfg.SMTPFrom,
		TLSMode: settings.SMTPTLSModeAuto,
	}
	smtpSource := func() (mail.SMTPConfig, bool) {
		effective := smtpEnv
		if ov, err := settingsStore.SMTPOverrides(); err == nil {
			effective = ov.Apply(smtpEnv)
		}
		return mail.SMTPConfig{
			Host: effective.Host, Port: effective.Port,
			User: effective.User, Pass: effective.Pass,
			From: effective.From, TLSMode: effective.TLSMode,
		}, effective.Enabled
	}
	mailer := mail.NewSettingsMailer(smtpSource, mail.NewNoopMailer())
	if cfg.SMTPEnabled {
		log.Printf("mail transport: smtp (%s:%d from=%s; runtime overrides via /admin/settings/smtp)", cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPFrom)
	} else {
		log.Print("mail transport: env disabled (invitation/reset links are logged only until enabled via /admin/settings/smtp)")
	}
	// 邮件通知渠道（v1.1）：通知落库后按用户邮箱发送纯文本副本（Noop 时
	// 仅日志）；与站内通知共用同一偏好开关（偏好关闭时两者一并短路）。
	notifyService.SetMailNotifier(func(uid uuid.UUID, eventType, title, body string) {
		user, err := userStore.GetByID(uid)
		if err != nil {
			log.Printf("[notify] mail: lookup user %s: %v", uid, err)
			return
		}
		if err := mailer.SendNotification(user.Email, title, body); err != nil {
			log.Printf("[notify] mail: send to %s: %v", user.Email, err)
		}
	})
	// Webhook 通知渠道接线：通知落库后查该用户启用了该事件的 hook，
	// 逐个组装投递载荷入队 task:webhook-delivery（inprocess/redis 均可消费；
	// 投递由 webhook.Dispatcher 执行：签名 + 超时 10s + 3 次退避重试 +
	// 连续失败自动禁用）。
	notifyService.SetWebhookEnqueuer(func(uid uuid.UUID, eventType, title, body string, resourceID uuid.UUID) {
		hooks, err := webhookService.HooksForEvent(uid, eventType)
		if err != nil {
			log.Printf("[notify] webhook: list hooks for user %s: %v", uid, err)
			return
		}
		for _, hook := range hooks {
			var resource *uuid.UUID
			if resourceID != uuid.Nil {
				id := resourceID
				resource = &id
			}
			payload, err := webhook.MarshalDelivery(hook.ID, uid, eventType, title, body, resource)
			if err != nil {
				log.Printf("[notify] webhook: marshal delivery for hook %s: %v", hook.ID, err)
				continue
			}
			if err := enqueuer.EnqueueWebhookDelivery(payload); err != nil {
				log.Printf("[tasks] enqueue webhook-delivery for hook %s: %v", hook.ID, err)
			}
		}
	})
	router := gin.Default()
	// 可信代理（TRUSTED_PROXIES，逗号分隔 CIDR/IP）：控制 gin ClientIP 是否
	// 采信 X-Forwarded-For。默认空 = 不信任任何代理（ClientIP 取 RemoteAddr），
	// 防止客户端伪造 XFF 绕过按 IP 限流与审计记录；解析失败直接 fatal。
	if err := router.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		log.Fatalf("TRUSTED_PROXIES: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	handler := httpapi.NewHandler(service, userStore, fileStore, shareService, spaceService, uploadService, storage, cfg.CookieSecure, cfg.CookieDomain, cfg.RefreshTokenTTL)
	handler.SetCommentsDB(db)
	handler.SetReadinessChecker(readiness.New(sqlDB, cfg, storage))
	handler.SetAuditRecorder(auditStore)
	handler.SetAuditQuerySource(auditStore)
	handler.SetRealtimeHub(realtimeHub, cfg.AllowedOrigins, cfg.Environment)
	handler.SetWSSecret(cfg.JWTSecret)
	// 富文本实时协作房间（internal/collab）：每实例内存房间（Manager），
	// WS 端点 /api/v1/collab/:fileId/ws 复用上方 JWT 子协议认证，加入前
	// 经 files 的 ValidateReplaceTarget 校验目标文件写权限；总开关为
	// system_settings 的 collab.enabled（默认启用，热读取）。
	handler.SetCollabManager(collab.NewManager())
	handler.SetBackupDir(cfg.BackupDir)
	// 标签与收藏：Tag CRUD / 文件打去标签 / is_starred / 列表过滤
	//（文件读授权复用 fileStore.Get 的 authorizeFileAccess 语义）。
	handler.SetTagging(tagging.NewService(tagging.NewGormRepo(db), fileStore))
	// 全文检索：GET /api/v1/search（文件名 + 文本内容；索引构建经队列）。
	handler.SetSearch(searchStore)
	// 路径级 ACL 管理端点（GET/PUT /api/v1/folders/:id/acl）。
	handler.SetACL(aclService)
	// AI 摘要（v2 可落地子集）：OpenAI 兼容 /chat/completions；AI_ENABLED=false
	// 时端点恒注册并返回 503 AI_DISABLED。
	handler.SetAI(ai.New(cfg.AIEnabled, cfg.AIBaseURL, cfg.AIAPIKey, cfg.AIModel))
	if cfg.AIEnabled {
		log.Printf("ai summary enabled (base=%s model=%s)", cfg.AIBaseURL, cfg.AIModel)
	}
	// AI 能力第一版（多 Provider ChatService）：system_settings 的 ai.* 键
	//（管理端 CRUD）为运行时配置，env（AI_*）合成 id=env 的兜底 Provider；
	// 每次请求热读取（DB 覆盖 → env 回退）。用量记录 ai_usage（migration
	// 044），管理面板按用户聚合。
	aiEnvBaseline := settings.DefaultAIConfig()
	aiEnvBaseline.RAG = settings.AIRAGConfig{Mode: cfg.RAGMode, VectorEnabled: cfg.RAGVectorEnabled, QdrantURL: cfg.RAGQdrantURL, CollectionPrefix: cfg.RAGCollectionPrefix, EmbeddingProvider: cfg.RAGEmbeddingProvider, EmbeddingModel: cfg.RAGEmbeddingModel, TopK: cfg.RAGTopK, ChunkSize: cfg.RAGChunkSize, ChunkOverlap: cfg.RAGChunkOverlap}
	if cfg.AIEnabled {
		aiEnvBaseline.Providers = []settings.AIProvider{{
			ID: "env", Name: "Env (AI_*)", Kind: settings.AIKindOpenAICompatible,
			BaseURL: cfg.AIBaseURL, APIKey: cfg.AIAPIKey, Model: cfg.AIModel, Enabled: true,
		}}
		aiEnvBaseline.DefaultProvider = "env"
	}
	aiUsageStore := ai.NewUsageStore(db)
	aiService := ai.NewService(func() (settings.AIConfig, error) {
		effective := aiEnvBaseline
		if ov, _, err := settingsStore.AIOverrides(); err == nil {
			effective.Enabled = ov.Enabled // 总开关（ai.enabled；nil = 自动判定）
			if len(ov.Providers) > 0 {
				effective.Providers = ov.Providers
			}
			if ov.DefaultProvider != "" {
				effective.DefaultProvider = ov.DefaultProvider
			}
			effective.Temperature = ov.Temperature
			effective.MaxTokens = ov.MaxTokens
			effective.PerUserPerMin = ov.PerUserPerMin
			if ov.RAG.Mode != "" {
				effective.RAG = ov.RAG
			}
			// OCR 为整体 JSON 块（同 RAG 块语义）：载荷未带（零值）保持
			// env 基线（默认关闭）；带块即热生效（AIOverrides 已钳制
			// max_image_bytes）。
			if ov.OCR != (settings.AIOCRConfig{}) {
				effective.OCR = ov.OCR
			}
		}
		return effective, nil
	})
	aiService.SetUsageStore(aiUsageStore)
	aiService.SetSearcher(searchStore)
	// 外部 MCP 工具（ai.mcp 键热读取）：use_mcp 对话经工具循环调用外部
	// MCP 服务器；未装配/读取失败时静默降级为普通对话。
	aiService.SetMCPReader(func() []settings.AIMCPServiceDef {
		list, _ := settingsStore.AIMCPServices()
		return list
	})
	if vectorClient != nil {
		ragEffective := func() settings.AIRAGConfig {
			effective := aiEnvBaseline.RAG
			if ov, _, err := settingsStore.AIOverrides(); err == nil && ov.RAG.Mode != "" {
				effective = ov.RAG
			}
			return effective
		}
		// 向量路径热解析（索引与检索共用）：RAG mode/开关未启用 →
		// ErrVectorDisabled（索引安静跳过、检索降级关键词，零 Qdrant
		// 连接）；启用 → ResolveEmbedding 按当前 Provider/模型派生
		// collection，取 Qdrant 单例的 Scope 视图（同配置同库、切模型
		// 即换库）。
		resolveVectors := func() (ai.EmbeddingProvider, ai.VectorStore, error) {
			rag := ragEffective()
			if rag.Mode != "hybrid" || !rag.VectorEnabled {
				return nil, nil, ai.ErrVectorDisabled
			}
			embedding, collection, err := aiService.ResolveEmbedding()
			if err != nil {
				return nil, nil, err
			}
			return embedding, vectorClient.Scope(collection), nil
		}
		searchIndexer.SetVectorIndexer(&ai.VectorIndexer{Resolve: resolveVectors, ChunkSize: cfg.RAGChunkSize, Overlap: cfg.RAGChunkOverlap})
		hybrid := &ai.HybridRetriever{Keyword: searchStore, Resolve: resolveVectors, CurrentVersion: func(id uuid.UUID) (uuid.UUID, error) {
			var f files.File
			if err := db.Select("current_version_id").Where("id = ? AND deleted_at IS NULL", id).First(&f).Error; err != nil {
				return uuid.Nil, err
			}
			if f.CurrentVersionID == nil {
				return uuid.Nil, nil
			}
			return *f.CurrentVersionID, nil
		}}
		aiService.SetHybridRetriever(hybrid, ragEffective)
	}
	handler.SetAIService(aiService, aiEnvBaseline, aiUsageStore)
	// Agent 容器平台 AI IPC 网关（agentsock.go）：任务创建时签发一次性
	// 令牌，断网容器（NetworkMode=none）经 unix socket POST /chat 回调
	// 平台默认对话模型（零值 ChatRequest：禁用工具/联网/记忆/思考）；
	// 用量按任务归属用户记账（ForUser）。
	agentAI := httpapi.NewAgentAIGateway(func(ctx context.Context, user uuid.UUID, system string, messages []ai.Message, maxTokens int) (string, string, error) {
		res, err := aiService.ForUser(user).Chat(ctx, ai.ChatRequest{System: system, Messages: messages, MaxTokens: maxTokens}, nil)
		if err != nil {
			return "", "", err
		}
		return res.Content, res.ProviderID + "/" + res.Model, nil
	})
	agentAI.SetAuditRecorder(auditStore)
	handler.SetAgentAI(agentAI)
	// socket 服务：/run/docflow-ipc/ai.sock（DOCFLOW_AGENT_IPC_DIR 可配，
	// compose 经 named volume docflow-agent-ipc 与 agent 容器共享）。
	// 监听失败（如本机开发无 /run 权限）仅告警降级：agent 任务照常运行，
	// 容器内无 AI 能力（runner 降级为执行 prompt 中的 ```run 块）。
	if stopIPC, ipcErr := httpapi.StartAgentIPCServer(ctx, os.Getenv("DOCFLOW_AGENT_IPC_DIR"), agentAI.Handler()); ipcErr != nil {
		log.Printf("agent ai ipc socket disabled: %v", ipcErr)
	} else {
		defer stopIPC()
		ipcDir := os.Getenv("DOCFLOW_AGENT_IPC_DIR")
		if ipcDir == "" {
			ipcDir = httpapi.AgentAIIPCDefaultDir
		}
		log.Printf("agent ai ipc socket listening on %s/%s (POST /chat)", ipcDir, httpapi.AgentAISockName)
	}
	// 图片 OCR 自动入索引：aiService 读 blob 需要存储读取器；把它挂到
	// 索引器（*ai.Service 满足 search.OCRExtractor，编译期保证），OCR
	// 未开启时 ExtractImageText 安静返回空、索引行为不变。
	aiService.SetStorageReader(storage)
	searchIndexer.SetOCR(aiService)
	// 站内通知：列表/已读/未读数与通知偏好端点（本人维度）。
	handler.SetNotifications(notifyService)
	// Webhook 通知渠道端点（本人维度）：注册/列举/启停/删除。
	handler.SetWebhooks(webhookService)
	// 后台补完任务经队列派发（tus PATCH 写满后入队；inprocess 行为不变）。
	handler.SetTaskEnqueuer(enqueuer)
	// 管理端：系统设置（system_settings）、基础统计与 admin 角色查询。
	handler.SetSettingsService(settingsStore)
	handler.SetAgentDB(db)
	// 重建索引端点（POST /admin/settings/ai/reindex）的文件列表源
	//（files 表游标分页直查；切换 embedding 模型后重建新 collection 用）。
	handler.SetReindexLister(httpapi.NewReindexLister(db))
	handler.SetWebDAV(auth.NewWebDAVStore(db))
	handler.SetStatsSource(httpapi.NewAdminStats(db))
	handler.SetRoleLookup(userStore)
	// 用户组管理（migration 035）：组 CRUD 与成员维护（仅 admin 路由组）。
	handler.SetGroups(group.NewService(group.NewGormStore(db)))
	// HTTPS 运行时切换（管理页面）：CADDY_ADMIN_ADDR 配置时经 Caddy admin
	// API 热下发；启动期对账覆盖 caddy 先于 backend 重启丢配置的窗口。
	// custom 模式证书目录（TLS_CERT_DIR，compose 共享卷默认 /data/tls）
	// 保存管理页上传的证书/私钥。
	if cfg.CaddyAdminAddr != "" {
		caddyTLS := caddytls.NewService(db, cfg.CaddyAdminAddr, cfg.TLSCertDir)
		caddyTLS.SetAuditRecorder(auditStore)
		handler.SetCaddyTLS(caddyTLS)
		go caddyTLS.ReapplyStartup(ctx)
		log.Printf("caddy tls runtime switching enabled (admin=%s)", cfg.CaddyAdminAddr)
	}
	// 隔离区管理（G6，仅 admin）：隔离 blob 列表与 rescan/release/delete
	// 处置（复用 fileStore.PurgeBlobs 的两步物理删除语义；扫描器与上传
	// 链路同一选型的独立实例）。
	handler.SetQuarantineService(httpapi.NewQuarantineService(db, storage,
		upload.NewCountingScanner(upload.SelectScanner(cfg.ScanEnabled, cfg.ClamAVAddr, cfg.ClamAVRequired, cfg.ClamAVTimeout)), fileStore))
	// C9 登录失败锁定策略与 C10 refresh/logout 同源严格校验（CSRF_STRICT）。
	handler.SetLoginLockout(cfg.LoginMaxRetries, cfg.LoginLockDuration)
	handler.SetCSRFStrict(cfg.CSRFStrict)
	// 个人仪表盘概览统计（v1.1）：文件聚合复用 fileStore，分享/上传计数直查 DB。
	handler.SetDashboardSource(httpapi.NewDashboardSource(fileStore, db))
	// 邀请制注册与邮件通道（POST /api/v1/admin/invitations、/api/v1/auth/register、
	// /forgot-password、/reset-password）。
	handler.SetInvites(inviteService, mailer, cfg.PublicBaseURL)
	// 换绑邮箱（账号安全，v2.4）：验证码存储（email_change_codes，migration
	// 042）+ 账号读写源；邮件经同一 mailer 通道投递到新邮箱。
	handler.SetEmailChange(auth.NewGormEmailChangeStore(db), userStore)
	// OIDC 单点登录（v2）：启用时启动即拉取发现文档（失败 fatal——IdP
	// 不可达则 SSO 形同虚设，宁可拒启）；login/callback 路由随之注册，
	// config 探测端点恒注册（禁用时 enabled=false）。用户映射：sub 关联
	//（oidc_links，migration 021）→ email 匹配 → 自动开户
	//（OIDC_AUTO_PROVISION，默认 true）。SSO 信任 IdP 认证强度，跳过
	// 本地 TOTP 二验（密码登录的 TOTP 拦截不变）。
	if cfg.OIDCEnabled {
		provider, err := oidc.Discover(ctx, cfg.OIDCIssuer)
		if err != nil {
			log.Fatalf("oidc discovery: %v", err)
		}
		oidcSvc := oidc.NewService(oidc.New(provider, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCRedirectURL), oidc.NewGormLinkStore(db), userStore, cfg.OIDCAutoProvision)
		oidcSvc.SetAuditRecorder(auditStore)
		handler.SetOIDC(oidcSvc)
		log.Printf("oidc sso enabled (issuer=%s redirect=%s auto_provision=%t)", cfg.OIDCIssuer, cfg.OIDCRedirectURL, cfg.OIDCAutoProvision)
	}
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
		// 保存回调写鉴权（fail closed 的唯一放行出口）：以验签 claims 的
		// users[0] 为 actor，复用「覆盖为新版本」的写权限判定（个人 owner、
		// 团队 CanWrite）。未接线时保存回调一律拒绝、编辑配置降级只读。
		onlyofficeSvc.SetWriteAuthorizer(func(user, fileID uuid.UUID) error {
			_, err := fileStore.ValidateReplaceTarget(user, fileID)
			return err
		})
		handler.SetOnlyOffice(onlyofficeSvc, cfg.OnlyOfficeRateLimitPerMinute)
		log.Printf("onlyoffice integration enabled (server=%s public=%s)", cfg.OnlyOfficeServerURL, onlyofficeSvc.PublicServerURL())
	}
	// Prometheus 指标：全局 HTTP 中间件在 Register 内挂载；/metrics 端点由
	// METRICS_ENABLED 控制（默认启用，无认证，生产由 Caddy/网络层限制访问）。
	handler.SetMetricsEnabled(cfg.MetricsEnabled)
	// draw.io 图表编辑集成：仅注入配置（编辑器为浏览器侧 iframe embed，
	// postMessage JSON 协议；官方 webapp 静态层由 caddy 镜像 /drawio/* 服务，
	// 后端零依赖）。config 探测端点恒注册（禁用时 enabled=false）。
	handler.SetDrawio(cfg.DrawioEnabled, cfg.DrawioServerURL, cfg.DrawioPublicURL)
	if cfg.DrawioEnabled {
		log.Printf("drawio integration enabled (server=%s public=%s)", cfg.DrawioServerURL, cfg.DrawioPublicURL)
	}
	// 网页包内容端点 /content/:pid/*filepath（独立按 IP 轻限流）与手动解包入口。
	handler.SetWebpkg(webpkgService, cfg.WebpkgRateLimitPerMinute)
	// 受控原始内容（/raw/*）：短期授权 HMAC grant 签发器（RAW_URL_SECRET，
	// 缺省由 JWT_SECRET 经 HKDF 派生）与 origin_content 绝对化基地址
	//（CONTENT_PUBLIC_BASE_URL，可选）。resolve API 据此签发 raw_url。
	rawSecret := cfg.RawURLSecret
	if rawSecret == "" {
		rawSecret = cfg.JWTSecret
	}
	handler.SetContentSigner(contenturl.NewSigner(rawSecret, contenturl.DefaultTTL))
	handler.SetContentPublicBaseURL(cfg.ContentPublicBaseURL)
	handler.Register(router, cfg.JWTSecret, cfg.RateLimitPerMinute, cfg.LoginRateLimitPerMinute, cfg.PublicRateLimitPerMinute)
	// 后台清理任务（janitor）：过期上传会话、deleting blob 回收与回收站超期清理。
	// 回收站超期清理走系统级 PurgeSystem（不做用户 CanDelete 判定——清理的是
	// 全部用户的超期项，与 HTTP purge 入口的授权删除区分）。
	if cfg.JanitorEnabled {
		j := janitor.New(janitor.NewGormRepo(db), systemPurger{store: fileStore}, storage, settingsStore, auditStore)
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
	// HTTP 已 Shutdown（见上方 goroutine）。退出序列：在途后台任务 →
	// asynq worker（带超时）→ 入队客户端。
	if inProcess != nil {
		if !inProcess.Close(10 * time.Second) {
			log.Print("[tasks] inprocess close: timed out waiting for in-flight tasks")
		}
	}
	if shutdownQueue != nil {
		shutdownQueue()
	}
	if closer, ok := enqueuer.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	// 广播桥最后收尾（HTTP 已 Shutdown，本地连接均已在 Register cleanup 摘除）。
	if closer, ok := broadcaster.(interface{ Close() error }); ok {
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

// systemPurger 将 files.Store 的系统级彻底删除适配为 janitor.Purger：
// 回收站超期清理不做用户 CanDelete 判定（HTTP purge 入口走 Store.Purge 的
// 授权版本），owner 参数仅为满足接口、实际忽略。
type systemPurger struct{ store *files.Store }

func (p systemPurger) Purge(_, id uuid.UUID) ([]files.File, []files.ObjectBlob, error) {
	return p.store.PurgeSystem(id)
}

func (p systemPurger) PurgeBlobs(blobs []files.ObjectBlob, deleteObject func(string) error) error {
	return p.store.PurgeBlobs(blobs, deleteObject)
}

// notifyDispatch 把 upload/share 的通知回调适配为 notify.Dispatcher 调用
// （source 仅用于错误日志定位）；写库失败只记日志，不影响业务主流程。
func notifyDispatch(dispatcher notify.Dispatcher, source string) func(uuid.UUID, string, string, string, uuid.UUID) {
	return func(userID uuid.UUID, eventType, title, body string, resourceID uuid.UUID) {
		if err := dispatcher.Notify(userID, eventType, title, body, resourceID); err != nil {
			log.Printf("[notify] %s event %s for user %s: %v", source, eventType, userID, err)
		}
	}
}

// quotaWarnPercent 为配额用量警告阈值（用量百分比 ≥ 该值时通知，C3）。
const quotaWarnPercent = 80

// quotaWarnDispatcher 构造上传成功后的配额用量警告回调（异步执行）：
// 读取已用（软删计入）与配额，达到阈值（默认 80%）即发 quota.warning 站内
// 通知；每次超过都发（不做阈值去重——多实例部署下内存去重不可靠，且上传
// 频次受配额约束，通知量可控）。查询/投递失败仅记日志。
func quotaWarnDispatcher(users *auth.UserStore, fileStore *files.Store, notifier notify.Dispatcher) func(uuid.UUID, string, uuid.UUID) {
	return func(userID uuid.UUID, fileName string, fileID uuid.UUID) {
		go func() {
			quota, err := users.StorageQuota(userID)
			if err != nil {
				log.Printf("[notify] quota.warning: read quota for user %s: %v", userID, err)
				return
			}
			if quota <= 0 {
				return
			}
			used, err := fileStore.UsedStorage(userID)
			if err != nil {
				log.Printf("[notify] quota.warning: read usage for user %s: %v", userID, err)
				return
			}
			percent := used * 100 / quota
			if percent < quotaWarnPercent {
				return
			}
			title := fmt.Sprintf("存储用量已达 %d%%", percent)
			body := fmt.Sprintf("文件「%s」上传后，你的存储用量为 %s / %s（约 %d%%）。超出配额后将无法继续上传；可清理回收站（彻底删除后才释放配额）或联系管理员调整配额。", fileName, humanBytes(used), humanBytes(quota), percent)
			if err := notifier.Notify(userID, notify.EventQuotaWarning, title, body, fileID); err != nil {
				log.Printf("[notify] quota.warning for user %s: %v", userID, err)
			}
		}()
	}
}

// humanBytes 把字节数格式化为人类可读的 GiB/MiB 文案（通知正文用）。
func humanBytes(n int64) string {
	const gib = 1 << 30
	if n >= gib {
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(gib))
	}
	const mib = 1 << 20
	if n >= mib {
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(mib))
	}
	return fmt.Sprintf("%d B", n)
}
