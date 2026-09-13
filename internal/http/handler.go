package http

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/metrics"
	"github.com/docflow/docflow/internal/onlyoffice"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/tasks"
	"github.com/docflow/docflow/internal/team"
	"github.com/docflow/docflow/internal/upload"
	"github.com/docflow/docflow/internal/webpkg"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// userDirectory 抽象用户目录查询（生产实现为 *auth.UserStore），
// login 走 FindActiveByEmail，用户查找/邀请走 Lookup，分享者名走 Username。
type userDirectory interface {
	FindActiveByEmail(email string) (auth.User, error)
	Lookup(q string, limit int) ([]auth.User, error)
	Username(id uuid.UUID) (string, error)
}

var _ userDirectory = (*auth.UserStore)(nil)

type Handler struct {
	auth            *auth.Service
	users           userDirectory
	files           *files.Store
	shares          *share.Service
	teams           *team.Service
	uploads         *upload.Service
	storage         upload.Storage
	cookieSecure    bool
	cookieDomain    string
	refreshTokenTTL time.Duration
	audit           audit.Recorder
	// 管理端依赖（admin 组）：系统设置、统计与角色查询源。
	settings settingsService
	stats    statsSource
	roles    auth.RoleLookup
	// onlyoffice 为 ONLYOFFICE 集成服务；非 nil（SetOnlyOffice 注入）时
	// Register 挂载 /api/v1/onlyoffice 的 session/download/callback 路由，
	// 否则不注册（默认 404）；config 探测端点恒注册（禁用时 enabled=false）。
	onlyoffice                *onlyoffice.Service
	onlyofficeRateLimitPerMin int
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
	tasks tasks.Enqueuer
}

func NewHandler(authService *auth.Service, users *auth.UserStore, fileStore *files.Store, shares *share.Service, teams *team.Service, uploads *upload.Service, storage upload.Storage, cookieSecure bool, cookieDomain string, refreshTokenTTL time.Duration) *Handler {
	return &Handler{auth: authService, users: users, files: fileStore, shares: shares, teams: teams, uploads: uploads, storage: storage, cookieSecure: cookieSecure, cookieDomain: cookieDomain, refreshTokenTTL: refreshTokenTTL, audit: audit.NopRecorder{}}
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

func (h *Handler) Register(r *gin.Engine, jwtSecret string, rateLimit, loginRateLimit, publicRateLimit int) {
	// Prometheus HTTP 指标中间件：全局挂载（须先于任何路由注册），
	// route 标签取 gin 路由模板，未匹配路由（404）归一为 unknown。
	r.Use(metrics.GinMiddleware())
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/ready", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ready"}) })
	// Prometheus 指标端点：挂根路由（不在 /api/v1 下），无认证。
	// 生产环境务必由 Caddy/反向代理或网络层限制访问；契约测试按基础设施
	// 路由豁免（openapi_test.go isExcludedRoute），不入 docs/openapi.yaml。
	if !h.metricsDisabled {
		r.GET("/metrics", gin.WrapH(metrics.Handler()))
	}
	loginLimiterMW := loginLimiter(NewRateLimiter(loginRateLimit))
	authGroup := r.Group("/api/v1/auth")
	authGroup.POST("/login", loginLimiterMW, h.login)
	authGroup.POST("/refresh", h.refresh)
	authGroup.POST("/logout", h.logout)
	api := r.Group("/api/v1", auth.RequireAccessToken(jwtSecret), apiLimiter(NewRateLimiter(rateLimit)))
	api.GET("/files", h.listFiles)
	api.POST("/folders", h.createFolder)
	api.GET("/files/:id", h.getFile)
	api.PATCH("/files/:id", h.renameFile)
	api.DELETE("/files/:id", h.deleteFile)
	api.GET("/files/:id/download", h.downloadFile)
	api.GET("/files/:id/preview", h.previewFile)
	// 文件版本管理：版本列表与 current_version 回滚（新版本经上传链路 file_id 写入）。
	api.GET("/files/:id/versions", h.listFileVersions)
	api.POST("/files/:id/versions/:versionId/restore", h.restoreFileVersion)
	api.GET("/trash", h.listTrash)
	api.POST("/files/:id/restore", h.restoreFile)
	api.DELETE("/trash/:id", h.purgeFile)
	api.POST("/uploads", h.createUpload)
	api.PATCH("/uploads/:id", h.patchUpload)
	api.POST("/uploads/:id/complete", h.completeUpload)
	api.GET("/uploads/:id", h.getUpload)
	// tus 1.0.0 断点续传端点，与自定义 /api/v1/uploads API 并存。
	registerTUSRoutes(api.Group("/tus"), h)
	api.POST("/shares", h.createShare)
	api.GET("/shares", h.listShares)
	api.GET("/shares/shared-with-me", h.listSharedWithMe)
	api.DELETE("/shares/:id", h.revokeShare)
	// 私有分享访问入口（登录用户）：按显式授权访问分享文件。
	api.GET("/shares/:id/files/:fid", h.shareFileInfo)
	api.GET("/shares/:id/files/:fid/download", h.shareFileDownload)
	api.GET("/shares/:id/files/:fid/preview", h.shareFilePreview)
	// 用户目录查找（邀请场景）：任何登录用户可用，仅返回 id 与 username。
	api.GET("/users/lookup", h.lookupUsers)
	// 团队与团队空间。
	api.POST("/teams", h.createTeam)
	api.GET("/teams", h.listTeams)
	api.POST("/teams/:id/members", h.addTeamMember)
	api.GET("/teams/:id/members", h.listTeamMembers)
	api.DELETE("/teams/:id/members/:uid", h.removeTeamMember)
	api.GET("/teams/:id/files", h.listTeamFiles)
	api.POST("/teams/:id/folders", h.createTeamFolder)
	// ONLYOFFICE 集成：config 探测端点恒注册（认证组；禁用时 enabled=false
	// 且不暴露 server_url）；启用（SetOnlyOffice 注入）时追加挂载 session
	//（认证组）与 download/callback（公开组，绕过 Bearer 与认证接口限流，
	// 自带 JWT 校验与独立轻限流）；未启用时其余 onlyoffice 路由不注册（404）。
	api.GET("/onlyoffice/config", h.onlyofficeConfig)
	if h.onlyoffice != nil {
		h.registerOnlyOfficeRoutes(api, r)
	}
	// 网页包安全预览：内容端点挂根路由（/content 在 /api/v1 之外，无 Bearer、
	// 不携带主站 refresh cookie——其 Path 为 /api/v1/auth/refresh），按 IP
	// 独立轻限流；手动解包入口挂认证组。未注入时不注册（默认 404）。
	if h.webpkg != nil {
		r.GET("/content/:pid/*filepath", publicLimiter(NewRateLimiter(h.webpkgRateLimitPerMin)), h.webpkgContent)
		api.POST("/files/:id/webpkg/extract", h.extractWebpkg)
	}
	// 管理端（admin 组）：RequireAccessToken 之后叠加 RequireRole(admin)。
	admin := api.Group("/admin", auth.RequireRole(auth.RoleAdmin, h.roles))
	admin.GET("/settings", h.listAdminSettings)
	admin.PUT("/settings/:key", h.updateAdminSetting)
	admin.GET("/stats", h.adminStats)
	// 公开分享接口：无认证、不设 cookie，单独按 IP 限流。
	public := r.Group("/api/v1/public", publicLimiter(NewRateLimiter(publicRateLimit)))
	public.GET("/shares/:token", h.publicShareInfo)
	public.GET("/shares/:token/download", h.publicShareDownload)
	public.GET("/shares/:token/preview", h.publicSharePreview)
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) login(c *gin.Context) {
	var request loginRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	user, err := h.users.FindActiveByEmail(request.Email)
	if err != nil || h.auth.VerifyPassword(user.PasswordHash, request.Password) != nil {
		h.recordAudit(c, audit.Entry{UserID: nil, Action: audit.ActionLoginFailure, ResourceType: audit.ResourceSession, Status: audit.StatusFailure, Metadata: `{"email":"` + sanitizeAuditToken(request.Email) + `"}`})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	access, err := h.auth.AccessToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	refresh, err := h.auth.NewSession(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}
	h.setRefreshCookie(c, refresh)
	h.recordAudit(c, audit.Entry{UserID: &user.ID, Action: audit.ActionLoginSuccess, ResourceType: audit.ResourceSession, ResourceID: user.ID.String(), Status: audit.StatusSuccess})
	c.JSON(http.StatusOK, gin.H{"access_token": access, "token_type": "Bearer"})
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
	access, err := h.auth.AccessToken(session.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	h.setRefreshCookie(c, replacement)
	c.JSON(http.StatusOK, gin.H{"access_token": access, "token_type": "Bearer"})
}
func (h *Handler) logout(c *gin.Context) {
	if cookie, err := c.Request.Cookie("refresh_token"); err == nil {
		_ = h.auth.RevokeRefreshToken(cookie.Value)
	}
	c.SetCookie("refresh_token", "", -1, "/api/v1/auth/refresh", h.cookieDomain, h.cookieSecure, true)
	c.Status(http.StatusNoContent)
}
func (h *Handler) setRefreshCookie(c *gin.Context, token string) {
	cookie := &http.Cookie{Name: "refresh_token", Value: token, Path: "/api/v1/auth/refresh", Domain: h.cookieDomain, MaxAge: int(h.refreshTokenTTL.Seconds()), HttpOnly: true, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode}
	http.SetCookie(c.Writer, cookie)
}

func userID(c *gin.Context) uuid.UUID { return c.MustGet(auth.UserIDContextKey).(uuid.UUID) }

// lookupUsers GET /api/v1/users/lookup?q=：按邮箱或用户名精确/前缀匹配查找用户。
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
func parseID(c *gin.Context, value string) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return uuid.Nil, false
	}
	return id, true
}
func fileJSON(f files.File) gin.H {
	return gin.H{"id": f.ID, "name": f.Name, "parent_id": f.ParentID, "type": f.Type, "is_root": f.IsRoot, "description": f.Description, "is_public": f.IsPublic, "view_count": f.ViewCount, "download_count": f.DownloadCount, "created_at": f.CreatedAt, "updated_at": f.UpdatedAt}
}
func setETag(c *gin.Context, f files.File) {
	c.Header("ETag", fmt.Sprintf("\"%s\"", f.UpdatedAt.UTC().Format(time.RFC3339Nano)))
}
func (h *Handler) listFiles(c *gin.Context) {
	owner := userID(c)
	parentText := c.Query("parent_id")
	var parent uuid.UUID
	if parentText == "" {
		root, err := h.files.EnsureRoot(owner)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to ensure root folder"})
			return
		}
		parent = root.ID
	} else if id, ok := parseID(c, parentText); ok {
		parent = id
	} else {
		return
	}
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if n < limit {
			limit = n
		}
	}
	out, err := h.files.List(owner, &parent, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list files"})
		return
	}
	result := make([]gin.H, 0, len(out))
	for _, f := range out {
		result = append(result, fileJSON(f))
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
	var parent uuid.UUID
	if request.ParentID == "" {
		root, err := h.files.EnsureRoot(owner)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to ensure root folder"})
			return
		}
		parent = root.ID
	} else if id, ok := parseID(c, request.ParentID); ok {
		parent = id
	} else {
		return
	}
	f, err := h.files.CreateFolder(owner, parent, request.Name)
	if h.fileError(c, err) {
		return
	}
	setETag(c, f)
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
	case errors.Is(err, files.ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "name conflict"})
	case errors.Is(err, files.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "no permission to write this folder"})
	case errors.Is(err, files.ErrRoot):
		c.JSON(http.StatusForbidden, gin.H{"error": "root folder cannot be changed"})
	case errors.Is(err, files.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
	case errors.Is(err, files.ErrNoVersion):
		c.JSON(http.StatusConflict, gin.H{"error": "file has no current version"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "file operation failed"})
	}
	return true
}
