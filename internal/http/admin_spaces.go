package http

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/audit"
)

// 本文件实现管理端空间管理（仅 admin，统一空间模型）：
//   GET    /api/v1/admin/spaces           空间列表（含 owner/成员数/用量/配额；
//                                        ?dissolved=1 筛选已解散的软删空间）
//   PATCH  /api/v1/admin/spaces/:id       越权修改（name/description/quota_bytes）
//   DELETE /api/v1/admin/spaces/:id       解散空间（软删；默认空间同样拒绝）
//   DELETE /api/v1/admin/spaces/:id/purge 彻底删除已解散空间（物理删文件/成员/分享）

// adminSpaceRow 为 admin 空间列表的扫描行（单 SQL 聚合，无 N+1）。
type adminSpaceRow struct {
	ID          uuid.UUID  `gorm:"column:id" json:"id"`
	Name        string     `gorm:"column:name" json:"name"`
	Description string     `gorm:"column:description" json:"description"`
	QuotaBytes  int64      `gorm:"column:quota_bytes" json:"quota_bytes"`
	OwnerID     uuid.UUID  `gorm:"column:owner_id" json:"owner_id"`
	OwnerName   string     `gorm:"column:owner_name" json:"owner_name"`
	IsDefault   bool       `gorm:"column:is_default" json:"is_default"`
	MemberCount int64      `gorm:"column:member_count" json:"member_count"`
	StorageUsed int64      `gorm:"column:storage_used" json:"storage_used"`
	CreatedAt   string     `gorm:"column:created_at" json:"created_at"`
	DeletedAt   *time.Time `gorm:"column:deleted_at" json:"deleted_at,omitempty"`
}

// adminListSpaces GET /api/v1/admin/spaces?q=&limit=&offset=&dissolved=：空间
// 列表倒序，q 按名称/owner 用户名过滤；dissolved=1 时列出已解散（软删）
// 空间供「彻底删除」处置。
func (h *Handler) adminListSpaces(c *gin.Context) {
	q := c.Query("q")
	dissolved := c.Query("dissolved") == "1"
	limit, offset := 50, 0
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if raw := c.Query("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}
	db := h.statsDB
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "stats source is not configured"})
		return
	}
	deletedCond := "s.deleted_at IS NULL"
	if dissolved {
		deletedCond = "s.deleted_at IS NOT NULL"
	}
	query := db.Table("spaces s").
		Select(`s.id, s.name, s.description, s.quota_bytes, s.owner_id,
			COALESCE(u.username, '') AS owner_name, s.is_default, s.deleted_at,
			(SELECT COUNT(*) FROM space_members sm WHERE sm.space_id = s.id) AS member_count,
			(SELECT COALESCE(SUM(fv.size), 0) FROM files f
			 JOIN file_versions fv ON fv.id = f.current_version_id
			 WHERE f.space_id = s.id AND f.deleted_at IS NULL
			   AND f.is_root = false AND f.type = 'file') AS storage_used,
			s.created_at`).
		Joins("LEFT JOIN users u ON u.id = s.owner_id").
		Where(deletedCond)
	if q != "" {
		needle := "%" + q + "%"
		query = query.Where("s.name ILIKE ? OR u.username ILIKE ?", needle, needle)
	}
	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list spaces"})
		return
	}
	var rows []adminSpaceRow
	if err := query.Session(&gorm.Session{}).
		Order("s.created_at DESC, s.id").
		Limit(limit).Offset(offset).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list spaces"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"spaces": rows, "total": total})
}

// adminPatchSpace PATCH /api/v1/admin/spaces/:id：系统 admin 越权修改
// （name/description/quota_bytes；quota 不受 space.max_quota 限制）。
func (h *Handler) adminPatchSpace(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		QuotaBytes  *int64  `json:"quota_bytes"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	if req.Name != nil || req.Description != nil {
		current, err := h.spaces.Get(id)
		if spaceError(c, err) {
			return
		}
		name := current.Name
		if req.Name != nil {
			name = *req.Name
		}
		desc := current.Description
		if req.Description != nil {
			desc = *req.Description
		}
		if spaceError(c, h.spaces.Update(actor, id, name, desc)) {
			return
		}
	}
	if req.QuotaBytes != nil {
		if spaceError(c, h.spaces.UpdateQuota(actor, id, *req.QuotaBytes, true)) {
			return
		}
	}
	sp, err := h.spaces.Get(id)
	if spaceError(c, err) {
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: "space.admin_update", ResourceType: audit.ResourceSpace, ResourceID: id.String(), Metadata: `{"by":"admin"}`})
	c.JSON(http.StatusOK, spaceJSON(sp))
}

// adminDeleteSpace DELETE /api/v1/admin/spaces/:id：系统 admin 解散空间
// （软删；默认空间同样拒绝，400）。
func (h *Handler) adminDeleteSpace(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	// 越权路径：临时以空间 owner 身份判定不可行——Delete 校验 actor==owner。
	// 直接调 service 的 owner 判定：系统 admin 可删任意非默认空间。
	sp, err := h.spaces.Get(id)
	if spaceError(c, err) {
		return
	}
	if sp.IsDefault {
		c.JSON(http.StatusBadRequest, gin.H{"error": "default space cannot be deleted", "code": "DEFAULT_SPACE"})
		return
	}
	if spaceError(c, h.spaces.Delete(sp.OwnerID, id)) {
		return
	}
	actor := userID(c)
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionSpaceDelete, ResourceType: audit.ResourceSpace, ResourceID: id.String(), Metadata: `{"by":"admin"}`})
	c.Status(http.StatusNoContent)
}

// adminPurgeSpace DELETE /api/v1/admin/spaces/:id/purge：彻底删除已解散
// （软删）空间——物理删除全部文件/版本（引用计数归零的对象一并删除）、
// 成员/用户组授权/邀请/分享（外键级联）与空间行。仅接受已解散空间
// （未解散 409，前端二次确认防误触）；对象删除失败不阻断行删除（残留
// deleting blob 由 janitor 兜底回收）。
func (h *Handler) adminPurgeSpace(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if h.files == nil || h.storage == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "file store is not configured"})
		return
	}
	deleting, err := h.files.PurgeSpace(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to purge space files"})
		return
	}
	if err := h.spaces.Purge(id); spaceError(c, err) {
		return
	}
	// 行删除已提交：物理对象删除 best-effort（janitor sweepDeletingBlobs 兜底）。
	if len(deleting) > 0 {
		if err := h.files.PurgeBlobs(deleting, h.storage.Delete); err != nil {
			log.Printf("[admin] purge space %s: delete objects: %v", id, err)
		}
	}
	actor := userID(c)
	h.recordAudit(c, audit.Entry{
		UserID: &actor, Action: audit.ActionSpacePurge, ResourceType: audit.ResourceSpace, ResourceID: id.String(),
		Metadata: `{"by":"admin","files_versions_purged":true}`,
	})
	c.Status(http.StatusNoContent)
}
