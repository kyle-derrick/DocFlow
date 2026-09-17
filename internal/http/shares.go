package http

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type shareRequest struct {
	FileID           string   `json:"file_id"`
	Permission       string   `json:"permission"`
	Visibility       string   `json:"visibility"`        // public|private，缺省 public
	UserIDs          []string `json:"user_ids"`          // 私有分享：显式授权用户
	TeamIDs          []string `json:"team_ids"`          // 私有分享：授权团队
	ExpiresIn        *int64   `json:"expires_in"`        // 秒；缺省或 0 表示永久
	MaxDownloads     *int     `json:"max_downloads"`     // 缺省表示不限
	Password         string   `json:"password"`          // 公开分享访问密码（4-64 字符，存哈希）
	WatermarkEnabled *bool    `json:"watermark_enabled"` // 缺省用 settings 默认
	WatermarkText    *string  `json:"watermark_text"`    // 自定义模板；缺省用 settings 默认
}

// parseIDList 解析 UUID 列表（去重顺序保留）。
func parseIDList(c *gin.Context, values []string) ([]uuid.UUID, bool) {
	seen := make(map[uuid.UUID]struct{}, len(values))
	out := make([]uuid.UUID, 0, len(values))
	for _, v := range values {
		id, err := uuid.Parse(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id in list"})
			return nil, false
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, true
}

// createShare POST /api/v1/shares 为当前用户文件创建分享。
// visibility=public（默认）返回公开 token（仅本次响应可见一次，之后只能通过 token_hash 匹配）；
// visibility=private 创建私有分享（不生成 token，授权给 user_ids/team_ids）。
// 公开分享可附 password（存哈希，响应仅返回 has_password 标记）；
// 水印开关/模板未显式指定时采用 settings 默认（share.default_watermark /
// share.watermark_text）。
func (h *Handler) createShare(c *gin.Context) {
	var req shareRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	fileID, ok := parseID(c, req.FileID)
	if !ok {
		return
	}
	owner := userID(c)
	permission := req.Permission
	if permission == "" {
		permission = share.PermissionView
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = share.VisibilityPublic
	}
	if visibility != share.VisibilityPublic && visibility != share.VisibilityPrivate {
		c.JSON(http.StatusBadRequest, gin.H{"error": share.ErrInvalidVisibility.Error()})
		return
	}
	var expiresIn time.Duration
	if req.ExpiresIn != nil {
		expiresIn = time.Duration(*req.ExpiresIn) * time.Second
	}
	opts := share.ShareOptions{Password: req.Password, WatermarkEnabled: req.WatermarkEnabled, WatermarkText: req.WatermarkText}
	var out gin.H
	if visibility == share.VisibilityPrivate {
		if req.Password != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "password is only allowed for public shares"})
			return
		}
		userIDs, ok := parseIDList(c, req.UserIDs)
		if !ok {
			return
		}
		teamIDs, ok := parseIDList(c, req.TeamIDs)
		if !ok {
			return
		}
		created, err := h.shares.CreatePrivateWithOptions(owner, fileID, permission, expiresIn, req.MaxDownloads, userIDs, teamIDs, opts)
		if err != nil {
			h.shareCreateError(c, err)
			return
		}
		h.recordAudit(c, audit.Entry{UserID: &owner, Action: audit.ActionShareCreate, ResourceType: audit.ResourceShare, ResourceID: created.ID.String(), Metadata: `{"file_id":"` + created.FileID.String() + `","permission":"` + created.Permission + `","visibility":"private","users":` + strconv.Itoa(len(userIDs)) + `,"teams":` + strconv.Itoa(len(teamIDs)) + `}`})
		out = shareJSON(created)
		out["visibility"] = share.VisibilityPrivate
		// 私有分享无 token / share_url。
	} else {
		if len(req.UserIDs) > 0 || len(req.TeamIDs) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user_ids/team_ids are only allowed for private shares"})
			return
		}
		created, token, err := h.shares.CreatePublic(owner, fileID, permission, expiresIn, req.MaxDownloads, opts)
		if err != nil {
			h.shareCreateError(c, err)
			return
		}
		hasPassword := "false"
		if created.HasPassword() {
			hasPassword = "true"
		}
		h.recordAudit(c, audit.Entry{UserID: &owner, Action: audit.ActionShareCreate, ResourceType: audit.ResourceShare, ResourceID: created.ID.String(), Metadata: `{"file_id":"` + created.FileID.String() + `","permission":"` + created.Permission + `","max_downloads":` + nullableIntJSON(created.MaxDownloads) + `,"password":` + hasPassword + `}`})
		out = shareJSON(created)
		out["visibility"] = share.VisibilityPublic
		out["token"] = token
		out["share_url"] = "/api/v1/public/shares/" + token
	}
	c.JSON(http.StatusCreated, out)
}

func nullableIntJSON(v *int) string {
	if v == nil {
		return "null"
	}
	return strconv.Itoa(*v)
}

func (h *Handler) shareCreateError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, share.ErrInvalidPermission), errors.Is(err, share.ErrInvalidExpiry), errors.Is(err, share.ErrInvalidMaxDownloads), errors.Is(err, share.ErrInvalidPassword), errors.Is(err, share.ErrInvalidWatermarkText), errors.Is(err, share.ErrInvalidVisibility):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, share.ErrPublicDisabled):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error(), "code": "PUBLIC_SHARING_DISABLED"})
	case errors.Is(err, share.ErrForbidden):
		// 团队文件 CanShare 门控（设计 6.5.5）：viewer/无 share 权限角色不可创建分享。
		c.JSON(http.StatusForbidden, gin.H{"error": "no permission to share this file"})
	case errors.Is(err, share.ErrFileNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
	case errors.Is(err, share.ErrFileNotShareable):
		c.JSON(http.StatusConflict, gin.H{"error": "file cannot be shared"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to create share"})
	}
}

// listShares GET /api/v1/shares 列出当前用户的分享（created_at 倒序）。
// 每条附 file_name（文件已删除时为空）与 visibility（public|private）。
func (h *Handler) listShares(c *gin.Context) {
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
	out, err := h.shares.ListWithFileNames(userID(c), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list shares"})
		return
	}
	items := make([]gin.H, 0, len(out))
	for _, it := range out {
		item := shareJSON(it.Share)
		item["visibility"] = it.Share.Visibility
		item["file_name"] = it.FileName
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"shares": items})
}

// listSharedWithMe GET /api/v1/shares/shared-with-me：分享给当前用户的有效私有分享列表
// （share_users 显式授权或 share_teams 团队成员；公开分享与已撤销/过期/达上限的不含）。
// 每条附文件元数据（name/size/mime_type）与分享者用户名（owner_username，不含 email）。
func (h *Handler) listSharedWithMe(c *gin.Context) {
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
	items, err := h.shares.SharedWithMe(userID(c), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list shared with me"})
		return
	}
	out := make([]gin.H, 0, len(items))
	for _, it := range items {
		out = append(out, gin.H{
			"id":             it.Share.ID,
			"file_id":        it.Share.FileID,
			"name":           it.FileName,
			"size":           it.FileSize,
			"mime_type":      it.FileMime,
			"permission":     it.Share.Permission,
			"owner_username": it.OwnerName,
			"expires_at":     it.Share.ExpiresAt,
			"created_at":     it.Share.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"shares": out})
}

// revokeShare DELETE /api/v1/shares/:id 撤销分享（幂等）。
func (h *Handler) revokeShare(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	owner := userID(c)
	revoked, err := h.shares.Revoke(owner, id)
	if err != nil {
		if errors.Is(err, share.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to revoke share"})
		}
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &owner, Action: audit.ActionShareRevoke, ResourceType: audit.ResourceShare, ResourceID: revoked.ID.String(), Metadata: `{"file_id":"` + revoked.FileID.String() + `"}`})
	c.Status(http.StatusNoContent)
}

// ---------- 分享详情与访问统计（C8，设计 6.6.3 / 3.2.20） ----------

// accessEventJSON 序列化最近访问记录条目（脱敏：time / action / ip_prefix /
// user_agent 摘要，不含 ip_hash 与内部 ID）。
func accessEventJSON(e share.AccessEvent) gin.H {
	return gin.H{"time": e.CreatedAt, "action": e.Action, "ip_prefix": e.IPPrefix, "user_agent": e.UserAgent}
}

// shareStatsJSON 序列化统计聚合：总访问次数、独立访客与最近 20 条访问记录。
func shareStatsJSON(stats share.AccessStats) gin.H {
	recent := make([]gin.H, 0, len(stats.Recent))
	for _, e := range stats.Recent {
		recent = append(recent, accessEventJSON(e))
	}
	return gin.H{"total_access": stats.TotalAccess, "unique_visitors": stats.UniqueVisitors, "recent": recent}
}

// getShare GET /api/v1/shares/:id：分享详情（仅 owner）。脱敏全字段（不含
// token/password 哈希，仅 has_password 标记）+ 关联文件名 + 访问统计
// （total_access / unique_visitors / 最近 20 条访问记录，IP 已脱敏为前缀）。
func (h *Handler) getShare(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	detail, err := h.shares.GetDetail(userID(c), id)
	if err != nil {
		if errors.Is(err, share.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load share"})
		}
		return
	}
	out := shareJSON(detail.Share)
	out["visibility"] = detail.Share.Visibility
	out["file_name"] = detail.FileName
	out["stats"] = shareStatsJSON(detail.Stats)
	c.JSON(http.StatusOK, out)
}

// shareUpdateRequest 为 PATCH /api/v1/shares/:id 请求体：nil 字段不修改；
// expires_in / max_downloads 取 0 表示清除限制（永久 / 不限）；
// watermark_text 取空串表示恢复默认模板。
type shareUpdateRequest struct {
	ExpiresIn        *int64  `json:"expires_in"`
	MaxDownloads     *int    `json:"max_downloads"`
	WatermarkEnabled *bool   `json:"watermark_enabled"`
	WatermarkText    *string `json:"watermark_text"`
}

// updateShare PATCH /api/v1/shares/:id（仅 owner）：修改有效期 / 下载上限 /
// 水印开关与模板。permission、visibility、授权名单与密码不可改
// （密码变更请撤销后重建分享）。返回更新后的分享记录。
func (h *Handler) updateShare(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req shareUpdateRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	patch := share.SharePatch{MaxDownloads: req.MaxDownloads, WatermarkEnabled: req.WatermarkEnabled, WatermarkText: req.WatermarkText}
	if req.ExpiresIn != nil {
		d := time.Duration(*req.ExpiresIn) * time.Second
		patch.ExpiresIn = &d
	}
	updated, err := h.shares.Update(userID(c), id, patch)
	if err != nil {
		switch {
		case errors.Is(err, share.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		case errors.Is(err, share.ErrInvalidExpiry), errors.Is(err, share.ErrInvalidMaxDownloads), errors.Is(err, share.ErrInvalidWatermarkText):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to update share"})
		}
		return
	}
	out := shareJSON(updated)
	out["visibility"] = updated.Visibility
	c.JSON(http.StatusOK, out)
}

func shareJSON(s share.Share) gin.H {
	return gin.H{"id": s.ID, "file_id": s.FileID, "permission": s.Permission, "has_password": s.HasPassword(), "watermark_enabled": s.WatermarkEnabled, "watermark_text": s.WatermarkText, "expires_at": s.ExpiresAt, "max_downloads": s.MaxDownloads, "download_count": s.DownloadCount, "revoked_at": s.RevokedAt, "created_at": s.CreatedAt}
}

// ---------- 公开分享密码保护（设计 6.6.2 / US-003） ----------

// shareSessionCookiePrefix / cookie 常量：cookie 名为 docflow_sa_<token 前 8 字符>，
// HttpOnly + SameSite=Lax，Path 限定 /api/v1/public（不随认证接口发送），1 小时。
const (
	shareSessionCookiePrefix = "docflow_sa_"
	shareSessionCookiePath   = "/api/v1/public"
	// shareSessionCookieTTL 与服务层会话有效期（share.shareSessionTTL）一致。
	shareSessionCookieTTL = time.Hour
)

// shareSessionCookieName 返回 token 对应的会话 cookie 名（前 8 字符区分不同分享，
// 同页多分享互不覆盖）。
func shareSessionCookieName(token string) string {
	prefix := token
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return shareSessionCookiePrefix + prefix
}

// shareSessionAllowed 校验密码保护分享的访问会话：无密码分享直接放行；
// 有密码但 cookie 缺失/无效时写 401 {"error":"password_required",
// "code":"PASSWORD_REQUIRED"} 并返回 false。会话有效但分享已失效由
// 后续 Resolve 系列判定（410）。
func (h *Handler) shareSessionAllowed(c *gin.Context, token string, sh share.Share) bool {
	if !sh.HasPassword() {
		return true
	}
	value, err := c.Cookie(shareSessionCookieName(token))
	if err == nil && h.shares.ValidateShareSession(sh.ID, value) {
		return true
	}
	c.JSON(http.StatusUnauthorized, gin.H{"error": "password_required", "code": "PASSWORD_REQUIRED"})
	return false
}

// publicShareVerify POST /api/v1/public/shares/:token/verify {password}：
// 密码正确时创建 1 小时访问会话并经 HttpOnly cookie 下发，返回 {ok:true}；
// 错误密码 401（独立按 IP+token 限流 5/min，见 Register）；未设密码 400；
// token 不存在 404、失效 410。明文密码不落库（只存 SHA-256(password||id)）。
func (h *Handler) publicShareVerify(c *gin.Context) {
	token := c.Param("token")
	var req struct {
		Password string `json:"password"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	value, err := h.shares.VerifySharePassword(token, req.Password)
	switch {
	case err == nil:
		cookie := &http.Cookie{Name: shareSessionCookieName(token), Value: value, Path: shareSessionCookiePath, MaxAge: int(shareSessionCookieTTL.Seconds()), HttpOnly: true, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode}
		http.SetCookie(c.Writer, cookie)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	case errors.Is(err, share.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
	case errors.Is(err, share.ErrGone):
		c.JSON(http.StatusGone, gin.H{"error": "share is no longer available"})
	case errors.Is(err, share.ErrPasswordNotSet):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, share.ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to verify share password"})
	}
}

// recordPublicAccessEvent 写入公开访问事件（file_access_events，设计 3.2.20）：
// ip_hash = SHA-256(accessSalt || ip)（明文 IP 不落库），ip_prefix 为脱敏
// 展示前缀，UA 截断 512 字节。尽力而为：写入失败不影响主响应。
func (h *Handler) recordPublicAccessEvent(c *gin.Context, r share.Resolved, action string) {
	if h.shares == nil {
		return
	}
	ip := c.ClientIP()
	hash := sha256.Sum256([]byte(h.accessSalt + ip))
	ua := c.GetHeader("User-Agent")
	if len(ua) > 512 {
		ua = ua[:512]
	}
	_ = h.shares.RecordAccessEvent(share.AccessEvent{
		FileID:    r.File.ID,
		ShareID:   r.Share.ID,
		Action:    action,
		IPHash:    hex.EncodeToString(hash[:]),
		IPPrefix:  share.IPPrefix(ip),
		UserAgent: ua,
		CreatedAt: time.Now().UTC(),
	})
}

// publicShareError 映射公开接口错误：不存在 404、失效 410、权限不足/不可用 403，
// 其余一律 500 且不泄露内部细节。
func publicShareError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, share.ErrNotFound), errors.Is(err, share.ErrFileNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
	case errors.Is(err, share.ErrGone), errors.Is(err, share.ErrDownloadLimit):
		c.JSON(http.StatusGone, gin.H{"error": "share is no longer available"})
	case errors.Is(err, share.ErrDownloadForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "download not allowed"})
	case errors.Is(err, share.ErrFileNotAvailable):
		c.JSON(http.StatusForbidden, gin.H{"error": "file is not available for download"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to process share"})
	}
	return true
}

// publicShareInfo GET /api/v1/public/shares/:token 公开文件元数据（无认证、不设 cookie）。
// 仅暴露安全字段：名称、大小、mime、版本状态等；不含 owner、内部 ID 与存储路径。
// 密码保护分享须先经 /verify 取得会话 cookie，否则 401 PASSWORD_REQUIRED。
// 附带水印渲染结果（watermark_enabled / watermark_text，模板占位符已替换：
// {email}/{ip} → 脱敏 IP 前缀、{date} → 日期、{name} → 文件名）。
func (h *Handler) publicShareInfo(c *gin.Context) {
	token := c.Param("token")
	r, err := h.shares.Resolve(token)
	if publicShareError(c, err) {
		return
	}
	if !h.shareSessionAllowed(c, token, r.Share) {
		return
	}
	// 目录分享根：无版本/blob 语义，返回目录元数据（子树经 /tree 与 /raw/share）。
	if r.File.Type == "folder" {
		c.JSON(http.StatusOK, gin.H{
			"name":              r.File.Name,
			"type":              "folder",
			"permission":        r.Share.Permission,
			"expires_at":        r.Share.ExpiresAt,
			"max_downloads":     r.Share.MaxDownloads,
			"download_count":    r.Share.DownloadCount,
			"watermark_enabled": r.Share.WatermarkEnabled,
			"watermark_text":    watermarkTextOf(r, c),
		})
		return
	}
	var watermarkText any
	if r.Share.WatermarkEnabled {
		watermarkText = share.RenderWatermark(r.Share.WatermarkTemplate(), r.File.Name, c.ClientIP(), time.Now())
	}
	c.JSON(http.StatusOK, gin.H{
		"name":              r.File.Name,
		"type":              "file",
		"size":              r.Blob.Size,
		"mime_type":         r.Blob.MimeType,
		"version":           r.Version.Version,
		"version_status":    r.Blob.Status,
		"permission":        r.Share.Permission,
		"expires_at":        r.Share.ExpiresAt,
		"max_downloads":     r.Share.MaxDownloads,
		"download_count":    r.Share.DownloadCount,
		"watermark_enabled": r.Share.WatermarkEnabled,
		"watermark_text":    watermarkText,
	})
}

// watermarkTextOf 渲染分享水印文案（目录分享根用目录名）。
func watermarkTextOf(r share.Resolved, c *gin.Context) any {
	if !r.Share.WatermarkEnabled {
		return nil
	}
	return share.RenderWatermark(r.Share.WatermarkTemplate(), r.File.Name, c.ClientIP(), time.Now())
}

// publicShareDownload GET /api/v1/public/shares/:token/download 公开流式下载。
// 校验权限与 ObjectBlob 可用性，成功时已原子递增分享与文件下载计数，并写入审计
// 与访问事件（action=download）。密码保护分享须先经 /verify 取得会话 cookie
// （未通过时在消耗下载计数之前拦截 401 PASSWORD_REQUIRED）。
// 存储读取失败（500）时经 DecrementDownload 补偿回退已消耗的分享/文件计数。
// 同样支持 Range 与 Content-Disposition。
func (h *Handler) publicShareDownload(c *gin.Context) {
	token := c.Param("token")
	// 先做不消耗计数的解析，完成密码会话校验后再进入消耗路径。
	pre, err := h.shares.Resolve(token)
	if publicShareError(c, err) {
		return
	}
	if !h.shareSessionAllowed(c, token, pre.Share) {
		return
	}
	r, err := h.shares.ResolveForDownload(token)
	if publicShareError(c, err) {
		return
	}
	fileRange, hasRange, err := parseRange(c.GetHeader("Range"), r.Blob.Size)
	if err != nil {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", r.Blob.Size))
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}
	// 区间读取统一走 upload.ReadSection（S3 原生 Range / LocalStorage Seek；
	// 无 Range 时整读 [0, size)），不再依赖 io.Seeker 断言。
	start, length := int64(0), r.Blob.Size
	if hasRange {
		start, length = fileRange.Start, fileRange.Length()
	}
	reader, err := upload.ReadSection(h.storage, r.Blob.StorageKey, start, length)
	if err != nil {
		_ = h.shares.DecrementDownload(r)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	defer reader.Close()
	h.recordAudit(c, audit.Entry{UserID: nil, Action: audit.ActionPublicDownload, ResourceType: audit.ResourceShare, ResourceID: r.Share.ID.String(), Metadata: `{"file_id":"` + r.File.ID.String() + `","size":` + strconv.FormatInt(r.Blob.Size, 10) + `}`})
	h.recordPublicAccessEvent(c, r, share.ActionDownload)
	status := http.StatusOK
	contentLength := r.Blob.Size
	if hasRange {
		status = http.StatusPartialContent
		contentLength = fileRange.Length()
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", fileRange.Start, fileRange.End, r.Blob.Size))
	}
	c.Header("Content-Disposition", contentDisposition(r.File.Name))
	c.Header("Accept-Ranges", "bytes")
	c.DataFromReader(status, contentLength, r.Blob.MimeType, reader, nil)
}

// parseShareFileParams 解析 /api/v1/shares/:id/files/:fid 路径参数。
func (h *Handler) parseShareFileParams(c *gin.Context) (shareID, fileID uuid.UUID, ok bool) {
	shareID, ok = parseID(c, c.Param("id"))
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	fileID, ok = parseID(c, c.Param("fid"))
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return shareID, fileID, true
}

// privateShareError 映射私有分享访问错误：未授权 403、不存在 404、失效 410，
// 其余一律 500 且不泄露内部细节（含分享/文件存在性）。
func privateShareError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, share.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
	default:
		// 复用公开接口映射：ErrNotFound→404、ErrGone/ErrDownloadLimit→410、
		// ErrDownloadForbidden/ErrFileNotAvailable→403、其余 500。
		publicShareError(c, err)
	}
	return true
}

// shareFileInfo GET /api/v1/shares/:id/files/:fid：私有分享文件元数据（登录用户）。
// owner 直接可见；其他用户须通过 CanAccess（显式授权或团队成员）。
func (h *Handler) shareFileInfo(c *gin.Context) {
	shareID, fileID, ok := h.parseShareFileParams(c)
	if !ok {
		return
	}
	r, err := h.shares.ResolveForUser(shareID, fileID, userID(c))
	if privateShareError(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"name":           r.File.Name,
		"size":           r.Blob.Size,
		"mime_type":      r.Blob.MimeType,
		"version":        r.Version.Version,
		"version_status": r.Blob.Status,
		"permission":     r.Share.Permission,
		"expires_at":     r.Share.ExpiresAt,
		"max_downloads":  r.Share.MaxDownloads,
		"download_count": r.Share.DownloadCount,
	})
}

// shareFileDownload GET /api/v1/shares/:id/files/:fid/download：私有分享流式下载（登录用户）。
// 校验授权与下载权限，成功时原子递增分享与文件下载计数，支持 Range。
// 存储读取失败（500）时经 DecrementDownload 补偿回退已消耗的分享/文件计数。
func (h *Handler) shareFileDownload(c *gin.Context) {
	shareID, fileID, ok := h.parseShareFileParams(c)
	if !ok {
		return
	}
	user := userID(c)
	r, err := h.shares.ResolveForUserForDownload(shareID, fileID, user)
	if privateShareError(c, err) {
		return
	}
	fileRange, hasRange, err := parseRange(c.GetHeader("Range"), r.Blob.Size)
	if err != nil {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", r.Blob.Size))
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}
	// 同公开下载：区间读取统一走 upload.ReadSection。
	start, length := int64(0), r.Blob.Size
	if hasRange {
		start, length = fileRange.Start, fileRange.Length()
	}
	reader, err := upload.ReadSection(h.storage, r.Blob.StorageKey, start, length)
	if err != nil {
		_ = h.shares.DecrementDownload(r)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	defer reader.Close()
	h.recordAudit(c, audit.Entry{UserID: &user, Action: audit.ActionPublicDownload, ResourceType: audit.ResourceShare, ResourceID: r.Share.ID.String(), Metadata: `{"file_id":"` + r.File.ID.String() + `","size":` + strconv.FormatInt(r.Blob.Size, 10) + `,"access":"private"}`})
	status := http.StatusOK
	contentLength := r.Blob.Size
	if hasRange {
		status = http.StatusPartialContent
		contentLength = fileRange.Length()
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", fileRange.Start, fileRange.End, r.Blob.Size))
	}
	c.Header("Content-Disposition", contentDisposition(r.File.Name))
	c.Header("Accept-Ranges", "bytes")
	c.DataFromReader(status, contentLength, r.Blob.MimeType, reader, nil)
}

// shareFilePreview GET /api/v1/shares/:id/files/:fid/preview：私有分享内联预览（登录用户）。
// view 与 download 权限均可预览，不消耗分享 download_count。
func (h *Handler) shareFilePreview(c *gin.Context) {
	shareID, fileID, ok := h.parseShareFileParams(c)
	if !ok {
		return
	}
	r, err := h.shares.ResolveForUserForPreview(shareID, fileID, userID(c))
	if privateShareError(c, err) {
		return
	}
	h.servePreviewBlob(c, r.File.Name, r.Blob, func() error {
		return h.shares.IncrementPreviewView(r)
	})
}
