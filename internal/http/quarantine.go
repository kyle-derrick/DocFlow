// 隔离区管理（仅 admin，G6）：
//
//	GET  /api/v1/admin/quarantine                  隔离 blob 列表（含引用文件名）
//	POST /api/v1/admin/quarantine/:sha256/action   rescan | release | delete
//
// release 须显式 confirm=true（误操作防线）并写 quarantine.release 审计；
// rescan 重新扫描 blob 内容（通过 → available，未通过维持 quarantined）；
// delete 解除全部版本引用后复用 purge blob 两步语义（标 deleting → 行锁
// 复核删对象删行）。三个动作均写审计（quarantine.rescan/release/delete）。
package http

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// QuarantineItem 为隔离 blob 的列表视图：sha256/大小/时间 + 经 file_versions
// join 得到的引用文件名与文件 ID（同内容可被多个版本引用，取最新引用）。
type QuarantineItem struct {
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	MimeType  string    `json:"mime_type"`
	RefCount  int64     `json:"ref_count"`
	FileID    *string   `json:"file_id,omitempty"`
	FileName  *string   `json:"file_name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// quarantineService 抽象隔离区数据访问与处置动作；生产实现为
// gormQuarantineService（注入 db + storage + scanner + blob 清理器），
// 测试可用内存实现验证 HTTP 语义矩阵。
type quarantineService interface {
	// List 返回 status='quarantined' 的 blob（含引用文件信息，时间倒序）。
	List(limit int) ([]QuarantineItem, error)
	// Rescan 重新扫描 blob 内容：通过置 available；未通过维持 quarantined。
	// 返回处置后的 blob 状态。
	Rescan(sha256 string) (string, error)
	// Release 管理员判定误报时解除隔离（quarantined → available）。
	Release(sha256 string) error
	// Delete 删除该 blob 的全部版本引用并物理删除对象与行。
	Delete(sha256 string) error
}

// SetQuarantineService 注入隔离区服务（幂等）；未注入时隔离区端点 503。
func (h *Handler) SetQuarantineService(svc quarantineService) {
	if svc != nil {
		h.quarantine = svc
	}
}

// listQuarantine GET /api/v1/admin/quarantine?limit=：隔离 blob 列表。
func (h *Handler) listQuarantine(c *gin.Context) {
	if h.quarantine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quarantine service is not configured"})
		return
	}
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		limit = n
	}
	items, err := h.quarantine.List(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list quarantined blobs"})
		return
	}
	if items == nil {
		items = []QuarantineItem{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

type quarantineActionRequest struct {
	Action  string `json:"action"`
	Confirm *bool  `json:"confirm"`
}

// quarantineAction POST /api/v1/admin/quarantine/:sha256/action {action,confirm}：
//   - rescan：重新扫描（通过 → available，未通过维持 quarantined）；
//   - release：解除隔离，须显式 confirm=true，缺省/为 false 一律 400；
//   - delete：删除全部版本引用并物理删除对象（current 指向被删版本的文件
//     重指向剩余最新版本，无剩余版本则置空）。
//
// 全部动作写审计（quarantine.rescan / quarantine.release / quarantine.delete）。
func (h *Handler) quarantineAction(c *gin.Context) {
	if h.quarantine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quarantine service is not configured"})
		return
	}
	sha256 := c.Param("sha256")
	var req quarantineActionRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	switch req.Action {
	case "rescan":
		status, err := h.quarantine.Rescan(sha256)
		if errors.Is(err, files.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "quarantined blob not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to rescan blob"})
			return
		}
		h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionQuarantineRescan, ResourceType: audit.ResourceBlob, ResourceID: sha256, Status: audit.StatusSuccess, Metadata: `{"status":"` + status + `"}`})
		c.JSON(http.StatusOK, gin.H{"sha256": sha256, "status": status})
	case "release":
		if req.Confirm == nil || !*req.Confirm {
			c.JSON(http.StatusBadRequest, gin.H{"error": "release requires confirm=true", "code": "CONFIRM_REQUIRED"})
			return
		}
		if err := h.quarantine.Release(sha256); err != nil {
			if errors.Is(err, files.ErrNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "quarantined blob not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to release blob"})
			return
		}
		h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionQuarantineRelease, ResourceType: audit.ResourceBlob, ResourceID: sha256, Status: audit.StatusSuccess})
		c.JSON(http.StatusOK, gin.H{"sha256": sha256, "status": files.BlobStatusAvailable})
	case "delete":
		if err := h.quarantine.Delete(sha256); err != nil {
			if errors.Is(err, files.ErrNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "quarantined blob not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to delete blob"})
			return
		}
		h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionQuarantineDelete, ResourceType: audit.ResourceBlob, ResourceID: sha256, Status: audit.StatusSuccess})
		c.Status(http.StatusNoContent)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid action (rescan|release|delete)"})
	}
}

// blobPurger 抽象物理 blob 清理（生产实现 *files.Store.PurgeBlobs：
// 行锁复核 deleting/0 → 删对象 → 删行），隔离区 delete 复用该两步语义。
type blobPurger interface {
	PurgeBlobs(blobs []files.ObjectBlob, deleteObject func(storageKey string) error) error
}

// gormQuarantineService 为 quarantineService 的 GORM/PostgreSQL 实现。
type gormQuarantineService struct {
	db      *gorm.DB
	storage upload.Storage
	scanner upload.Scanner
	purger  blobPurger
}

// NewQuarantineService 构造隔离区服务：scanner 为安全扫描器（与上传链路
// 同一选型，独立实例无状态）、purger 通常为 *files.Store（PurgeBlobs 复用）。
func NewQuarantineService(db *gorm.DB, storage upload.Storage, scanner upload.Scanner, purger blobPurger) quarantineService {
	return &gormQuarantineService{db: db, storage: storage, scanner: scanner, purger: purger}
}

// quarantineItemRow 为 List 的 SQL 投影行。
type quarantineItemRow struct {
	SHA256    string    `gorm:"column:sha256"`
	Size      int64     `gorm:"column:size"`
	MimeType  string    `gorm:"column:mime_type"`
	RefCount  int64     `gorm:"column:ref_count"`
	CreatedAt time.Time `gorm:"column:created_at"`
	FileID    *string   `gorm:"column:file_id"`
	FileName  *string   `gorm:"column:file_name"`
}

func (g *gormQuarantineService) List(limit int) ([]QuarantineItem, error) {
	if limit < 1 {
		limit = 100
	}
	var rows []quarantineItemRow
	// 文件名经 file_versions join 取引用文件中最新的版本记录（DISTINCT ON
	// 按 blob 聚合一行）；无引用版本（孤儿隔离 blob）时 file_* 为 NULL。
	err := g.db.Raw(`
SELECT b.sha256, b.size, b.mime_type, b.ref_count, b.created_at,
       fv.file_id::text AS file_id, f.name AS file_name
FROM object_blobs b
LEFT JOIN LATERAL (
    SELECT v.file_id, v.created_at
    FROM file_versions v
    WHERE v.object_blob_id = b.id
    ORDER BY v.created_at DESC
    LIMIT 1
) fv ON true
LEFT JOIN files f ON f.id = fv.file_id
WHERE b.status = ?
ORDER BY b.created_at DESC
LIMIT ?`, files.BlobStatusQuarantined, limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]QuarantineItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, QuarantineItem{SHA256: r.SHA256, Size: r.Size, MimeType: r.MimeType, RefCount: r.RefCount, CreatedAt: r.CreatedAt, FileID: r.FileID, FileName: r.FileName})
	}
	return out, nil
}

// loadQuarantined 按 sha256 加载隔离中的 blob；不存在或非隔离态返回 ErrNotFound。
func (g *gormQuarantineService) loadQuarantined(sha256 string) (files.ObjectBlob, error) {
	var blob files.ObjectBlob
	err := g.db.Where("sha256 = ? AND status = ?", sha256, files.BlobStatusQuarantined).First(&blob).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return files.ObjectBlob{}, files.ErrNotFound
	}
	return blob, err
}

func (g *gormQuarantineService) Rescan(sha256 string) (string, error) {
	blob, err := g.loadQuarantined(sha256)
	if err != nil {
		return "", err
	}
	r, err := g.storage.Read(blob.StorageKey)
	if err != nil {
		return "", err
	}
	scanErr := g.scanner.Scan(r)
	r.Close()
	if scanErr != nil {
		// 未通过：维持 quarantined（下次仍可 rescan/release/delete）。
		return files.BlobStatusQuarantined, nil
	}
	result := g.db.Model(&files.ObjectBlob{}).
		Where("id = ? AND status = ?", blob.ID, files.BlobStatusQuarantined).
		Update("status", files.BlobStatusAvailable)
	if result.Error != nil {
		return "", result.Error
	}
	return files.BlobStatusAvailable, nil
}

func (g *gormQuarantineService) Release(sha256 string) error {
	result := g.db.Model(&files.ObjectBlob{}).
		Where("sha256 = ? AND status = ?", sha256, files.BlobStatusQuarantined).
		Update("status", files.BlobStatusAvailable)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return files.ErrNotFound
	}
	return nil
}

// Delete 解除该 blob 的全部版本引用并物理删除对象与行：
//  1. 事务内：加载隔离 blob → 找引用它的 file_versions → 受影响文件的
//     current_version_id 重指向剩余最新版本（无剩余则置 NULL）→ 删版本行
//     → blob 置 ref_count=0/status=deleting；
//  2. 事务提交后复用 PurgeBlobs 的「行锁复核 → 删对象 → 删行」两步语义
//     （期间被复活/仍被引用则跳过，绝不删除仍可能被引用的对象）。
func (g *gormQuarantineService) Delete(sha256 string) error {
	blob, err := g.loadQuarantined(sha256)
	if err != nil {
		return err
	}
	err = g.db.Transaction(func(tx *gorm.DB) error {
		var versions []files.FileVersion
		if err := tx.Where("object_blob_id = ?", blob.ID).Find(&versions).Error; err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, v := range versions {
			if !seen[v.FileID.String()] {
				seen[v.FileID.String()] = true
				// current 指向被删版本时重指向该文件剩余最新版本（排除本 blob）。
				var remaining []files.FileVersion
				if err := tx.Where("file_id = ? AND object_blob_id <> ?", v.FileID, blob.ID).
					Order("version DESC").Find(&remaining).Error; err != nil {
					return err
				}
				var next *string
				if len(remaining) > 0 {
					id := remaining[0].ID.String()
					next = &id
				}
				if err := tx.Model(&files.File{}).Where("id = ?", v.FileID).
					Update("current_version_id", next).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Where("object_blob_id = ?", blob.ID).Delete(&files.FileVersion{}).Error; err != nil {
			return err
		}
		return tx.Model(&files.ObjectBlob{}).Where("id = ?", blob.ID).
			Updates(map[string]any{"ref_count": 0, "status": files.BlobStatusDeleting}).Error
	})
	if err != nil {
		return err
	}
	blob.RefCount = 0
	blob.Status = files.BlobStatusDeleting
	if g.purger != nil {
		return g.purger.PurgeBlobs([]files.ObjectBlob{blob}, g.storage.Delete)
	}
	return nil
}
