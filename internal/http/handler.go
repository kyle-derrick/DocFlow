package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/caddytls"
	"github.com/docflow/docflow/internal/collab"
	"github.com/docflow/docflow/internal/contenturl"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/group"
	"github.com/docflow/docflow/internal/invite"
	"github.com/docflow/docflow/internal/mail"
	"github.com/docflow/docflow/internal/mcp"
	"github.com/docflow/docflow/internal/metrics"
	"github.com/docflow/docflow/internal/notify"
	"github.com/docflow/docflow/internal/oidc"
	"github.com/docflow/docflow/internal/onlyoffice"
	"github.com/docflow/docflow/internal/realtime"
	"github.com/docflow/docflow/internal/settings"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/space"
	"github.com/docflow/docflow/internal/tagging"
	"github.com/docflow/docflow/internal/tasks"
	"github.com/docflow/docflow/internal/upload"
	"github.com/docflow/docflow/internal/webhook"
	"github.com/docflow/docflow/internal/webpkg"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// userDirectory 抽象用户目录与账号管理查询（生产实现为 *auth.UserStore），
// login 走 FindActiveByIdentifier（email 或 username，C21a），用户查找/邀请走
// Lookup，分享者名走 Username，refresh 轮换成功后经 Status 复查账号是否仍
// active（自动锁定派生为 locked，C9）；/me 与管理端用户管理走其余方法。
type userDirectory interface {
	FindActiveByIdentifier(identifier string) (auth.User, error)
	Lookup(q string, limit int) ([]auth.User, error)
	// Search 为成员/ACL 主体选择器的用户检索（username/nickname 子串）。
	Search(q string, limit int) ([]auth.User, error)
	Username(id uuid.UUID) (string, error)
	Status(id uuid.UUID) (string, error)
	// GetByID 完整用户记录（/me、管理端；不存在返回 auth.ErrUserNotFound）。
	GetByID(id uuid.UUID) (auth.User, error)
	// RecordLoginFailure / ClearLoginFailures 为 C9 登录失败锁定计数。
	RecordLoginFailure(id uuid.UUID, maxRetries int, lockFor time.Duration) error
	ClearLoginFailures(id uuid.UUID) error
	// UpdateProfile 为 C21a 档案更新（校验由调用方先行）。
	UpdateProfile(id uuid.UUID, update auth.ProfileUpdate) error
	// AdminListUsers / AdminUpdateUser 为 C6 管理端用户管理。
	// AdminListUsers returns a paginated list of users and the total count.
	AdminListUsers(q string, limit, offset int) ([]auth.User, int64, error)
	// AdminUpdateUser applies administrative changes to a user account.
	AdminUpdateUser(id uuid.UUID, update auth.AdminUserUpdate) error
}

var _ userDirectory = (*auth.UserStore)(nil)

// emailChangeAccount 为换绑邮箱链路的最小账号读写源（生产实现
// *auth.UserStore；独立窄接口避免扩张 userDirectory 造成测试替身连锁改动）。
type emailChangeAccount interface {
	GetByID(id uuid.UUID) (auth.User, error)
	UpdateEmail(id uuid.UUID, email string) error
	EmailExists(email string) (bool, error)
}

var _ emailChangeAccount = (*auth.UserStore)(nil)

// SetEmailChange 注入换绑邮箱链路依赖（验证码存储 + 账号读写源；幂等）。
func (h *Handler) SetEmailChange(codes auth.EmailChangeStore, account emailChangeAccount) {
	if codes != nil {
		h.emailChangeCodes = codes
	}
	if account != nil {
		h.emailChangeAccount = account
	}
}

// dummyPasswordHash 为包初始化时一次性生成的 bcrypt 哈希（与 HashPassword
// 同 cost）。用户不存在/非 active 分支对它执行同样的 CompareHashAndPassword，
// 使响应耗时与「用户存在但密码错误」路径一致，防止通过时间差枚举邮箱。
var dummyPasswordHash = func() string {
	hash, err := bcrypt.GenerateFromPassword([]byte("docflow-dummy-password-timing-align"), bcrypt.DefaultCost+2)
	if err != nil {
		panic("auth: generate dummy password hash: " + err.Error())
	}
	return string(hash)
}()

// authOpsRateLimitPerMin 为 refresh/logout 端点的独立按 IP 轻限流
// （每分钟次数；防无认证刷轮换/登出，复用 publicLimiter 模式的独立实例）。
const authOpsRateLimitPerMin = 60

// authSensitiveRateLimitPerMin 为注册/忘记密码/重置密码三个公开端点的独立
// 按 IP 限流（每分钟 10 次，防无认证刷注册与重置邮件，复用 publicLimiter 模式
// 的独立实例，比公开分享接口更严）。
const authSensitiveRateLimitPerMin = 10

// shareVerifyRateLimitPerMin 为公开分享密码校验端点（POST /public/shares/:token/verify）
// 的独立按 IP+token 限流（每分钟 5 次，防无认证暴力猜测分享密码）。
const shareVerifyRateLimitPerMin = 5

type ReadinessChecker interface {
	Check(context.Context) (map[string]string, bool)
}

type Handler struct {
	readiness ReadinessChecker
	auth      *auth.Service
	users     userDirectory
	files     *files.Store
	shares    *share.Service
	spaces    *space.Service
	// groups 为管理端用户组服务（migration 035，SetGroups 注入）：组 CRUD
	// 与成员管理（仅 admin 路由组）；未注入时组端点返回 503（生产恒注入）。
	groups          *group.Service
	uploads         *upload.Service
	storage         upload.Storage
	cookieSecure    bool
	cookieDomain    string
	refreshTokenTTL time.Duration
	audit           audit.Recorder
	// 管理端依赖（admin 组）：系统设置、统计与角色查询源。
	settings   settingsService
	stats      statsSource
	auditQuery auditQuerySource
	roles      auth.RoleLookup
	// statsDB 为管理端空间列表的直查 DB（SetStatsSource 一并注入；仅 admin
	// 路由组消费，缺省时 /admin/spaces 返回 503）。
	statsDB *gorm.DB
	// caddyTLS 为 HTTPS 运行时切换服务（SetCaddyTLS 注入）；nil 时
	// GET /admin/tls 返回 managed=false，PUT 返回 503。
	caddyTLS *caddytls.Service
	// quarantine 为隔离区管理服务（SetQuarantineService 注入）；nil 时
	// 隔离区端点 503（生产恒注入）。
	quarantine quarantineService
	// dashboard 为个人仪表盘聚合源（SetDashboardSource 注入）；nil 时
	// GET /api/v1/dashboard 返回 500（生产恒注入）。
	dashboard dashboardSource
	// invites 为邀请制注册服务、mailer 为邮件通道（邀请/重置链接），
	// publicBaseURL 用于拼接邮件里的绝对链接；SetInvites 注入，未注入时
	// 邀请与注册/重置端点返回 503。
	invites       *invite.Service
	mailer        mail.Mailer
	publicBaseURL string
	// onlyoffice 为 ONLYOFFICE 集成服务；非 nil（SetOnlyOffice 注入）时
	// Register 挂载 /api/v1/onlyoffice 的 session/download/callback 路由，
	// 否则不注册（默认 404）；config 探测端点恒注册（禁用时 enabled=false）。
	onlyoffice                *onlyoffice.Service
	onlyofficeRateLimitPerMin int
	// drawioEnabled / drawioURL 为 draw.io 图表编辑集成配置（SetDrawio 注入）：
	// 编辑器为浏览器侧 iframe embed（postMessage JSON 协议），后端不与 drawio
	// 服务通信，保存走通用「上传 file_id 覆盖新版本」链路——仅 config 探测
	// 端点需要这两个值，且恒注册（禁用时 enabled=false、url=null）。
	drawioEnabled bool
	drawioURL     string
	// webpkg 为网页包安全预览服务；非 nil（SetWebpkg 注入）时 Register 挂载
	// 内容端点 /content/:pid/*filepath（无认证、独立按 IP 轻限流）与手动
	// 解包 POST /api/v1/files/:id/webpkg/extract，否则不注册（默认 404）。
	webpkg                *webpkg.Service
	webpkgRateLimitPerMin int
	// metricsDisabled 由 SetMetricsEnabled(false) 设置：true 时不注册
	// GET /metrics 端点（默认启用）。HTTP 指标中间件不受此开关影响。
	metricsDisabled bool
	// tasks 为后台任务队列入队器（SetTaskEnqueuer 注入）：tus PATCH 写满后
	// 的后台补完经其派发（inprocess 与原内联 goroutine 行为一致；redis 时
	// 由任意实例 worker 处理）。nil 时回退进程内 goroutine（tusAutocomplete）。
	// 重建索引端点（POST /admin/settings/ai/reindex）复用同一入队器。
	tasks tasks.Enqueuer
	// reindex 为重建索引端点的文件列表源（SetReindexLister 注入）：files
	// 表游标分页直查的生产实现；未注入时该端点返回 503（生产恒注入）。
	reindex reindexLister
	// tags 为标签服务（SetTagging 注入）：标签 CRUD 与文件打/去标签；
	// 未注入时 tags 端点返回 503（生产恒注入，契约测试注入内存实现）。
	tags *tagging.Service
	// acl 为路径级 ACL 管理服务（SetACL 注入）：GET/PUT /folders/:id/acl；
	// 未注入时端点返回 503（生产恒注入，契约测试注入内存实现）。
	acl aclService
	// search 为全文检索服务（SetSearch 注入）：GET /api/v1/search 的
	// 名称 + 内容检索；未注入时该端点返回 503（生产恒注入）。
	search searchService
	// notifications 为站内通知服务（SetNotifications 注入）：通知列表/已读
	// 与通知偏好；未注入时通知端点返回 503（生产恒注入）。
	notifications  *notify.Service
	realtime       *realtime.Hub
	allowedOrigins []string
	// collab 为富文本实时协作房间管理器（SetCollabManager 注入）；collabFiles
	// 为加入房间的写权限判定源（ValidateReplaceTarget 写权限链）。未注入时
	// /api/v1/collab/:fileId/ws 返回 503（生产恒注入）。
	collab      *collab.Manager
	collabFiles collabFileAuthorizer
	// emailChangeAccount 为换绑邮箱链路的账号读写源（SetEmailChange 注入，
	// 生产为 *auth.UserStore）；emailChangeCodes 为验证码存储。任一未注入
	// 时换绑邮箱端点返回 503（契约测试不注入）。
	emailChangeAccount emailChangeAccount
	emailChangeCodes   auth.EmailChangeStore
	environment        string
	wsSecret           string
	// webhooks 为 Webhook 通知渠道服务（SetWebhooks 注入）：注册/列举/
	// 启停/删除（本人维度）；未注入时 webhook 端点返回 503（生产恒注入）。
	webhooks *webhook.Service
	// idem 为批量端点的幂等响应缓存（进程内，TTL 60s；见 batch.go）。
	idem *idemCache
	// oidc 为 OIDC 单点登录服务；非 nil（SetOIDC 注入）时 Register 挂载
	// /api/v1/auth/oidc/login 与 callback 路由（公开组），否则不注册
	//（默认 404）；config 探测端点恒注册（禁用时 enabled=false）。
	oidc *oidc.Service
	// versionReader 为版本内容读取源（NewHandler 以 *files.Store 装配，
	// 接口化便于单测注入内存实现）：GET /files/:id/versions/:versionId/content。
	versionReader versionContentReader
	// accessSalt 为公开访问事件 IP 哈希的静态盐（ACCESS_SALT，缺省由
	// JWT secret 派生，见 config.Load / SetAccessSalt）：明文 IP 不落库。
	accessSalt string
	// usage 为个人空间存储占用查询源（C3 配额；NewHandler 以 *files.Store
	// 装配，接口化便于单测注入内存实现）：GET/PATCH /me 的 storage.used。
	usage storageUsage
	// ai 为 AI 摘要客户端（SetAI 注入）：POST /files/:id/ai/summary；
	// 未注入或 AI_ENABLED=false 时端点 503 AI_DISABLED。
	ai aiSummarizer
	// aiFiles 为 AI 摘要所需的文件读取源（NewHandler 以 *files.Store 装配，
	// 接口化便于单测注入内存实现）。
	aiFiles aiFileSource
	// aiSvc 为 AI 能力第一版服务（SetAIService 注入）：POST /ai/chat（SSE）、
	// POST /ai/summarize 与 MCP 工具共用；未注入时相关端点 503。
	aiSvc *ai.Service
	// aiEnv 为 AI env 基线（AI_* 合成的 env Provider），SetAIService 注入。
	aiEnv settings.AIConfig
	// aiUsageStore 为 AI 用量聚合源（SetAIService 注入）；GET /admin/ai/usage。
	aiUsageStore *ai.UsageStore
	// aiLimiter 为 AI 端点的每用户限流器（热读取 Provider 级
	// requests_per_min/daily_quota，全局 ai.per_user_per_min 兜底）。
	aiLimiter *dynamicRateLimiter
	// csrfStrict 控制 refresh/logout 同源严格校验（C10，CSRF_STRICT 默认
	// true）：true 时缺失 Origin/Referer 一律 403，见 csrf.go。
	csrfStrict bool
	// resolver 为路径型访问（/resolve 与 /raw/auth）的最小依赖（NewHandler
	// 以 *files.Store 装配，接口化便于单测注入内存实现，模式同 versionReader）。
	resolver resolveAPI
	// unpacker 为 zip 解包导入的最小文件依赖（NewHandler 以 *files.Store
	// 装配，接口化便于单测注入内存实现）。
	unpacker unpackAPI
	// contentSigner 为 /raw/* 短期授权（HMAC grant）签发器（SetContentSigner
	// 注入）；nil 时 resolve 端点 503、raw 端点 404（生产恒注入）。
	contentSigner *contenturl.Signer
	// contentBaseURL 为受控原始内容的对外基地址（CONTENT_PUBLIC_BASE_URL，
	// 可选）：resolve 以 origin_content=1 请求时拼接绝对 raw_url。
	contentBaseURL string
	// loginMaxRetries / loginLockDuration 为 C9 连续登录失败锁定策略的
	// env 基线（LOGIN_MAX_RETRIES 默认 5 / LOGIN_LOCK_MINUTES 默认 15m；
	// SetLoginLockout 启动时注入）。运行时生效值经 loginLockoutPolicy
	// 热读取（settings 的 security.login_* 键优先，键未入库回落本基线）。
	loginMaxRetries   int
	loginLockDuration time.Duration
	backupDir         string
	// mcpDeps 为 MCP 端点（POST /mcp）的服务依赖：NewHandler 以 files/
	// upload/share/space/storage 装配，search 于 Register 时并入（SetSearch
	// 后注册）；测试可直接改写注入内存实现（模式同 resolver/unpacker）。
	mcpDeps *mcp.Deps
	// mcpServer 为 Register 时构建的 MCP 服务端实例（mcpPost 消费）。
	mcpServer *mcp.Server
	// zipper 为目录打包下载（download.zip）的最小文件依赖（NewHandler 以
	// *files.Store 装配，接口化便于单测注入内存实现，模式同 unpacker）。
	zipper zipDownloadAPI
	// openWith 为「默认打开方式」偏好存取（NewHandler 以 *auth.UserStore
	// 装配，接口化便于单测注入内存实现）；未注入时端点返回 503。
	openWith openWithStore
	// aiPrefs 为用户个人 AI 配置存取（user_ai_prefs，双轨制；NewHandler 以
	// *auth.UserStore 装配，接口化便于单测注入内存实现）；未注入时个人
	// 配置端点 503、AI 解析链仅平台池。
	aiPrefs aiPrefsStore
	// aiMemory 为用户 AI 记忆存取（ai_memory 表，migration 050，本人维度；
	// NewHandler 以 *auth.UserStore 装配，接口化便于单测注入内存实现）；
	// 未注入时 /ai/memory 端点 503、chat 的 include_memory 静默跳过。
	aiMemory      aiMemoryStore
	webdav        *davHandler
	webdavTokens  *auth.WebDAVStore
	commentsDB    *gorm.DB
	agentDB       *gorm.DB
	agentSem      chan struct{}
	agentSemMu    sync.Mutex
	agentCancelMu sync.Mutex
	agentCancels  map[uuid.UUID]context.CancelFunc
	// agentAI 为 Agent 容器 IPC 调平台 AI 的网关（SetAgentAI 注入，main
	// 装配并启动 unix socket server）；nil 时任务不签发 AI 令牌（容器
	// 内无 AI 能力，agent-runner 降级为仅执行 prompt 中的 ```run 块）。
	agentAI *AgentAIGateway
}

func NewHandler(authService *auth.Service, users *auth.UserStore, fileStore *files.Store, shares *share.Service, spaces *space.Service, uploads *upload.Service, storage upload.Storage, cookieSecure bool, cookieDomain string, refreshTokenTTL time.Duration) *Handler {
	h := &Handler{auth: authService, users: users, files: fileStore, shares: shares, spaces: spaces, uploads: uploads, storage: storage, cookieSecure: cookieSecure, cookieDomain: cookieDomain, refreshTokenTTL: refreshTokenTTL, audit: audit.NopRecorder{}, mailer: mail.NewNoopMailer(), idem: newIdemCache(idempotencyTTL), versionReader: fileStore, usage: fileStore, aiFiles: fileStore, resolver: fileStore, unpacker: fileStore, csrfStrict: true, loginMaxRetries: 5, loginLockDuration: 15 * time.Minute, mcpDeps: &mcp.Deps{Files: fileStore, Uploads: uploads, Storage: storage, Shares: shares, Spaces: spaces}}
	if fileStore != nil {
		h.zipper = fileStore
	}
	if users != nil {
		h.openWith = users
		h.aiPrefs = users
		h.aiMemory = users
	}
	return h
}

// SetCSRFStrict 控制 refresh/logout 的同源严格校验（CSRF_STRICT，幂等；
// 默认 true）。非浏览器客户端（curl）无法携带 Origin 时需显式置 false。
func (h *Handler) SetCSRFStrict(strict bool) { h.csrfStrict = strict }
func (h *Handler) webdavEnabled() bool {
	if h.settings == nil {
		return false
	}
	v, err := h.settings.GetAll()
	if err != nil {
		return false
	}
	for _, s := range v {
		if s.Key == "webdav.enabled" {
			b, ok := s.Value.(bool)
			return ok && b
		}
	}
	return false
}

func (h *Handler) SetWebDAV(store *auth.WebDAVStore) {
	if store != nil {
		h.webdavTokens = store
		h.webdav = &davHandler{h: h, tokens: store, base: "/webdav", enabledFn: func() bool { return h.webdavEnabled() }}
	}
}

// SetLoginLockout 注入 C9 登录失败锁定策略的 env 基线（LOGIN_MAX_RETRIES /
// LOGIN_LOCK_MINUTES；maxRetries<1 或 lockFor<=0 时忽略，保持默认）。
// 运行时可被 system_settings 的 security.login_max_retries /
// security.login_lock_minutes 覆盖（见 loginLockoutPolicy，env 为引导默认）。
func (h *Handler) SetLoginLockout(maxRetries int, lockFor time.Duration) {
	if maxRetries >= 1 && lockFor > 0 {
		h.loginMaxRetries = maxRetries
		h.loginLockDuration = lockFor
	}
}

// loginLockoutPolicy 返回当前生效的防爆破锁定策略（阈值 + 时长）：settings
// 热读取优先（security.login_max_retries / security.login_lock_minutes，
// 每次调用直读 system_settings，管理端变更即时生效；键未入库或值非法
// 回落 env 基线）。登录失败计数与 WebDAV Basic 防爆破共用本策略；只读
// 共享字段（settings 与 env 基线启动装配后不变），并发安全。
func (h *Handler) loginLockoutPolicy() (int, time.Duration) {
	maxRetries, lockFor := h.loginMaxRetries, h.loginLockDuration
	if h.settings != nil {
		if n, ok := h.settings.GetIntDefined(settings.KeyLoginMaxRetries); ok && n >= 1 {
			maxRetries = n
		}
		if m, ok := h.settings.GetIntDefined(settings.KeyLoginLockMinutes); ok && m >= 1 {
			lockFor = time.Duration(m) * time.Minute
		}
	}
	return maxRetries, lockFor
}

// SetAuditRecorder 注入审计写入器；nil 时保持 Nop。
func (h *Handler) SetAuditRecorder(recorder audit.Recorder) {
	if recorder != nil {
		h.audit = recorder
	}
}

// SetMetricsEnabled 控制 GET /metrics 端点（METRICS_ENABLED，默认 true）。
// 端点无认证：生产环境应由反向代理（Caddy）或网络层限制访问。
func (h *Handler) SetMetricsEnabled(enabled bool) { h.metricsDisabled = !enabled }

func (h *Handler) SetReadinessChecker(checker ReadinessChecker) { h.readiness = checker }
func (h *Handler) SetRealtimeHub(hub *realtime.Hub, origins []string, environment string) {
	h.realtime = hub
	h.allowedOrigins = origins
	h.environment = environment
}
func (h *Handler) SetWSSecret(secret string) { h.wsSecret = secret }
func (h *Handler) SetBackupDir(dir string)   { h.backupDir = strings.TrimSpace(dir) }

// SetAgentAI 注入 Agent 容器平台 AI IPC 网关（幂等；nil 保持未装配）。
// 网关的 unix socket server 由 main 经 StartAgentIPCServer 启动。
func (h *Handler) SetAgentAI(gw *AgentAIGateway) {
	if gw != nil {
		h.agentAI = gw
	}
}

// SetAccessSalt 注入公开访问事件 IP 哈希的静态盐（ACCESS_SALT；缺省由
// config 从 JWT secret 派生）。空值时回退固定占位盐（仅测试场景）。
func (h *Handler) SetAccessSalt(salt string) {
	if salt != "" {
		h.accessSalt = salt
	}
}

// SetWebpkg 注入网页包安全预览服务（幂等）；rateLimitPerMin 为 /content
// 内容端点的独立按 IP 轻限流（WEBPKG_RATE_LIMIT_PER_MIN，默认 120）。
func (h *Handler) SetWebpkg(svc *webpkg.Service, rateLimitPerMin int) {
	if svc != nil {
		h.webpkg = svc
		h.webpkgRateLimitPerMin = rateLimitPerMin
	}
}

// SetTaskEnqueuer 注入后台任务队列入队器（幂等）；nil 时保持回退路径
// （tus 自动完成走进程内 goroutine，行为与既有版本一致）。
func (h *Handler) SetTaskEnqueuer(enqueuer tasks.Enqueuer) {
	if enqueuer != nil {
		h.tasks = enqueuer
	}
}

// SetTagging 注入标签服务（幂等）；repo 通常为 tagging.NewGormRepo(db)，
// fileSource 为 *files.Store（文件读授权）。未注入时 tags 端点 503。
func (h *Handler) SetTagging(svc *tagging.Service) {
	if svc != nil {
		h.tags = svc
	}
}

// SetSearch 注入全文检索服务（幂等）；svc 通常为 search.NewStore(
// search.NewGormRepo(db))。未注入时 GET /api/v1/search 返回 503
// （生产恒注入，契约测试注入内存实现）。
func (h *Handler) SetSearch(svc searchService) {
	if svc != nil {
		h.search = svc
	}
}

// SetAI 注入 AI 摘要客户端（幂等）；svc 通常为 ai.NewClient(...)。
// 未注入或 AI_ENABLED=false 时 POST /files/:id/ai/summary 返回 503
// AI_DISABLED。
func (h *Handler) SetAI(svc aiSummarizer) {
	if svc != nil {
		h.ai = svc
	}
}

// SetAIService 注入 AI 能力第一版服务（幂等）：svc 为多 Provider ChatService
// （POST /ai/chat SSE / POST /ai/summarize / 管理端设置与连接测试）；
// envBase 为 AI_* env 合成的基线 Provider；usageStore 可为 nil（跳过统计）。
func (h *Handler) SetAIService(svc *ai.Service, envBase settings.AIConfig, usageStore *ai.UsageStore) {
	if svc == nil {
		return
	}
	h.aiSvc = svc
	h.aiEnv = envBase
	h.aiUsageStore = usageStore
	if h.aiLimiter == nil {
		h.aiLimiter = newDynamicRateLimiter()
	}
}

// SetInvites 注入邀请制注册服务与邮件通道（幂等）；publicBaseURL 用于拼接
// 邀请/重置邮件中的绝对链接（空则输出相对路径）。mailer 为 nil 时回退
// Noop（仅日志输出链接）。未注入 invites 时邀请管理与注册/重置端点 503。
func (h *Handler) SetInvites(svc *invite.Service, mailer mail.Mailer, publicBaseURL string) {
	if svc == nil {
		return
	}
	h.invites = svc
	if mailer == nil {
		mailer = mail.NewNoopMailer()
	}
	h.mailer = mailer
	h.publicBaseURL = publicBaseURL
}

// publicLink 拼接邮件/一次性响应里的链接：配置了 PUBLIC_BASE_URL 时返回
// 绝对地址，否则返回相对路径（由日志型邮件通道原样输出）。
func (h *Handler) publicLink(path string) string {
	if h.publicBaseURL == "" {
		return path
	}
	return strings.TrimSuffix(h.publicBaseURL, "/") + path
}

func (h *Handler) Register(r *gin.Engine, jwtSecret string, rateLimit, loginRateLimit, publicRateLimit int) {
	if h.webdav != nil {
		r.Any("/webdav", gin.WrapH(h.webdav))
		r.Any("/webdav/*path", gin.WrapH(h.webdav))
	}
	// Prometheus HTTP 指标中间件：全局挂载（须先于任何路由注册），
	// route 标签取 gin 路由模板，未匹配路由（404）归一为 unknown。
	r.Use(metrics.GinMiddleware())
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/ready", func(c *gin.Context) {
		if h.readiness == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "checks": gin.H{"readiness": "not_configured"}})
			return
		}
		checks, ready := h.readiness.Check(c.Request.Context())
		status := "ready"
		code := http.StatusOK
		if !ready {
			status = "not_ready"
			code = http.StatusServiceUnavailable
		}
		c.JSON(code, gin.H{"status": status, "checks": checks})
	})
	// Prometheus 指标端点：挂根路由（不在 /api/v1 下），无认证。
	// 生产环境务必由 Caddy/反向代理或网络层限制访问；契约测试按基础设施
	// 路由豁免（openapi_test.go isExcludedRoute），不入 docs/openapi.yaml。
	if !h.metricsDisabled {
		r.GET("/metrics", gin.WrapH(metrics.Handler()))
	}
	loginLimiterMW := loginLimiter(NewRateLimiter(loginRateLimit))
	// refresh/logout：无认证的会话操作端点，独立实例按 IP 轻限流
	//（与 login 限流互不挤占；publicLimiter 模式复用）。
	authOpsLimiterMW := publicLimiter(NewRateLimiter(authOpsRateLimitPerMin))
	// 注册/忘记密码/重置密码：无认证的敏感公开端点，共享独立实例按 IP
	// 更严限流（10/min），防刷注册与重置邮件。
	authSensitiveLimiterMW := publicLimiter(NewRateLimiter(authSensitiveRateLimitPerMin))
	authGroup := r.Group("/api/v1/auth")
	authGroup.POST("/login", loginLimiterMW, h.login)
	// 两步验证登录第二段（公开）：与 /login 共享同一限流器实例与限流键
	//（IP+邮箱前缀哈希，重放同样的密码+邮箱消耗同一桶）。
	authGroup.POST("/login/totp", loginLimiterMW, h.loginTOTP)
	// refresh/logout：cookie 认证端点，先过同源（CSRF）校验再进按 IP 轻限流
	//（C10，设计 6.1.5；严格模式要求 Origin/Referer 存在且 host 一致，
	// CSRF_STRICT=false 时放行无两头请求供非浏览器客户端使用）。
	csrfMW := applyCSRF(h.csrfStrict)
	authGroup.POST("/refresh", csrfMW, authOpsLimiterMW, h.refresh)
	authGroup.POST("/logout", csrfMW, authOpsLimiterMW, h.logout)
	// 邀请制注册（凭一次性邀请 token）与密码找回/重置：公开端点。
	authGroup.POST("/register", authSensitiveLimiterMW, h.register)
	authGroup.POST("/forgot-password", authSensitiveLimiterMW, h.forgotPassword)
	authGroup.POST("/reset-password", authSensitiveLimiterMW, h.resetPassword)
	// OIDC 单点登录（v2）：config 探测端点恒注册（禁用时 enabled=false），
	// login/callback 仅启用（SetOIDC 注入）时注册；均为公开 GET，复用
	// refresh/logout 的按 IP 轻限流实例。
	authGroup.GET("/oidc/config", h.oidcConfig)
	if h.oidc != nil {
		authGroup.GET("/oidc/login", authOpsLimiterMW, h.oidcLogin)
		authGroup.GET("/oidc/callback", authOpsLimiterMW, h.oidcCallback)
	}
	// PAT 认证路径挂在中间件上（dfpat_ 前缀走 Service.VerifyPersonalAccessToken）；
	// h.auth 未注入（契约测试）时显式传 nil，中间件对该前缀一律 401。
	var patVerifier auth.AccessTokenVerifier
	if h.auth != nil {
		patVerifier = h.auth
	}
	api := r.Group("/api/v1", auth.RequireAccessToken(jwtSecret, patVerifier), apiLimiter(NewRateLimiter(rateLimit)), applyCSRF(h.csrfStrict))
	// 个人档案与配额（C3/C21a）：GET /me 读档案+用量，PATCH /me 改档案
	//（nickname/department/position/phone/bio/language/timezone）。
	api.GET("/me", h.me)
	api.PATCH("/me", h.updateMe)
	// 换绑邮箱（账号安全，v2.4）：验证密码后向新邮箱投递 6 位验证码，
	// 二次提交确认完成换绑（见 me_email.go）。
	api.POST("/me/email/change-request", h.requestEmailChange)
	api.POST("/me/email/change-confirm", h.confirmEmailChange)
	// 默认打开方式偏好（按扩展名，user_open_with）：列表、upsert 与删除。
	api.GET("/me/open-with", h.listOpenWith)
	api.PUT("/me/open-with", h.updateOpenWith)
	api.DELETE("/me/open-with", h.deleteOpenWith)
	// 修改密码（认证）：成功撤销其他会话并轮换当前会话。
	api.POST("/auth/change-password", h.changePassword)
	// 两步验证（TOTP，v2）：状态、开始设置、确认启用（返回一次性恢复码）
	// 与禁用（密码或 TOTP 码二选一验证）；登录第二段为公开端点 /auth/login/totp。
	api.GET("/auth/totp", h.totpStatus)
	api.POST("/auth/totp/setup", h.totpSetup)
	api.POST("/auth/totp/confirm", h.totpConfirm)
	api.DELETE("/auth/totp", h.totpDisable)
	// 会话管理（多端登录）：活跃会话列表、撤销单个、撤销全部（含当前）。
	api.GET("/auth/sessions", h.listSessions)
	api.DELETE("/auth/sessions/:id", h.revokeSession)
	api.DELETE("/auth/sessions", h.revokeAllSessions)
	// 个人访问令牌（PAT）：创建（明文仅返回一次）、列表、撤销。
	api.POST("/tokens", h.createToken)
	api.GET("/tokens", h.listTokens)
	api.PATCH("/tokens/:id", h.updateToken)
	api.DELETE("/tokens/:id", h.revokeToken)
	api.GET("/webdav/tokens", h.webdavTokensList)
	api.POST("/webdav/tokens", h.webdavTokenCreate)
	api.DELETE("/webdav/tokens/:id", h.webdavTokenRevoke)
	// 站内通知与通知偏好（本人维度）：列表分页（created_at 游标）+未读数、
	// 单条已读、全部已读、各事件类型开关与更新。
	api.GET("/notifications", h.listNotifications)
	r.GET("/api/v1/ws/notifications", h.wsNotifications)
	// 富文本实时协作房间（ProseMirror prosemirror-collab）：认证/升级与
	// /ws/notifications 一致（JWT 子协议），房间管理见 internal/collab；
	// 写权限校验（ValidateReplaceTarget）与 collab.enabled 开关见 collab.go。
	r.GET("/api/v1/collab/:fileId/ws", h.wsCollab)
	api.POST("/notifications/:id/read", h.markNotificationRead)
	api.POST("/notifications/read-all", h.markAllNotificationsRead)
	api.GET("/notification-preferences", h.listNotificationPreferences)
	api.PUT("/notification-preferences/:type", h.updateNotificationPreference)
	// Webhook 通知渠道（v1.1，本人维度）：注册（一次性 secret 仅本次返回）、
	// 列表（含投递状态）、启停与删除。
	api.POST("/webhooks", h.createWebhook)
	api.GET("/webhooks", h.listWebhooks)
	api.PATCH("/webhooks/:id", h.updateWebhook)
	api.DELETE("/webhooks/:id", h.deleteWebhook)
	api.GET("/files", auth.RequireScope("files:read"), h.listFiles)
	api.POST("/files/from-template", auth.RequireScope("files:write"), h.createOfficeTemplate)
	api.POST("/files/:id/copy", auth.RequireScope("files:write"), h.copyFile)
	// 全文检索（文件名 + 文本内容）：高频读端点，不记录审计；
	// 访问判定与 /files 检索模式一致（个人 owner + 团队在册成员）。
	api.GET("/search", h.searchFiles)
	// 个人仪表盘概览统计（admin 附加全局统计，见 dashboard.go）。
	api.GET("/dashboard", h.dashboardStats)
	api.POST("/folders", h.createFolder)
	api.POST("/folders/:id/agent-tasks", auth.RequireScope("files:write"), h.createAgentTask)
	api.GET("/agent-tasks", h.listAgentTasks)
	api.GET("/agent-tasks/:id", h.getAgentTask)
	api.GET("/agent-tasks/:id/diff", h.getAgentTaskDiff)
	api.GET("/agent-tasks/:id/logs", h.listAgentTaskLogs)
	api.POST("/agent-tasks/:id/cancel", h.cancelAgentTask)
	api.POST("/agent-tasks/:id/apply", auth.RequireScope("files:write"), h.applyAgentTask)
	api.POST("/agent-tasks/:id/discard", h.discardAgentTask)
	api.POST("/agent-tasks/:id/rollback", auth.RequireScope("files:write"), h.rollbackAgentTask)
	api.POST("/folders/:id/snapshots", h.createSnapshot)
	api.GET("/folders/:id/snapshots", h.listSnapshots)
	api.GET("/folders/:id/diff", h.diffSnapshot)
	api.GET("/snapshots/:id", h.getSnapshot)
	api.POST("/snapshots/:id/restore", h.restoreSnapshot)
	// 路径级 ACL（设计 6.5.3/6.5.4 最小落地）：团队空间目录的条目查看与
	// 整体替换（仅团队 owner / 系统 admin；个人空间 400；PUT 记审计 acl.update）。
	api.GET("/folders/:id/acl", h.getFolderACL)
	api.PUT("/folders/:id/acl", h.replaceFolderACL)
	api.GET("/files/:id/comments", auth.RequireScope("files:read"), h.listComments)
	api.POST("/files/:id/comments", auth.RequireScope("files:write"), h.createComment)
	api.POST("/comments/:id/replies", auth.RequireScope("files:write"), h.replyComment)
	api.PATCH("/comments/:id", auth.RequireScope("files:write"), h.patchComment)
	api.DELETE("/comments/:id", auth.RequireScope("files:write"), h.deleteComment)
	api.GET("/files/:id", h.getFile)
	api.PATCH("/files/:id", h.renameFile)
	api.DELETE("/files/:id", h.deleteFile)
	api.GET("/files/:id/download", h.downloadFile)
	// 目录打包下载（流式 zip）：子树预遍历限 2000 条目/2GB（超限 413）。
	api.GET("/files/:id/download.zip", auth.RequireScope("files:read"), h.downloadFolderZip)
	// XMind → Markdown 转换：源须 .xmind，产物经上传管线落库同目录。
	api.POST("/files/:id/convert-markdown", auth.RequireScope("files:write"), h.convertToMarkdown)
	api.GET("/files/:id/preview", h.previewFile)
	// AI 摘要（OpenAI 兼容 /chat/completions）：读权限 + 文本类 + 当前版本
	// available；高频端点不记审计；AI 禁用时 503 AI_DISABLED。
	api.POST("/files/:id/ai/summary", h.fileAISummary)
	// AI 能力第一版：多 Provider 对话（SSE 流式，scope ai:chat，每用户
	// 限流按所选 Provider 的 requests_per_min/daily_quota 执行、全局
	// ai.per_user_per_min 兜底——限流在 handler 内感知请求体的 Provider）
	// 与全类型文件摘要（office/pdf/drawio/excalidraw/dfdoc/文本）；未启用
	//（ai.enabled=false 或无可用 Provider）时 chat/summarize/models 404。
	// status 恒可用（前端 AI 入口显隐）；models 为登录用户可读的可用模型
	// 列表（含能力勾选，绝不回显 api_key）。
	api.GET("/ai/status", h.aiStatus)
	api.GET("/ai/models", h.aiModels)
	// 模型能力自动识别（models.dev 元数据；登录侧别名——个人池模型编辑
	// 复用，公开目录无敏感信息；管理端契约路径 /admin/settings/ai/models/lookup）。
	api.GET("/ai/models/lookup", h.aiModelLookup)
	// 个人 AI 配置（双轨制：本人维度 providers/默认模型/人设/技能/
	// prefer_personal；api_key 掩码回显、PUT 留空继承）。
	api.GET("/ai/personal-settings", h.getAIPersonalSettings)
	api.PUT("/ai/personal-settings", h.putAIPersonalSettings)
	// 平台人设/技能模板（ai.personas / ai.skills，管理员维护、登录可读；
	// 配置数据不随 AI 总开关 404——前端人设下拉/技能弹层恒可拉取）。
	api.GET("/ai/personas", h.aiPersonasList)
	api.GET("/ai/skills", h.aiSkillsList)
	// 外部 MCP 服务器（ai.mcp，管理员维护、登录可读启用项 id/name——
	// 不泄 URL/凭据；对话入口 use_mcp 开关展示用）。
	api.GET("/ai/mcp", h.aiMCPList)
	// 用户 AI 记忆（ai_memory，本人维度、仅手动维护；include_memory 开启
	// 时 chat 取最近 20 条拼入 system 上下文）。
	api.GET("/ai/memory", h.aiMemoryList)
	api.POST("/ai/memory", h.aiMemoryCreate)
	api.DELETE("/ai/memory/:id", h.aiMemoryDelete)
	api.PUT("/ai/memory/:id", h.aiMemoryUpdate)
	api.POST("/ai/chat", auth.RequireScope(auth.ScopeAIChat), h.aiChat)
	api.POST("/ai/summarize", auth.RequireScope(auth.ScopeAIChat), h.aiSummarize)
	// 文件版本管理：版本列表与 current_version 回滚（新版本经上传链路 file_id 写入）。
	api.GET("/files/:id/versions", h.listFileVersions)
	api.GET("/files/:id/versions/:versionId/content", h.fileVersionContent)
	api.DELETE("/files/:id/versions/:versionId", h.deleteFileVersion)
	api.POST("/files/:id/versions/:versionId/restore", h.restoreFileVersion)
	api.GET("/trash", h.listTrash)
	api.POST("/files/:id/restore", auth.RequireScope("files:write"), h.restoreFile)
	api.DELETE("/trash/:id", h.purgeFile)
	// 标签与收藏：标签 CRUD、文件打/去标签、行级星标切换
	//（starred 切换读权限即可；files 列表的 tag/starred 过滤见 listFiles）。
	api.GET("/tags", h.listTags)
	api.POST("/tags", h.createTag)
	api.DELETE("/tags/:id", h.deleteTag)
	api.GET("/files/:id/tags", h.listFileTags)
	api.POST("/files/:id/tags", h.addFileTag)
	api.DELETE("/files/:id/tags/:tagId", h.removeFileTag)
	api.PATCH("/files/:id/starred", h.setFileStarred)
	// 批量操作（部分成功语义；可选 Idempotency-Key 60s 幂等重放，见 batch.go）。
	api.POST("/files/batch/move", h.idempotency, h.batchMove)
	api.POST("/files/batch/trash", h.idempotency, h.batchTrash)
	api.POST("/files/batch/restore", h.idempotency, h.batchRestore)
	api.POST("/files/batch/download", h.idempotency, h.batchDownload)
	api.POST("/uploads", h.createUpload)
	api.PATCH("/uploads/:id", h.patchUpload)
	api.POST("/uploads/:id/complete", h.completeUpload)
	api.GET("/uploads/:id", h.getUpload)
	// tus 1.0.0 断点续传端点，与自定义 /api/v1/uploads API 并存。
	registerTUSRoutes(api.Group("/tus"), h)
	api.POST("/shares", h.createShare)
	api.GET("/shares", h.listShares)
	api.GET("/shares/shared-with-me", h.listSharedWithMe)
	api.GET("/shares/:id", h.getShare)
	api.PATCH("/shares/:id", h.updateShare)
	api.DELETE("/shares/:id", h.revokeShare)
	// 清除已撤销分享的记录（物理删除；须撤销满 30 天，见 shares.go）。
	api.DELETE("/shares/:id/purge", h.purgeShare)
	// 私有分享访问入口（登录用户）：按显式授权访问分享文件。
	api.GET("/shares/:id/files/:fid", h.shareFileInfo)
	api.GET("/shares/:id/files/:fid/download", h.shareFileDownload)
	api.GET("/shares/:id/files/:fid/preview", h.shareFilePreview)
	// 用户目录查找（邀请场景）：任何登录用户可用，仅返回 id 与 username。
	api.GET("/users/lookup", h.lookupUsers)
	// 用户检索（成员/ACL 主体选择器）：任何登录用户可用。
	api.GET("/users/search", h.searchUsers)
	// 用户只读信息（成员列表行点击查看，v2.4）：任何登录用户可用；暴露面
	// 与 /users/search 一致（自托管协作场景可接受，不含手机号等敏感档案）。
	api.GET("/users/:id", h.getUser)
	// 空间与空间文件（统一空间模型，migration 040）。
	api.POST("/spaces", h.createSpace)
	api.GET("/spaces", h.listSpaces)
	api.PATCH("/spaces/:id", h.patchSpace)
	api.DELETE("/spaces/:id", h.deleteSpace)
	// 成员主动退出（非 owner 直接成员）。
	api.POST("/spaces/:id/leave", h.leaveSpace)
	// 所有权转让（仅 owner）：POST {user_id}（五级内置角色，见 spaces.go）。
	api.POST("/spaces/:id/transfer-ownership", h.transferSpaceOwnership)
	api.POST("/spaces/:id/members", h.addSpaceMember)
	api.GET("/spaces/:id/members", h.listSpaceMembers)
	api.PATCH("/spaces/:id/members/:uid", h.updateSpaceMember)
	api.DELETE("/spaces/:id/members/:uid", h.removeSpaceMember)
	// 空间用户组成员（space_group_members：加组/改组角色/移除组）。
	api.GET("/spaces/:id/groups", h.listSpaceGroups)
	api.POST("/spaces/:id/groups", h.addSpaceGroup)
	api.PATCH("/spaces/:id/groups/:gid", h.updateSpaceGroup)
	api.DELETE("/spaces/:id/groups/:gid", h.removeSpaceGroup)
	// 空间邮箱邀请（owner/admin 管理，token 一次性）。
	api.POST("/spaces/:id/invites", h.createSpaceInvite)
	api.GET("/spaces/:id/invites", h.listSpaceInvites)
	api.DELETE("/spaces/:id/invites/:iid", h.revokeSpaceInvite)
	// 接受邀请（登录用户凭 token 入空间；独立前缀避免与 /spaces/:id 路由树冲突）。
	api.POST("/space-invites/join/:token", h.acceptSpaceInvite)
	api.GET("/spaces/:id/files", h.listSpaceFiles)
	api.POST("/spaces/:id/folders", h.createSpaceFolder)
	// ONLYOFFICE 集成：config 探测端点恒注册（认证组；禁用时 enabled=false
	// 且不暴露 server_url）；启用（SetOnlyOffice 注入）时追加挂载 session
	//（认证组）与 download/callback（公开组，绕过 Bearer 与认证接口限流，
	// 自带 JWT 校验与独立轻限流）；未启用时其余 onlyoffice 路由不注册（404）。
	api.GET("/onlyoffice/config", h.onlyofficeConfig)
	if h.onlyoffice != nil {
		h.registerOnlyOfficeRoutes(api, r)
	}
	// draw.io 图表编辑集成：config 探测端点恒注册（认证组；禁用时 enabled=false
	// 且不暴露 url）。编辑器为浏览器侧 iframe embed（postMessage JSON 协议：
	// init→load→save），后端无其他 drawio 路由；保存走通用「上传 file_id
	// 覆盖新版本」链路（前端把导出 XML 作为新版本上传）。
	api.GET("/drawio/config", h.drawioConfig)
	// 网页包安全预览：内容端点挂根路由（/content 在 /api/v1 之外，无 Bearer、
	// 不携带主站 refresh cookie——其 Path 为 /api/v1/auth/refresh），按 IP
	// 独立轻限流；手动解包入口挂认证组。未注入时不注册（默认 404）。
	if h.webpkg != nil {
		r.GET("/content/:pid/*filepath", publicLimiter(NewRateLimiter(h.webpkgRateLimitPerMin)), h.webpkgContent)
		api.POST("/files/:id/webpkg/extract", h.extractWebpkg)
	}
	// 路径型访问（v1.1）：登录 resolve API 换取短期 grant 后经 /raw/* 取
	// 受控原始内容。raw 域挂根路由（在 /api/v1 之外，无 Bearer、不携带主站
	// refresh cookie），授权完全由 HMAC grant + 每请求实时读校验保证；
	// 独立按 IP 轻限流。/raw/share 为公开目录分享子资源入口（token+grant）。
	api.GET("/resolve/:nsType/:nsScope/*path", auth.RequireScope("files:read"), h.resolvePath)
	api.POST("/files/:id/unpack", auth.RequireScope("files:write"), h.unpackZip)
	rawLimiter := publicLimiter(NewRateLimiter(rawRateLimitPerMin))
	r.GET("/raw/auth/:grant/:nsType/:nsScope/*path", rawLimiter, h.serveRawAuth)
	r.HEAD("/raw/auth/:grant/:nsType/:nsScope/*path", rawLimiter, h.serveRawAuth)
	r.GET("/raw/share/:token/:grant/*path", rawLimiter, h.serveRawShare)
	r.HEAD("/raw/share/:token/:grant/*path", rawLimiter, h.serveRawShare)
	// 管理端（admin 组）：RequireAccessToken 之后叠加 RequireRole(admin)。
	admin := api.Group("/admin", auth.RequireRole(auth.RoleAdmin, h.roles))
	admin.GET("/settings", h.listAdminSettings)
	admin.PUT("/settings/:key", h.updateAdminSetting)
	// SMTP 运行时配置（system_settings 的 smtp.* 键）：读生效值（DB 覆盖 →
	// env 回退）/ 写即时生效（邮件发送处每次读库）；密码只写不读。
	admin.GET("/settings/smtp", h.getSMTPSettings)
	admin.PUT("/settings/smtp", h.putSMTPSettings)
	// 测试邮件：用当前生效配置向指定邮箱投递一封测试邮件（错误详情回传）。
	admin.POST("/settings/smtp/test", h.testSMTPSettings)
	// AI 运行时配置（system_settings 的 ai.* 键）：多 Provider CRUD（密钥
	// 只写不读，留空保持）、默认 Provider/温度/max_tokens/每用户限流，
	// 连接测试（发一条 ping 返回延迟/错误）与用量统计（按用户聚合）。
	admin.GET("/settings/agent", h.agentConfig)
	admin.PUT("/settings/agent", h.putAgentConfig)
	// 启动级环境变量只读总览（配置总览页；敏感值脱敏）。
	admin.GET("/settings/env", h.adminGetEnv)
	admin.GET("/settings/ai", h.getAISettings)
	admin.PUT("/settings/ai", h.putAISettings)
	admin.POST("/settings/ai/test", h.testAIProvider)
	admin.POST("/settings/ai/rag/test", h.testAIRAG)
	// 模型能力自动识别（models.dev 元数据，5s 超时 + 10 分钟内存缓存；
	// 未命中/网络失败 found:false——前端静默回退手动选择）。
	admin.GET("/settings/ai/models/lookup", h.aiModelLookup)
	// 重建索引：切换 embedding Provider/模型后重建派生 collection 的向量
	// 索引（分批重投 task:search-index，返回入队数；请求体可选 space_id）。
	admin.POST("/settings/ai/reindex", h.adminPostAIReindex)
	// 平台人设/技能模板（system_settings 的 ai.personas / ai.skills 键）：
	// 登录侧 GET /ai/personas|/ai/skills 读取；管理端整块读替（校验见
	// settings 包：≤50 条、id 唯一、name ≤64、prompt ≤4000）。
	admin.GET("/settings/ai/personas", h.aiPersonasList)
	admin.PUT("/settings/ai/personas", h.adminPutAIPersonas)
	admin.GET("/settings/ai/skills", h.aiSkillsList)
	admin.PUT("/settings/ai/skills", h.adminPutAISkills)
	// 外部 MCP 服务器（system_settings 的 ai.mcp 键）：登录侧 GET /ai/mcp
	// 读启用项；管理端整块读替（auth_header 只写不读、留空继承，回显
	// configured 布尔；校验见 settings 包：≤8 条、id 唯一、url http(s)）。
	admin.GET("/settings/ai/mcp", h.adminGetAIMCPServices)
	admin.PUT("/settings/ai/mcp", h.adminPutAIMCPServices)
	admin.POST("/settings/ai/mcp/test", h.adminTestAIMCP)
	admin.GET("/ai/usage", h.adminAIUsage)
	// HTTPS 运行时切换（热下发 Caddy admin API）：GET 恒注册（未托管时
	// managed=false 供页面降级展示）；PUT 未托管时 503；POST /tls/cert
	// 上传自定义证书（custom 模式，multipart cert+key）。
	admin.GET("/tls", h.adminGetTLS)
	admin.PUT("/tls", h.adminUpdateTLS)
	admin.POST("/tls/cert", h.adminUploadTLSCert)
	// 隔离区管理（G6，仅 admin）：隔离 blob 列表与 rescan/release/delete 处置
	//（release 须显式 confirm=true；全部动作写审计）。
	admin.GET("/quarantine", h.listQuarantine)
	admin.POST("/quarantine/:sha256/action", h.quarantineAction)
	admin.GET("/stats", h.adminStats)
	admin.GET("/audit-logs", h.adminAudit)
	admin.GET("/audit-logs/export.csv", h.adminAuditCSV)
	admin.GET("/backups/status", h.adminBackupStatus)
	admin.POST("/backups/run", h.adminBackupRun)
	admin.POST("/backups/verify", h.adminBackupVerify)
	// 用户管理（C6，设计 6.2.1/9.1.1）：列表（q 前缀检索+分页）、禁用/启用/
	// 改配额/改角色（禁用立即撤销全部会话；不可禁用自己）与重置密码。
	// 设计 DELETE /users/:id 以软禁用替代（数据完整性取舍，见 admin_users.go）。
	admin.GET("/users", h.adminListUsers)
	admin.GET("/users/:id", h.adminGetUser)
	admin.PATCH("/users/:id", h.adminUpdateUser)
	admin.DELETE("/users/:id", h.adminDeleteUser)
	admin.POST("/users/:id/reset-password", h.adminResetUserPassword)
	// 邀请管理（仅 admin）：创建（返回一次性注册链接）、列表、撤销与重发
	//（重发=撤销旧 token 并生成新链接）。
	admin.POST("/invitations", h.createInvitation)
	admin.GET("/invitations", h.listInvitations)
	admin.DELETE("/invitations/:id", h.revokeInvitation)
	admin.POST("/invitations/:id/resend", h.resendInvitation)
	// 用户组管理（migration 035，仅 admin）：组 CRUD 与成员增删；删除组级联
	// 清 group_members（组不删用户）。
	admin.GET("/groups", h.adminListGroups)
	admin.POST("/groups", h.adminCreateGroup)
	admin.PATCH("/groups/:id", h.adminUpdateGroup)
	admin.DELETE("/groups/:id", h.adminDeleteGroup)
	admin.GET("/groups/:id/members", h.adminListGroupMembers)
	admin.POST("/groups/:id/members", h.adminAddGroupMember)
	admin.DELETE("/groups/:id/members/:uid", h.adminRemoveGroupMember)
	// 空间管理（统一空间模型，仅 admin）：列表（含用量/成员数）、越权修改
	//（name/desc/配额）、解散（软删）与已解散空间的彻底删除（物理清理）。
	admin.GET("/spaces", h.adminListSpaces)
	admin.PATCH("/spaces/:id", h.adminPatchSpace)
	admin.DELETE("/spaces/:id", h.adminDeleteSpace)
	admin.DELETE("/spaces/:id/purge", h.adminPurgeSpace)
	// 公开分享接口：无认证、不设 cookie，单独按 IP 限流。
	// 密码校验端点（verify）额外叠加独立按 IP+token 的更严限流（5/min），
	// 防无认证暴力猜测分享密码。
	public := r.Group("/api/v1/public", publicLimiter(NewRateLimiter(publicRateLimit)))
	public.GET("/shares/:token", h.publicShareInfo)
	public.GET("/shares/:token/download", h.publicShareDownload)
	// 目录分享打包下载（流式 zip）：成功消耗一次下载额度（原子消费+限额）。
	public.GET("/shares/:token/download.zip", h.publicShareDownloadZip)
	public.GET("/shares/:token/preview", h.publicSharePreview)
	// Office 文档公开查看会话（OnlyOffice view 配置；集成启用时可用，访客
	// 无需登录，view/download 权限均可预览）。
	public.GET("/shares/:token/office", h.publicShareOfficeConfig)
	// 目录分享树（清单/子文件元数据 + raw grant 签发；密码分享先 verify）。
	public.GET("/shares/:token/tree/*path", h.publicShareTree)
	public.POST("/shares/:token/verify", shareVerifyLimiter(NewRateLimiter(shareVerifyRateLimitPerMin)), h.publicShareVerify)
	// MCP（Model Context Protocol）：JSON-RPC 2.0 over HTTP，挂根级 /mcp
	//（不经 /api/v1 的 CSRF；独立按 IP 轻限流）。依赖经 NewHandler 装配
	//（files/upload/share/space/storage），search 经 SetSearch 注入。
	h.registerMCP(r, jwtSecret)
}

type loginRequest struct {
	// Identifier 为登录标识（C21a，设计 6.1.3）：email 或 username，优先于 Email。
	Identifier string `json:"identifier"`
	// Email 为旧字段（兼容保留）：identifier 缺省时回退使用。
	Email    string `json:"email"`
	Password string `json:"password"`
}

// loginIdentifier 解析登录标识：identifier 优先，缺省回退旧 email 字段。
func (r loginRequest) loginIdentifier() string {
	if id := strings.TrimSpace(r.Identifier); id != "" {
		return id
	}
	return strings.TrimSpace(r.Email)
}

// lockedResponse 为锁定账号的统一应答（C9，设计 6.1.3/7.3）：423 +
// code=ACCOUNT_LOCKED；不计新失败（调用方须在计数前检查）。
func lockedResponse(c *gin.Context) {
	c.JSON(http.StatusLocked, gin.H{"error": "account is locked due to repeated failed logins", "code": "ACCOUNT_LOCKED"})
}

// recordLoginFailure 记录一次登录失败（C9）：达到阈值即锁定；best-effort
// （写库失败不影响统一 401 应答，防把 DB 故障当作凭据差异信号）。
// 阈值/时长经 loginLockoutPolicy 热读取（settings 优先，回落 env 基线）。
func (h *Handler) recordLoginFailure(id uuid.UUID) {
	maxRetries, lockFor := h.loginLockoutPolicy()
	_ = h.users.RecordLoginFailure(id, maxRetries, lockFor)
}

func (h *Handler) login(c *gin.Context) {
	var request loginRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	identifier := request.loginIdentifier()
	// 统一的失败应答与审计：两个分支均返回相同 401，耗时也须对齐。
	reject := func() {
		h.recordAudit(c, audit.Entry{UserID: nil, Action: audit.ActionLoginFailure, ResourceType: audit.ResourceSession, Status: audit.StatusFailure, Metadata: `{"identifier":"` + sanitizeAuditToken(identifier) + `"}`})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
	}
	user, err := h.users.FindActiveByIdentifier(identifier)
	if err != nil {
		// 用户不存在（或非 active）：对包级 dummy 哈希执行等耗 bcrypt 比较，
		// 消除与「密码错误」分支的时序差异，防止账号枚举。
		_ = compareDummyPassword(request.Password)
		reject()
		return
	}
	// 锁定检查（C9）：锁定期间一律 423，不计新失败。
	if auth.IsLocked(user.LockedUntil, time.Now()) {
		lockedResponse(c)
		return
	}
	if h.auth.VerifyPassword(user.PasswordHash, request.Password) != nil {
		h.recordLoginFailure(user.ID)
		reject()
		return
	}
	// 两步验证：enabled 用户在 /login 不发放任何 token（防绕过），前端凭
	// 401 code=TOTP_REQUIRED 转 POST /auth/login/totp 二段提交（密码 + 码，
	// 限流键与 /login 一致）。查询失败 fail closed（500，不发 token）。
	totpEnabled, err := h.auth.TOTPEnabled(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to verify second factor"})
		return
	}
	if totpEnabled {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "totp_required", "code": "TOTP_REQUIRED"})
		return
	}
	access, err := h.auth.AccessToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	refresh, err := h.auth.NewSessionWithInfo(user.ID, sessionInfoFromRequest(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}
	// 成功登录清零失败计数并解除锁定（C9）。
	_ = h.users.ClearLoginFailures(user.ID)
	h.setRefreshCookie(c, refresh)
	h.setCSRFCookie(c)
	h.recordAudit(c, audit.Entry{UserID: &user.ID, Action: audit.ActionLoginSuccess, ResourceType: audit.ResourceSession, ResourceID: user.ID.String(), Status: audit.StatusSuccess})
	c.JSON(http.StatusOK, gin.H{"access_token": access, "token_type": "Bearer"})
}

// compareDummyPassword 对包级 dummy 哈希执行等耗 bcrypt 比较（用户不存在/
// 非 active 分支），消除与「密码错误」分支的时序差异，防止邮箱枚举。
func compareDummyPassword(password string) error {
	return bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(password))
}

// sessionInfoFromRequest 采集会话创建时的请求环境（ip/user_agent，审计用途）。
func sessionInfoFromRequest(c *gin.Context) auth.SessionInfo {
	return auth.SessionInfo{IP: c.ClientIP(), UserAgent: c.GetHeader("User-Agent")}
}

// sanitizeAuditToken 只保留可安全嵌入 JSON 字符串的字符，避免审计日志注入。
func sanitizeAuditToken(value string) string {
	var b []byte
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '@', ch == '.', ch == '-', ch == '_':
			b = append(b, ch)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

// recordAudit 记录审计事件；写入失败不影响主流程。
func (h *Handler) recordAudit(c *gin.Context, e audit.Entry) {
	if ip := c.ClientIP(); ip != "" {
		e.IP = &ip
	}
	e.UserAgent = c.GetHeader("User-Agent")
	if e.Status == "" {
		e.Status = audit.StatusSuccess
	}
	_ = h.audit.Record(e)
}
func (h *Handler) refresh(c *gin.Context) {
	cookie, err := c.Request.Cookie("refresh_token")
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}
	session, replacement, err := h.auth.RotateRefreshToken(cookie.Value)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}
	// 轮换成功后复查用户状态：账号被禁用/删除（或状态查询失败）时立即撤销
	// 刚轮换出的新 refresh token（整个 session 失效）并 401；自动锁定
	//（locked_until 未到期，C9）同样撤销并回 423 ACCOUNT_LOCKED。
	if status, err := h.users.Status(session.UserID); err != nil || (status != auth.StatusActive && status != auth.StatusLocked) {
		_ = h.auth.RevokeRefreshToken(replacement)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	} else if status == auth.StatusLocked {
		_ = h.auth.RevokeRefreshToken(replacement)
		lockedResponse(c)
		return
	}
	access, err := h.auth.AccessToken(session.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	h.setRefreshCookie(c, replacement)
	h.setCSRFCookie(c)
	c.JSON(http.StatusOK, gin.H{"access_token": access, "token_type": "Bearer"})
}
func (h *Handler) logout(c *gin.Context) {
	if cookie, err := c.Request.Cookie("refresh_token"); err == nil {
		_ = h.auth.RevokeRefreshToken(cookie.Value)
	}
	c.SetCookie("refresh_token", "", -1, "/api/v1/auth/refresh", h.cookieDomain, h.cookieSecure, true)
	clearCSRFCookies(c, h.cookieDomain, h.cookieSecure)
	c.Status(http.StatusNoContent)
}
func (h *Handler) setRefreshCookie(c *gin.Context, token string) {
	cookie := &http.Cookie{Name: "refresh_token", Value: token, Path: "/api/v1/auth/refresh", Domain: h.cookieDomain, MaxAge: int(h.refreshTokenTTL.Seconds()), HttpOnly: true, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode}
	http.SetCookie(c.Writer, cookie)
}

func userID(c *gin.Context) uuid.UUID { return c.MustGet(auth.UserIDContextKey).(uuid.UUID) }

// lookupUsers GET /api/v1/users/lookup?q=：按邮箱整串精确或用户名前缀匹配查找用户
// （q 含 @ 时仅邮箱精确匹配；匹配策略见 auth.UserStore.Lookup）。
// 权限考虑：任何登录用户均可查询（添加团队成员/邀请场景需要）；
// 为避免用户枚举与隐私泄露，响应只含 id 与 username，绝不返回 email，
// 且固定 LIMIT 10，并受通用认证接口限流约束。q 为空返回 400。
func (h *Handler) lookupUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing query"})
		return
	}
	users, err := h.users.Lookup(q, 10)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to lookup users"})
		return
	}
	out := make([]gin.H, 0, len(users))
	for _, u := range users {
		out = append(out, gin.H{"id": u.ID, "username": u.Username})
	}
	c.JSON(http.StatusOK, out)
}

// searchUsers GET /api/v1/users/search?q=：成员/ACL 主体选择器的用户检索。
// q 至少 2 个字符（防误触全量枚举）；匹配策略见 auth.UserStore.Search
// （username/nickname 子串，q 含 @ 时也匹配 email）。
// 权限与 lookupUsers 相同：任何登录用户可用（添加团队成员/ACL 场景需要），
// 固定 LIMIT 20；返回 id/username/email/nickname（团队协作场景需 email 辅助
// 区分同名用户，自托管环境内属可接受暴露面）。
func (h *Handler) searchUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if len([]rune(q)) < 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query too short"})
		return
	}
	users, err := h.users.Search(q, 20)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to search users"})
		return
	}
	out := make([]gin.H, 0, len(users))
	for _, u := range users {
		out = append(out, gin.H{"id": u.ID, "username": u.Username, "email": u.Email, "nickname": u.Nickname})
	}
	c.JSON(http.StatusOK, gin.H{"users": out})
}

// getUser GET /api/v1/users/:id：用户只读信息（成员列表行点击查看用户资料
// 弹窗，v2.4）。任何登录用户可用（暴露面与 /users/search 同口径：不含
// 手机号/状态/角色等管理字段）；不存在 404。
func (h *Handler) getUser(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	u, err := h.users.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":         u.ID,
		"username":   u.Username,
		"email":      u.Email,
		"nickname":   u.Nickname,
		"department": u.Department,
		"position":   u.Position,
	})
}

func parseID(c *gin.Context, value string) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return uuid.Nil, false
	}
	return id, true
}
func fileJSON(f files.File) gin.H {
	return gin.H{"id": f.ID, "name": f.Name, "parent_id": f.ParentID, "type": f.Type, "is_root": f.IsRoot, "description": f.Description, "is_public": f.IsPublic, "is_starred": f.IsStarred, "view_count": f.ViewCount, "download_count": f.DownloadCount, "created_at": f.CreatedAt, "updated_at": f.UpdatedAt}
}
func setETag(c *gin.Context, f files.File) {
	c.Header("ETag", fmt.Sprintf("\"%s\"", f.UpdatedAt.UTC().Format(time.RFC3339Nano)))
}

// resolveListSpace 解析 /files 与 /folders 的 space 查询参数：缺省为用户
// 默认空间；显式 space 须为可见空间（读权限），否则 404。
func (h *Handler) resolveListSpace(c *gin.Context, actor uuid.UUID) (uuid.UUID, bool) {
	raw := c.Query("space")
	if raw == "" {
		sp, err := h.spaces.DefaultSpace(actor)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to resolve default space"})
			return uuid.Nil, false
		}
		return sp.ID, true
	}
	spaceID, ok := parseID(c, raw)
	if !ok {
		return uuid.Nil, false
	}
	if !h.requireSpaceRead(c, spaceID, actor) {
		return uuid.Nil, false
	}
	return spaceID, true
}

// defaultSpaceRootID 返回用户默认空间根目录 ID（上传/建目录/模板的缺省
// parent；统一空间模型下取代个人 EnsureRoot 语义）。
func (h *Handler) defaultSpaceRootID(user uuid.UUID) (uuid.UUID, error) {
	sp, err := h.spaces.DefaultSpace(user)
	if err != nil {
		return uuid.Nil, err
	}
	root, err := h.files.SpaceRoot(sp.ID)
	if err != nil {
		return uuid.Nil, err
	}
	return root.ID, nil
}

// listFiles GET /api/v1/files?space=：目录列举（统一空间模型；space 缺省=
// 我的默认空间，parent_id 缺省=空间根目录）或跨目录检索。
// 提供 tag_id 或 starred 过滤时切换为检索模式（忽略 parent_id）：
// 覆盖全部可见空间文件（owner/在册成员，见 files.SearchAccessible）。
// sort=name|updated_at|size × order=asc|desc 对两种模式均生效。
func (h *Handler) listFiles(c *gin.Context) {
	owner := userID(c)
	tagID, ok := h.parseTagFilter(c)
	if !ok {
		return
	}
	starred, ok := parseStarredFilter(c)
	if !ok {
		return
	}
	sortOpt, ok := parseSortQuery(c)
	if !ok {
		return
	}
	// limit 1..1000（缺省 100）：允许显式放大（前端目录树/文件列表拉满
	// 1000，避免多子项目录截断——与 /spaces/:id/files 的 spaceFolderLimit 同口径）。
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if n < 1000 {
			limit = n
		} else {
			limit = 1000
		}
	}
	var out []files.File
	var err error
	if c.Query("recent") == "true" {
		out, err = h.files.Recent(owner, limit)
	} else if tagID != nil || starred != nil {
		out, err = h.files.SearchAccessible(owner, files.SearchOptions{TagID: tagID, Starred: starred, SortOptions: sortOpt, Limit: limit})
	} else {
		spaceID, ok := h.resolveListSpace(c, owner)
		if !ok {
			return
		}
		var parent uuid.UUID
		if parentText := c.Query("parent_id"); parentText == "" {
			root, rerr := h.files.SpaceRoot(spaceID)
			if rerr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to ensure space root"})
				return
			}
			parent = root.ID
		} else if id, pok := parseID(c, parentText); pok {
			parent = id
		} else {
			return
		}
		out, err = h.files.ListSpace(spaceID, parent, limit, files.SpaceListFilter{SortOptions: sortOpt})
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list files"})
		return
	}
	// 网页目录标记：目录直接子级含 index.html 时 has_index_web=true，
	// 前端把该目录默认点击行为切换为"网页打开"。查询失败不阻塞列举。
	folderIDs := make([]uuid.UUID, 0, len(out))
	zipIDs := make([]uuid.UUID, 0, len(out))
	for _, f := range out {
		if f.Type == "folder" {
			folderIDs = append(folderIDs, f.ID)
		} else if strings.HasSuffix(strings.ToLower(f.Name), ".zip") {
			zipIDs = append(zipIDs, f.ID)
		}
	}
	indexWeb, ierr := h.files.HasIndexWebChildren(folderIDs)
	if ierr != nil {
		indexWeb = nil
	}
	// 网页包标记：zip 已成功解包（Extract 校验保证含 index.html）时
	// has_index_web=true，前端在该 zip 行显示「网页」徽标（与目录口径一致）。
	zipWeb := h.webpkg.ReadyFileIDs(zipIDs)
	result := make([]gin.H, 0, len(out))
	for _, f := range out {
		item := fileJSON(f)
		if f.Type == "folder" {
			item["has_index_web"] = indexWeb[f.ID]
		} else if zipWeb[f.ID] {
			item["has_index_web"] = true
		}
		result = append(result, item)
	}
	c.JSON(http.StatusOK, gin.H{"files": result})
}

type folderRequest struct {
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
}

func (h *Handler) createFolder(c *gin.Context) {
	var request folderRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	owner := userID(c)
	spaceID, ok := h.resolveListSpace(c, owner)
	if !ok {
		return
	}
	var parent uuid.UUID
	if request.ParentID == "" {
		root, err := h.files.SpaceRoot(spaceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to ensure space root"})
			return
		}
		parent = root.ID
	} else if id, ok := parseID(c, request.ParentID); ok {
		parent = id
	} else {
		return
	}
	f, err := h.files.CreateFolderIn(owner, parent, request.Name)
	if h.fileError(c, err) {
		return
	}
	setETag(c, f)
	c.JSON(http.StatusCreated, fileJSON(f))
}

type copyRequest struct {
	ParentID string  `json:"parent_id"`
	Name     *string `json:"name"`
}

func (h *Handler) copyFile(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req copyRequest
	if c.ShouldBindJSON(&req) != nil || req.ParentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	parent, ok := parseID(c, req.ParentID)
	if !ok {
		return
	}
	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	uid := userID(c)
	f, err := h.files.Copy(uid, id, parent, name)
	if errors.Is(err, files.ErrFolderCopy) {
		// 仅根目录等不可复制目录命中（普通目录走 CopyFolder 递归复制）。
		c.JSON(http.StatusBadRequest, gin.H{"error": "root folder cannot be copied"})
		return
	}
	if errors.Is(err, files.ErrCopyLimit) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error(), "code": "COPY_LIMIT_EXCEEDED"})
		return
	}
	if h.fileError(c, err) {
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: "file.copy", ResourceType: audit.ResourceFile, ResourceID: f.ID.String()})
	c.JSON(http.StatusCreated, fileJSON(f))
}

type renameRequest struct {
	Name string `json:"name"`
}

func (h *Handler) renameFile(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var request renameRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	f, err := h.files.Rename(userID(c), id, request.Name)
	if h.fileError(c, err) {
		return
	}
	setETag(c, f)
	c.JSON(http.StatusOK, fileJSON(f))
}
func (h *Handler) deleteFile(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if h.fileError(c, h.files.Delete(userID(c), id)) {
		return
	}
	c.Status(http.StatusNoContent)
}
func (h *Handler) fileError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, files.ErrInvalidName):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid name"})
	case errors.Is(err, files.ErrFolderDepth):
		// 目录深度超限（folder.max_depth，建目录/移动校验）。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "FOLDER_DEPTH_LIMIT"})
	case errors.Is(err, files.ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "name conflict"})
	case errors.Is(err, files.ErrSnapshotConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "version referenced by snapshot", "code": "SNAPSHOT_VERSION_PROTECTED"})
	case errors.Is(err, files.ErrSnapshotNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "snapshot not found"})
	case errors.Is(err, files.ErrForbidden):
		// 团队文件写/删/恢复越权（CanWrite/CanDelete 判定，含自定义角色）。
		c.JSON(http.StatusForbidden, gin.H{"error": "no permission for this operation"})
	case errors.Is(err, files.ErrRoot):
		c.JSON(http.StatusForbidden, gin.H{"error": "root folder cannot be changed"})
	case errors.Is(err, files.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
	case errors.Is(err, files.ErrQuotaExceeded):
		// 空间配额超限（统一空间模型）：413 + 机器可读 code。
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error(), "code": "SPACE_QUOTA_EXCEEDED"})
	case errors.Is(err, files.ErrNoVersion):
		c.JSON(http.StatusConflict, gin.H{"error": "file has no current version"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "file operation failed"})
	}
	return true
}
