package config

import (
	"errors"
	"net/url"
	"os"
	"strconv"
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
	// OnlyOfficeServerURL 为 DocumentServer 基地址（如 http://onlyoffice:80），
	// 同时是回调下载 URL 防 SSRF 校验的同源基准；启用时必填。
	OnlyOfficeServerURL string
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
	QueueDriver string
	// RedisAddr 为 Redis 地址 host:port（默认 localhost:6379，
	// QUEUE_DRIVER=redis 时使用）。
	RedisAddr string
	// RedisPassword 为 Redis 密码（可选，REDIS_PASSWORD）。
	RedisPassword string
	// QueueConcurrency 为 redis 驱动下每实例并行处理任务数
	//（QUEUE_CONCURRENCY，默认 5）。
	QueueConcurrency int
}

func Load() (Config, error) {
	a, e := durationEnv("ACCESS_TOKEN_TTL", 15*time.Minute)
	if e != nil {
		return Config{}, e
	}
	r, e := durationEnv("REFRESH_TOKEN_TTL", 30*24*time.Hour)
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
	max, e := int64Env("MAX_FILE_SIZE", 1<<30)
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
	c := Config{Port: stringEnv("PORT", "8080"), DatabaseURL: os.Getenv("DATABASE_URL"), JWTSecret: os.Getenv("JWT_SECRET"), AccessTokenTTL: a, RefreshTokenTTL: r, CookieSecure: secure, CookieDomain: os.Getenv("COOKIE_DOMAIN"), StorageRoot: stringEnv("STORAGE_ROOT", "./storage"), MaxFileSize: max, ScanEnabled: scan, UploadSessionTTL: ttl, StorageDriver: stringEnv("STORAGE_DRIVER", "local"), S3Endpoint: os.Getenv("S3_ENDPOINT"), S3Bucket: os.Getenv("S3_BUCKET"), S3Region: stringEnv("S3_REGION", "us-east-1"), S3AccessKey: os.Getenv("S3_ACCESS_KEY"), S3SecretKey: os.Getenv("S3_SECRET_KEY"), S3PathStyle: pathStyle, ClamAVAddr: os.Getenv("CLAMAV_ADDR"), ClamAVTimeout: clamavTimeout, ClamAVRequired: clamavRequired, RateLimitPerMinute: rateLimit, LoginRateLimitPerMinute: loginRateLimit, PublicRateLimitPerMinute: publicRateLimit, MaxVersionsPerFile: maxVersions, JanitorEnabled: janitorEnabled, JanitorInterval: janitorInterval, OnlyOfficeEnabled: ooEnabled, OnlyOfficeServerURL: stringEnv("ONLYOFFICE_SERVER_URL", "http://onlyoffice:80"), OnlyOfficeJWTSecret: os.Getenv("ONLYOFFICE_JWT_SECRET"), OnlyOfficeDownloadURLBase: stringEnv("ONLYOFFICE_DOWNLOAD_URL_BASE", "http://backend:8080"), OnlyOfficeRateLimitPerMinute: ooRateLimit, MetricsEnabled: metricsEnabled, WebpkgEnabled: webpkgEnabled, WebpkgMaxEntries: webpkgMaxEntries, WebpkgMaxFileSize: webpkgMaxFileSize, WebpkgMaxTotalSize: webpkgMaxTotalSize, WebpkgMaxDepth: webpkgMaxDepth, WebpkgRateLimitPerMinute: webpkgRateLimit, QueueDriver: stringEnv("QUEUE_DRIVER", "inprocess"), RedisAddr: stringEnv("REDIS_ADDR", "localhost:6379"), RedisPassword: os.Getenv("REDIS_PASSWORD"), QueueConcurrency: queueConcurrency}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return Config{}, errors.New("JWT_SECRET must be at least 32 bytes")
	}
	if c.MaxVersionsPerFile < 1 {
		return Config{}, errors.New("MAX_VERSIONS_PER_FILE must be >= 1")
	}
	if c.StorageDriver != "local" && c.StorageDriver != "s3" {
		return Config{}, errors.New("STORAGE_DRIVER must be local or s3")
	}
	if c.StorageDriver == "s3" && c.S3Bucket == "" {
		return Config{}, errors.New("S3_BUCKET is required when STORAGE_DRIVER=s3")
	}
	if e := validateOnlyOffice(c); e != nil {
		return Config{}, e
	}
	if e := validateWebpkg(c); e != nil {
		return Config{}, e
	}
	if e := validateQueue(c); e != nil {
		return Config{}, e
	}
	return c, nil
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

// validateOnlyOffice 校验 ONLYOFFICE 集成配置：启用时 SERVER_URL 须为合法
// http(s) URL，JWT_SECRET 必填且 ≥32 字节（与主 JWT_SECRET 相互独立）。
func validateOnlyOffice(c Config) error {
	if !c.OnlyOfficeEnabled {
		return nil
	}
	if e := checkAbsoluteHTTPURL(c.OnlyOfficeServerURL, "ONLYOFFICE_SERVER_URL"); e != nil {
		return e
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
