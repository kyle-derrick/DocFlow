package config

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port             string
	DatabaseURL      string
	JWTSecret        string
	AccessTokenTTL   time.Duration
	RefreshTokenTTL  time.Duration
	CookieSecure     bool
	CookieDomain     string
	StorageRoot      string
	MaxFileSize      int64
	ScanEnabled      bool
	UploadSessionTTL time.Duration
	// TrustedProxies 为可信代理 CIDR/IP 列表（TRUSTED_PROXIES，逗号分隔）；
	// 默认空 = 不信任任何代理（ClientIP 取 RemoteAddr，忽略 X-Forwarded-For），
	// 防止客户端伪造 XFF 绕过按 IP 限流；条目合法性由 gin SetTrustedProxies 校验。
	TrustedProxies []string
	// StorageDriver 选择存储驱动：local（默认）| s3。
	StorageDriver string
	// S3 兼容对象存储配置（SeaweedFS/MinIO/AWS），STORAGE_DRIVER=s3 时生效。
	S3Endpoint  string
	S3Bucket    string
	S3Region    string
	S3AccessKey string
	S3SecretKey string
	S3PathStyle bool
	// ClamAVAddr 为 clamd INSTREAM 地址（host:port），SCAN_ENABLED 时配合使用。
	ClamAVAddr string
	// ClamAVTimeout 为单次扫描的超时时间。
	ClamAVTimeout time.Duration
	// ClamAVRequired 为 true 时 clamd 不可达即拒绝（fail closed）；
	// false 时拨号失败降级放行并记录警告日志。
	ClamAVRequired bool
	// RateLimitPerMinute 为 /api/v1 认证接口的基础限流（每分钟次数，0 表示禁用）。
	RateLimitPerMinute int
	// LoginRateLimitPerMinute 为登录接口单独限流（每分钟次数，0 表示禁用）。
	LoginRateLimitPerMinute int
	// PublicRateLimitPerMinute 为公开分享接口单独限流，按 IP（每分钟次数，0 表示禁用）。
	PublicRateLimitPerMinute int
	// MaxVersionsPerFile 每文件保留的版本数上限（覆盖上传后裁剪历史版本）；
	// 运行时可被 system_settings 的 upload.max_versions_per_file 热覆盖（读取失败回退本值）。
	MaxVersionsPerFile int
	// JanitorEnabled 控制后台清理任务（janitor）；env JANITOR_ENABLED 默认 true
	//（兼容任务书旧拼写 JANIOR_ENABLED 作为别名）。
	JanitorEnabled bool
	// JanitorInterval 为 janitor 清理周期（默认 10m）。
	JanitorInterval time.Duration
	// OnlyOfficeEnabled 控制 ONLYOFFICE Document Server 集成（默认 false）；
	// 未启用时后端不注册 session/download/callback 路由（请求 404），
	// config 探测端点恒注册并返回 {enabled:false, server_url:null}。
	OnlyOfficeEnabled bool
	// OnlyOfficeServerURL 为 DocumentServer 内网基地址（如 http://onlyoffice:80），
	// 同时是回调下载 URL 防 SSRF 校验的同源基准（始终以此为准）；启用时必填。
	OnlyOfficeServerURL string
	// OnlyOfficePublicURL 为浏览器可达的 DocumentServer 地址（如
	// https://example.com/onlyoffice 或本地直连 http://localhost:8081），
	// 仅用于 /onlyoffice/config 返回给前端加载 api.js；为空时回退
	// OnlyOfficeServerURL（保持既有行为），不影响 SSRF 校验基准。
	OnlyOfficePublicURL string
	// OnlyOfficeJWTSecret 与 DocumentServer 共享的 JWT 签名密钥（HS256），
	// 启用时必填且 ≥32 字节。
	OnlyOfficeJWTSecret string
	// OnlyOfficeDownloadURLBase 为 DocumentServer 回源访问后端用的基地址
	//（编辑配置中的 document.url 与 callbackUrl 前缀，默认 http://backend:8080）。
	OnlyOfficeDownloadURLBase string
	// OnlyOfficeRateLimitPerMinute 为 onlyoffice 公开组（download/callback）
	// 独立按 IP 轻限流（每分钟次数，默认 60；0 表示禁用）。
	OnlyOfficeRateLimitPerMinute int
	// MetricsEnabled 控制 GET /metrics 端点（env METRICS_ENABLED，默认 true）。
	// 端点无认证：生产环境应由反向代理（Caddy）或网络层限制访问。
	MetricsEnabled bool
	// DrawioEnabled 控制 draw.io 图表编辑集成（DRAWIO_ENABLED，默认 false）：
	// 编辑器为浏览器侧 iframe embed（postMessage JSON 协议），后端不与 drawio
	// 服务通信，仅恒注册 /api/v1/drawio/config 探测端点（禁用时
	// {enabled:false, url:null}）；保存走「上传 file_id 覆盖新版本」通用链路。
	DrawioEnabled bool
	// DrawioServerURL 为 drawio 服务内网基地址（如 http://drawio:8080，
	// compose full profile）；DRAWIO_PUBLIC_URL 未配置时作为 config 端点
	// 返回给前端的回退地址（内网名浏览器通常不可达，生产应配置 PUBLIC_URL）。
	DrawioServerURL string
	// DrawioPublicURL 为浏览器可达的 drawio 地址（如 https://example.com/drawio
	// 或本地直连 http://localhost:8082），/drawio/config 优先返回；
	// 为空时回退 DrawioServerURL。
	DrawioPublicURL string
	// WebpkgEnabled 控制网页包（zip）上传完成后的自动解包（默认 true）；
	// 关闭后仍可经 POST /api/v1/files/:id/webpkg/extract 手动解包。
	WebpkgEnabled bool
	// WebpkgMaxEntries 网页包解包条目数上限（默认 500，WEBPKG_MAX_ENTRIES）。
	WebpkgMaxEntries int
	// WebpkgMaxFileSize 单文件展开大小上限（默认 32MiB，WEBPKG_MAX_FILE_SIZE）。
	WebpkgMaxFileSize int64
	// WebpkgMaxTotalSize 解包展开总大小上限（默认 256MiB，WEBPKG_MAX_TOTAL_SIZE）。
	WebpkgMaxTotalSize int64
	// WebpkgMaxDepth 解包目录深度上限（默认 10，WEBPKG_MAX_DEPTH）。
	WebpkgMaxDepth int
	// WebpkgRateLimitPerMinute 为 /content 内容端点独立按 IP 轻限流
	//（默认 120，WEBPKG_RATE_LIMIT_PER_MIN）。
	WebpkgRateLimitPerMinute int
	// QueueDriver 选择后台任务队列驱动（QUEUE_DRIVER）：inprocess（默认，
	// 进程内 goroutine，零依赖）| redis（asynq，多实例横向扩展）。
	// WebSocket 站内通知的跨实例广播（realtime.RedisBroadcaster，
	// docflow:notify Pub/Sub）复用同一开关与 Redis 连接参数：redis 时
	// 启用跨实例扇出，inprocess 时 Noop（单实例纯本地分发）。
	QueueDriver string
	// RedisAddr 为 Redis 地址 host:port（默认 localhost:6379，
	// QUEUE_DRIVER=redis 时使用）。
	RedisAddr string
	// RedisPassword 为 Redis 密码（可选，REDIS_PASSWORD）。
	RedisPassword string
	// QueueConcurrency 为 redis 驱动下每实例并行处理任务数
	//（QUEUE_CONCURRENCY，默认 5）。
	QueueConcurrency int
	// PatchMaxBytes 单次上传 PATCH 请求体上限（PATCH_MAX_BYTES，默认
	// 64MiB，与 upload.DefaultPatchMaxBytes 一致）；接线 upload.Service.
	// SetPatchMaxBytes，约束单请求的存储写入量与连接占用时长。
	PatchMaxBytes int64
	// SMTPEnabled 控制邮件通道（SMTP_ENABLED，默认 false）：false 时使用
	// Noop 邮件通道（仅日志输出邀请/重置链接，不建立任何网络连接）。
	SMTPEnabled bool
	// SMTPHost/SMTPPort/SMTPUser/SMTPPass 为 SMTP 服务器连接与认证参数
	//（PORT 默认 587；USER 为空表示匿名投递）。
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	// SMTPFrom 为发件人地址（启用 SMTP 时必填）。
	SMTPFrom string
	// PublicBaseURL 为站点对外基地址（PUBLIC_BASE_URL，如
	// https://docflow.example.com）：拼接邀请注册与密码重置邮件里的链接；
	// 为空时邮件/日志输出相对路径 /register/<token>、/reset/<token>。
	PublicBaseURL string
	// CaddyAdminAddr 为 Caddy admin API 地址（CADDY_ADMIN_ADDR，如 caddy:2019）：
	// 管理页面 HTTPS 运行时切换（/api/v1/admin/tls）经其 POST /load 热下发；
	// 为空（默认）时功能关闭，TLS 完全由部署配置（APP_DOMAIN env）决定。
	CaddyAdminAddr string
	// TLSCertDir 为 HTTPS 自定义证书模式的证书目录（TLS_CERT_DIR，默认
	// /data/tls）：管理页上传的 cert.pem/key.pem 落盘于此（0600），
	// compose 中与 caddy 共享同一卷（backend 可写、caddy 只读）。
	TLSCertDir string
	// OIDCEnabled 控制 OIDC 单点登录（OIDC_ENABLED，默认 false）：false 时
	// 后端不注册 /api/v1/auth/oidc/login 与 callback 路由（404），config
	// 探测端点恒注册并返回 {enabled:false}。
	OIDCEnabled bool
	// OIDCIssuer 为 IdP 签发方基地址（如 https://accounts.google.com），
	// 启动时拉取 {issuer}/.well-known/openid-configuration 发现文档
	//（失败即 fatal）；启用时必填且须为绝对 http(s) URL。
	OIDCIssuer string
	// OIDCClientID / OIDCClientSecret 为 IdP 侧注册的应用凭据，启用时必填。
	OIDCClientID     string
	OIDCClientSecret string
	// OIDCRedirectURL 为授权码回调地址，缺省取 {PUBLIC_BASE_URL}/api/v1/auth/oidc/callback；
	// PUBLIC_BASE_URL 与本值均未设置（且已启用）时校验失败。
	OIDCRedirectURL string
	// OIDCAutoProvision 控制自动开户（OIDC_AUTO_PROVISION，默认 true）：
	// IdP 身份未关联既有用户且邮箱无匹配时自动创建 role=user 的随机密码
	// 账号；false 时无匹配一律 403（引导联系管理员）。
	OIDCAutoProvision bool
	// AccessSalt 为分享访问事件 IP 哈希的静态盐（ACCESS_SALT）：
	// file_access_events.ip_hash = SHA-256(AccessSalt || ip)，明文 IP 不落库。
	// 未设置时由 JWT secret 派生（"docflow-access:" 前缀），避免额外必填项。
	AccessSalt string
	// LoginMaxRetries 为连续登录失败锁定阈值（LOGIN_MAX_RETRIES，默认 5）：
	// 达到后账号锁定 LOGIN_LOCK_MINUTES（C9，设计 6.1.3/7.3）。
	LoginMaxRetries int
	// LoginLockDuration 为账号锁定时长（LOGIN_LOCK_MINUTES，默认 15 分钟）。
	LoginLockDuration time.Duration
	// CSRFStrict 控制 refresh/logout 的同源严格校验（CSRF_STRICT，默认 true，
	// 设计 6.1.5）：true 时缺失 Origin/Referer 一律拒绝（浏览器 POST 均携带
	// Origin）；false 时放行无两头请求，供 curl 等非浏览器客户端使用。
	CSRFStrict     bool
	AllowedOrigins []string
	Environment    string
	BackupDir      string
	// AIEnabled 控制 AI 文件摘要集成（AI_ENABLED，默认 false）：false 时
	// POST /files/:id/ai/summary 返回 503 AI_DISABLED（路由恒注册）。
	AIEnabled bool
	// AIBaseURL 为 OpenAI 兼容服务基地址（AI_BASE_URL，默认
	// https://api.openai.com/v1，可指向任意兼容网关），启用时须为绝对
	// http(s) URL。
	AIBaseURL string
	// AIAPIKey 为上游 API Key（AI_API_KEY，启用时必填；密钥只走环境变量）。
	AIAPIKey string
	// AIModel 为摘要模型（AI_MODEL，默认 gpt-4o-mini）。
	AIModel              string
	RAGMode              string
	RAGVectorEnabled     bool
	RAGQdrantURL         string
	RAGCollectionPrefix  string
	RAGEmbeddingProvider string
	RAGEmbeddingModel    string
	RAGTopK              int
	RAGChunkSize         int
	RAGChunkOverlap      int
	// SearchDriver 选择全文检索引擎（SEARCH_DRIVER）：pg（默认，PostgreSQL
	// 原生 ILIKE+tsvector）| meili（Meilisearch，v2 可选项）。
	SearchDriver string
	// MeiliURL 为 Meilisearch 基地址（MEILI_URL，meili 驱动时必填且须为
	// 绝对 http(s) URL；启动时 EnsureIndex 失败即退出）。
	MeiliURL string
	// MeiliAPIKey 为 Meilisearch API Key（MEILI_API_KEY，可空——未设
	// MASTER_KEY 的本地实例）。
	MeiliAPIKey string
	// ContentPublicBaseURL 为受控原始内容（/raw/*）的对外基地址
	//（CONTENT_PUBLIC_BASE_URL，可选）：resolve API 以 origin_content=1
	// 请求时拼接绝对 raw_url（跨 origin 内容域场景）；未配置回退相对路径。
	ContentPublicBaseURL string
	// RawURLSecret 为 /raw/* 短期授权（HMAC grant）的签名密钥源
	//（RAW_URL_SECRET，可选，≥32 字节）；未配置时由 JWT_SECRET 经 HKDF
	// 派生（contenturl 包内域分离标签），避免新增必填配置。
	RawURLSecret string
}

func Load() (Config, error) {
	a, e := durationEnv("ACCESS_TOKEN_TTL", 15*time.Minute)
	if e != nil {
		return Config{}, e
	}
	r, e := durationEnv("REFRESH_TOKEN_TTL", 7*24*time.Hour)
	if e != nil {
		return Config{}, e
	}
	secure, e := boolEnv("COOKIE_SECURE", true)
	if e != nil {
		return Config{}, e
	}
	scan, e := boolEnv("SCAN_ENABLED", false)
	if e != nil {
		return Config{}, e
	}
	ttl, e := durationEnv("UPLOAD_SESSION_TTL", 24*time.Hour)
	if e != nil {
		return Config{}, e
	}
	max, e := int64Env("MAX_FILE_SIZE", 2<<30)
	if e != nil {
		return Config{}, e
	}
	rateLimit, e := intEnv("RATE_LIMIT_PER_MIN", 120)
	if e != nil {
		return Config{}, e
	}
	loginRateLimit, e := intEnv("LOGIN_RATE_LIMIT_PER_MIN", 10)
	if e != nil {
		return Config{}, e
	}
	publicRateLimit, e := intEnv("PUBLIC_RATE_LIMIT_PER_MIN", 60)
	if e != nil {
		return Config{}, e
	}
	maxVersions, e := intEnv("MAX_VERSIONS_PER_FILE", 5)
	if e != nil {
		return Config{}, e
	}
	pathStyle, e := boolEnv("S3_PATH_STYLE", true)
	if e != nil {
		return Config{}, e
	}
	clamavTimeout, e := durationEnv("CLAMAV_TIMEOUT", 5*time.Minute)
	if e != nil {
		return Config{}, e
	}
	clamavRequired, e := boolEnv("CLAMAV_REQUIRED", true)
	if e != nil {
		return Config{}, e
	}
	janitorEnabled := true
	if v, ok := firstEnv("JANITOR_ENABLED", "JANIOR_ENABLED"); ok {
		b, e := strconv.ParseBool(v)
		if e != nil {
			return Config{}, errors.New("JANITOR_ENABLED must be a boolean")
		}
		janitorEnabled = b
	}
	janitorInterval, e := durationEnv("JANITOR_INTERVAL", 10*time.Minute)
	if e != nil {
		return Config{}, e
	}
	ooEnabled, e := boolEnv("ONLYOFFICE_ENABLED", false)
	if e != nil {
		return Config{}, e
	}
	ooRateLimit, e := intEnv("ONLYOFFICE_RATE_LIMIT_PER_MIN", 60)
	if e != nil {
		return Config{}, e
	}
	metricsEnabled, e := boolEnv("METRICS_ENABLED", true)
	if e != nil {
		return Config{}, e
	}
	drawioEnabled, e := boolEnv("DRAWIO_ENABLED", false)
	if e != nil {
		return Config{}, e
	}
	webpkgEnabled, e := boolEnv("WEBPKG_ENABLED", true)
	if e != nil {
		return Config{}, e
	}
	webpkgMaxEntries, e := intEnv("WEBPKG_MAX_ENTRIES", 500)
	if e != nil {
		return Config{}, e
	}
	webpkgMaxFileSize, e := int64Env("WEBPKG_MAX_FILE_SIZE", 32<<20)
	if e != nil {
		return Config{}, e
	}
	webpkgMaxTotalSize, e := int64Env("WEBPKG_MAX_TOTAL_SIZE", 256<<20)
	if e != nil {
		return Config{}, e
	}
	webpkgMaxDepth, e := intEnv("WEBPKG_MAX_DEPTH", 10)
	if e != nil {
		return Config{}, e
	}
	webpkgRateLimit, e := intEnv("WEBPKG_RATE_LIMIT_PER_MIN", 120)
	if e != nil {
		return Config{}, e
	}
	queueConcurrency, e := intEnv("QUEUE_CONCURRENCY", 5)
	if e != nil {
		return Config{}, e
	}
	patchMax, e := int64Env("PATCH_MAX_BYTES", 64<<20)
	if e != nil {
		return Config{}, e
	}
	smtpEnabled, e := boolEnv("SMTP_ENABLED", false)
	if e != nil {
		return Config{}, e
	}
	smtpPort, e := intEnv("SMTP_PORT", 587)
	if e != nil {
		return Config{}, e
	}
	oidcEnabled, e := boolEnv("OIDC_ENABLED", false)
	if e != nil {
		return Config{}, e
	}
	oidcAutoProvision, e := boolEnv("OIDC_AUTO_PROVISION", true)
	if e != nil {
		return Config{}, e
	}
	loginMaxRetries, e := intEnv("LOGIN_MAX_RETRIES", 5)
	if e != nil {
		return Config{}, e
	}
	loginLockMinutes, e := int64Env("LOGIN_LOCK_MINUTES", 15)
	if e != nil {
		return Config{}, e
	}
	csrfStrict, e := boolEnv("CSRF_STRICT", true)
	if e != nil {
		return Config{}, e
	}
	aiEnabled, e := boolEnv("AI_ENABLED", false)
	if e != nil {
		return Config{}, e
	}
	ragVector, e := boolEnv("AI_RAG_VECTOR_ENABLED", false)
	if e != nil {
		return Config{}, e
	}
	ragTopK, e := intEnv("AI_RAG_TOP_K", 8)
	if e != nil {
		return Config{}, e
	}
	ragChunk, e := intEnv("AI_RAG_CHUNK_SIZE", 1000)
	if e != nil {
		return Config{}, e
	}
	ragOverlap, e := intEnv("AI_RAG_CHUNK_OVERLAP", 100)
	if e != nil {
		return Config{}, e
	}
	c := Config{Port: stringEnv("PORT", "8080"), DatabaseURL: os.Getenv("DATABASE_URL"), JWTSecret: os.Getenv("JWT_SECRET"), AccessTokenTTL: a, RefreshTokenTTL: r, CookieSecure: secure, CookieDomain: os.Getenv("COOKIE_DOMAIN"), StorageRoot: stringEnv("STORAGE_ROOT", "./storage"), MaxFileSize: max, ScanEnabled: scan, UploadSessionTTL: ttl, TrustedProxies: listEnv("TRUSTED_PROXIES"), StorageDriver: stringEnv("STORAGE_DRIVER", "local"), S3Endpoint: os.Getenv("S3_ENDPOINT"), S3Bucket: os.Getenv("S3_BUCKET"), S3Region: stringEnv("S3_REGION", "us-east-1"), S3AccessKey: os.Getenv("S3_ACCESS_KEY"), S3SecretKey: os.Getenv("S3_SECRET_KEY"), S3PathStyle: pathStyle, ClamAVAddr: os.Getenv("CLAMAV_ADDR"), ClamAVTimeout: clamavTimeout, ClamAVRequired: clamavRequired, RateLimitPerMinute: rateLimit, LoginRateLimitPerMinute: loginRateLimit, PublicRateLimitPerMinute: publicRateLimit, MaxVersionsPerFile: maxVersions, JanitorEnabled: janitorEnabled, JanitorInterval: janitorInterval, OnlyOfficeEnabled: ooEnabled, OnlyOfficeServerURL: stringEnv("ONLYOFFICE_SERVER_URL", "http://onlyoffice:80"), OnlyOfficePublicURL: stringEnv("ONLYOFFICE_PUBLIC_URL", ""), OnlyOfficeJWTSecret: os.Getenv("ONLYOFFICE_JWT_SECRET"), OnlyOfficeDownloadURLBase: stringEnv("ONLYOFFICE_DOWNLOAD_URL_BASE", "http://backend:8080"), OnlyOfficeRateLimitPerMinute: ooRateLimit, MetricsEnabled: metricsEnabled, DrawioEnabled: drawioEnabled, DrawioServerURL: stringEnv("DRAWIO_SERVER_URL", "http://drawio:8080"), DrawioPublicURL: stringEnv("DRAWIO_PUBLIC_URL", ""), WebpkgEnabled: webpkgEnabled, WebpkgMaxEntries: webpkgMaxEntries, WebpkgMaxFileSize: webpkgMaxFileSize, WebpkgMaxTotalSize: webpkgMaxTotalSize, WebpkgMaxDepth: webpkgMaxDepth, WebpkgRateLimitPerMinute: webpkgRateLimit, QueueDriver: stringEnv("QUEUE_DRIVER", "inprocess"), RedisAddr: stringEnv("REDIS_ADDR", "localhost:6379"), RedisPassword: os.Getenv("REDIS_PASSWORD"), QueueConcurrency: queueConcurrency, PatchMaxBytes: patchMax, SMTPEnabled: smtpEnabled, SMTPHost: os.Getenv("SMTP_HOST"), SMTPPort: smtpPort, SMTPUser: os.Getenv("SMTP_USER"), SMTPPass: os.Getenv("SMTP_PASS"), SMTPFrom: os.Getenv("SMTP_FROM"), PublicBaseURL: stringEnv("PUBLIC_BASE_URL", ""), CaddyAdminAddr: stringEnv("CADDY_ADMIN_ADDR", ""), TLSCertDir: stringEnv("TLS_CERT_DIR", "/data/tls"), OIDCEnabled: oidcEnabled, OIDCIssuer: strings.TrimSuffix(stringEnv("OIDC_ISSUER", ""), "/"), OIDCClientID: os.Getenv("OIDC_CLIENT_ID"), OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"), OIDCRedirectURL: stringEnv("OIDC_REDIRECT_URL", ""), OIDCAutoProvision: oidcAutoProvision, AccessSalt: os.Getenv("ACCESS_SALT"), LoginMaxRetries: loginMaxRetries, LoginLockDuration: time.Duration(loginLockMinutes) * time.Minute, CSRFStrict: csrfStrict, AllowedOrigins: listEnv("ALLOWED_ORIGINS"), Environment: stringEnv("APP_ENV", "development"), BackupDir: stringEnv("BACKUP_DIR", ""), AIEnabled: aiEnabled, AIBaseURL: stringEnv("AI_BASE_URL", "https://api.openai.com/v1"), AIAPIKey: os.Getenv("AI_API_KEY"), AIModel: stringEnv("AI_MODEL", "gpt-4o-mini"), RAGMode: stringEnv("AI_RAG_MODE", "keyword"), RAGVectorEnabled: ragVector, RAGQdrantURL: stringEnv("AI_RAG_QDRANT_URL", "http://qdrant:6333"), RAGCollectionPrefix: stringEnv("AI_RAG_COLLECTION_PREFIX", "docflow_"), RAGEmbeddingProvider: stringEnv("AI_RAG_EMBEDDING_PROVIDER", "mock"), RAGEmbeddingModel: stringEnv("AI_RAG_EMBEDDING_MODEL", "text-embedding-3-small"), RAGTopK: ragTopK, RAGChunkSize: ragChunk, RAGChunkOverlap: ragOverlap, SearchDriver: stringEnv("SEARCH_DRIVER", "pg"), MeiliURL: stringEnv("MEILI_URL", ""), MeiliAPIKey: os.Getenv("MEILI_API_KEY"), ContentPublicBaseURL: stringEnv("CONTENT_PUBLIC_BASE_URL", ""), RawURLSecret: os.Getenv("RAW_URL_SECRET")}
	// OIDC 回调地址默认值：{PUBLIC_BASE_URL}/api/v1/auth/oidc/callback
	//（两者均未设置时由 validateOIDC 报错——IdP 侧必须注册确切回调地址）。
	if c.OIDCEnabled && c.OIDCRedirectURL == "" && c.PublicBaseURL != "" {
		c.OIDCRedirectURL = strings.TrimSuffix(c.PublicBaseURL, "/") + "/api/v1/auth/oidc/callback"
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return Config{}, errors.New("JWT_SECRET must be at least 32 bytes")
	}
	// 访问事件 IP 哈希盐：ACCESS_SALT 未设置时由 JWT secret 派生
	//（密钥类配置走环境变量，不额外入库）。
	if c.AccessSalt == "" {
		c.AccessSalt = "docflow-access:" + c.JWTSecret
	}
	if c.MaxVersionsPerFile < 1 {
		return Config{}, errors.New("MAX_VERSIONS_PER_FILE must be >= 1")
	}
	if c.LoginMaxRetries < 1 {
		return Config{}, errors.New("LOGIN_MAX_RETRIES must be >= 1")
	}
	if c.LoginLockDuration < time.Minute {
		return Config{}, errors.New("LOGIN_LOCK_MINUTES must be >= 1")
	}
	if c.StorageDriver != "local" && c.StorageDriver != "s3" {
		return Config{}, errors.New("STORAGE_DRIVER must be local or s3")
	}
	if c.PatchMaxBytes < 1 {
		return Config{}, errors.New("PATCH_MAX_BYTES must be >= 1")
	}
	if c.StorageDriver == "s3" && c.S3Bucket == "" {
		return Config{}, errors.New("S3_BUCKET is required when STORAGE_DRIVER=s3")
	}
	if e := validateOnlyOffice(c); e != nil {
		return Config{}, e
	}
	if e := validateDrawio(c); e != nil {
		return Config{}, e
	}
	if e := validateWebpkg(c); e != nil {
		return Config{}, e
	}
	if e := validateQueue(c); e != nil {
		return Config{}, e
	}
	if e := validateSMTP(c); e != nil {
		return Config{}, e
	}
	if e := validateOIDC(c); e != nil {
		return Config{}, e
	}
	if e := validateAI(c); e != nil {
		return Config{}, e
	}
	if e := validateRAG(c); e != nil {
		return Config{}, e
	}
	if e := validateSearch(c); e != nil {
		return Config{}, e
	}
	if e := validateRawURL(c); e != nil {
		return Config{}, e
	}
	return c, nil
}

// validateRawURL 校验受控原始内容配置：CONTENT_PUBLIC_BASE_URL 可选，
// 设置时须为绝对 http(s) URL；RAW_URL_SECRET 可选，设置时须 ≥32 字节
// （未设置由 contenturl 从 JWT_SECRET 派生，不新增必填项）。
func validateRawURL(c Config) error {
	if c.ContentPublicBaseURL != "" {
		if e := checkAbsoluteHTTPURL(c.ContentPublicBaseURL, "CONTENT_PUBLIC_BASE_URL"); e != nil {
			return e
		}
	}
	if c.RawURLSecret != "" && len(c.RawURLSecret) < 32 {
		return errors.New("RAW_URL_SECRET must be at least 32 bytes")
	}
	return nil
}

// validateAI 校验 AI 摘要配置：启用时 API_KEY 必填、BASE_URL（含默认值）
// 须为绝对 http(s) URL。
func validateAI(c Config) error {
	if !c.AIEnabled {
		return nil
	}
	if c.AIAPIKey == "" {
		return errors.New("AI_API_KEY is required when AI_ENABLED")
	}
	return checkAbsoluteHTTPURL(c.AIBaseURL, "AI_BASE_URL")
}

// validateSearch 校验全文检索引擎配置：驱动取值 pg|meili；meili 时
// MEILI_URL 必填且须为绝对 http(s) URL（启动时 EnsureIndex 校验连通性）。
func validateRAG(c Config) error {
	if c.RAGMode != "keyword" && c.RAGMode != "hybrid" {
		return errors.New("AI_RAG_MODE must be keyword or hybrid")
	}
	if c.RAGTopK < 1 || c.RAGTopK > 100 {
		return errors.New("AI_RAG_TOP_K must be 1-100")
	}
	if c.RAGChunkSize < 100 || c.RAGChunkOverlap < 0 || c.RAGChunkOverlap >= c.RAGChunkSize {
		return errors.New("invalid RAG chunk size/overlap")
	}
	if c.RAGVectorEnabled {
		if !c.AIEnabled {
			return errors.New("AI_RAG_VECTOR_ENABLED requires AI_ENABLED")
		}
		return checkAbsoluteHTTPURL(c.RAGQdrantURL, "AI_RAG_QDRANT_URL")
	}
	return nil
}

func validateSearch(c Config) error {
	if c.SearchDriver != "pg" && c.SearchDriver != "meili" {
		return errors.New("SEARCH_DRIVER must be pg or meili")
	}
	if c.SearchDriver == "meili" {
		if c.MeiliURL == "" {
			return errors.New("MEILI_URL is required when SEARCH_DRIVER=meili")
		}
		return checkAbsoluteHTTPURL(c.MeiliURL, "MEILI_URL")
	}
	return nil
}

// validateOIDC 校验 OIDC 单点登录配置：启用时 ISSUER 须为绝对 http(s) URL、
// CLIENT_ID/CLIENT_SECRET 必填；REDIRECT_URL 未显式设置且 PUBLIC_BASE_URL
// 为空时报错（默认值推导见 Load——IdP 侧必须注册确切回调地址）。
func validateOIDC(c Config) error {
	if !c.OIDCEnabled {
		return nil
	}
	if c.OIDCIssuer == "" {
		return errors.New("OIDC_ISSUER is required when OIDC_ENABLED")
	}
	if e := checkAbsoluteHTTPURL(c.OIDCIssuer, "OIDC_ISSUER"); e != nil {
		return e
	}
	if c.OIDCClientID == "" {
		return errors.New("OIDC_CLIENT_ID is required when OIDC_ENABLED")
	}
	if c.OIDCClientSecret == "" {
		return errors.New("OIDC_CLIENT_SECRET is required when OIDC_ENABLED")
	}
	if c.OIDCRedirectURL == "" {
		return errors.New("OIDC_REDIRECT_URL is required when OIDC_ENABLED and PUBLIC_BASE_URL is empty")
	}
	return checkAbsoluteHTTPURL(c.OIDCRedirectURL, "OIDC_REDIRECT_URL")
}

// validateSMTP 校验邮件通道配置：启用 SMTP 时 HOST 与 FROM 必填，
// PORT 为正，PUBLIC_BASE_URL 可选（为空时邮件链接退化为相对路径）。
func validateSMTP(c Config) error {
	if !c.SMTPEnabled {
		return nil
	}
	if c.SMTPHost == "" {
		return errors.New("SMTP_HOST is required when SMTP_ENABLED")
	}
	if c.SMTPPort < 1 || c.SMTPPort > 65535 {
		return errors.New("SMTP_PORT must be 1-65535")
	}
	if c.SMTPFrom == "" {
		return errors.New("SMTP_FROM is required when SMTP_ENABLED")
	}
	if c.PublicBaseURL != "" {
		if e := checkAbsoluteHTTPURL(c.PublicBaseURL, "PUBLIC_BASE_URL"); e != nil {
			return e
		}
	}
	return nil
}

// validateQueue 校验任务队列配置：驱动取值 inprocess|redis，并行数为正
// （仅 redis 驱动消费该值，统一校验避免静默回退）。
func validateQueue(c Config) error {
	if c.QueueDriver != "inprocess" && c.QueueDriver != "redis" {
		return errors.New("QUEUE_DRIVER must be inprocess or redis")
	}
	if c.QueueConcurrency < 1 {
		return errors.New("QUEUE_CONCURRENCY must be >= 1")
	}
	return nil
}

// validateWebpkg 校验网页包解包限制：条目数/深度/单文件大小为正，
// 且展开总大小不小于单文件上限。
func validateWebpkg(c Config) error {
	if c.WebpkgMaxEntries < 1 {
		return errors.New("WEBPKG_MAX_ENTRIES must be >= 1")
	}
	if c.WebpkgMaxDepth < 1 {
		return errors.New("WEBPKG_MAX_DEPTH must be >= 1")
	}
	if c.WebpkgMaxFileSize < 1 {
		return errors.New("WEBPKG_MAX_FILE_SIZE must be >= 1")
	}
	if c.WebpkgMaxTotalSize < c.WebpkgMaxFileSize {
		return errors.New("WEBPKG_MAX_TOTAL_SIZE must be >= WEBPKG_MAX_FILE_SIZE")
	}
	return nil
}

// validateDrawio 校验 draw.io 图表编辑集成配置：启用时 SERVER_URL 须为合法
// http(s) URL；PUBLIC_URL 可选（浏览器可达地址，留空回退 SERVER_URL），设置时
// 同样须为合法 http(s) URL。后端不与 drawio 服务通信，无凭据类配置。
func validateDrawio(c Config) error {
	if !c.DrawioEnabled {
		return nil
	}
	if e := checkAbsoluteHTTPURL(c.DrawioServerURL, "DRAWIO_SERVER_URL"); e != nil {
		return e
	}
	if c.DrawioPublicURL != "" {
		if e := checkAbsoluteHTTPURL(c.DrawioPublicURL, "DRAWIO_PUBLIC_URL"); e != nil {
			return e
		}
	}
	return nil
}

// validateOnlyOffice 校验 ONLYOFFICE 集成配置：启用时 SERVER_URL 须为合法
// http(s) URL，JWT_SECRET 必填且 ≥32 字节（与主 JWT_SECRET 相互独立）；
// PUBLIC_URL 可选（浏览器可达地址，留空回退 SERVER_URL），设置时同样
// 须为合法 http(s) URL。
func validateOnlyOffice(c Config) error {
	if !c.OnlyOfficeEnabled {
		return nil
	}
	if e := checkAbsoluteHTTPURL(c.OnlyOfficeServerURL, "ONLYOFFICE_SERVER_URL"); e != nil {
		return e
	}
	if c.OnlyOfficePublicURL != "" {
		if e := checkAbsoluteHTTPURL(c.OnlyOfficePublicURL, "ONLYOFFICE_PUBLIC_URL"); e != nil {
			return e
		}
	}
	if e := checkAbsoluteHTTPURL(c.OnlyOfficeDownloadURLBase, "ONLYOFFICE_DOWNLOAD_URL_BASE"); e != nil {
		return e
	}
	if len(c.OnlyOfficeJWTSecret) < 32 {
		return errors.New("ONLYOFFICE_JWT_SECRET must be at least 32 bytes when ONLYOFFICE_ENABLED")
	}
	return nil
}

func checkAbsoluteHTTPURL(raw, name string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New(name + " must be an absolute http(s) URL")
	}
	return nil
}

func stringEnv(k, f string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return f
}

// listEnv 读取逗号分隔的列表环境变量（TRUSTED_PROXIES 等）：
// 去除条目首尾空白并跳过空条目；未设置或全空返回 nil。
func listEnv(k string) []string {
	value := strings.TrimSpace(os.Getenv(k))
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// firstEnv 依次读取多个环境变量名，返回首个已设置（非空）的值；
// 全部未设置返回 ok=false。
func firstEnv(names ...string) (string, bool) {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v, true
		}
	}
	return "", false
}
func durationEnv(k string, f time.Duration) (time.Duration, error) {
	if v := os.Getenv(k); v != "" {
		return time.ParseDuration(v)
	}
	return f, nil
}
func boolEnv(k string, f bool) (bool, error) {
	if v := os.Getenv(k); v != "" {
		return strconv.ParseBool(v)
	}
	return f, nil
}
func intEnv(k string, f int) (int, error) {
	if v := os.Getenv(k); v != "" {
		return strconv.Atoi(v)
	}
	return f, nil
}
func int64Env(k string, f int64) (int64, error) {
	if v := os.Getenv(k); v != "" {
		n, e := strconv.ParseInt(v, 10, 64)
		return n, e
	}
	return f, nil
}
