package http

import (
	"net/http"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DashboardSummary 仪表盘个人统计聚合：文件聚合（files store）+ 分享与
// 上传会话计数。RecentFiles 包含定位目录和当前版本所需字段（见 dashboard 响应）。
type DashboardSummary struct {
	Files        int64
	StorageBytes int64
	TeamFiles    int64
	Shares       int64
	Uploads7d    int64
	RecentFiles  []files.File
}

// dashboardSource 抽象仪表盘聚合查询（生产实现为 gormDashboard）。
type dashboardSource interface {
	Dashboard(user uuid.UUID, now time.Time) (DashboardSummary, error)
}

// gormDashboard 生产实现：文件聚合复用 files.Store.PersonalDashboardStats，
// 有效分享数与近 7 天上传会话数直接对 gorm DB 计数。
type gormDashboard struct {
	files *files.Store
	db    *gorm.DB
}

// NewDashboardSource 构造基于 GORM 的仪表盘聚合源。
func NewDashboardSource(fileStore *files.Store, db *gorm.DB) *gormDashboard {
	return &gormDashboard{files: fileStore, db: db}
}

// DashboardRecentLimit 最近文件条数（v1.1 固定 5 条）。
const DashboardRecentLimit = 5

func (g *gormDashboard) Dashboard(user uuid.UUID, now time.Time) (DashboardSummary, error) {
	agg, err := g.files.PersonalDashboardStats(user, DashboardRecentLimit)
	if err != nil {
		return DashboardSummary{}, err
	}
	summary := DashboardSummary{
		Files:        agg.FileCount,
		StorageBytes: agg.StorageBytes,
		TeamFiles:    agg.TeamFileCount,
		RecentFiles:  agg.RecentFiles,
	}
	// 有效分享数（公开 + 私有）：未撤销且未过期；下载次数耗尽不视为失效。
	if err := g.db.Table("shares").
		Where("owner_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", user, now).
		Count(&summary.Shares).Error; err != nil {
		return DashboardSummary{}, err
	}
	// 近 7 天上传会话数（含进行中与终态会话）。
	if err := g.db.Table("upload_sessions").
		Where("user_id = ? AND created_at >= ?", user, now.Add(-7*24*time.Hour)).
		Count(&summary.Uploads7d).Error; err != nil {
		return DashboardSummary{}, err
	}
	return summary, nil
}

// SetDashboardSource 注入仪表盘聚合源；nil 保持缺省（请求返回 500）。
func (h *Handler) SetDashboardSource(s dashboardSource) {
	if s != nil {
		h.dashboard = s
	}
}

// dashboardStats GET /api/v1/dashboard：个人统计（我的文件数 / 存储占用 / 团队
// 空间文件数 / 有效分享数 / 近 7 天上传会话数 / 最近文件 5 条）；请求者为
// admin 时附加全局统计（复用 admin stats）。角色查询失败时静默降级为个人
// 视图（仪表盘非安全敏感端点，不做 fail closed）；admin 的全局统计查询
// 失败仍返回 500（与 /admin/stats 口径一致）。
func (h *Handler) dashboardStats(c *gin.Context) {
	if h.dashboard == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dashboard source is not configured"})
		return
	}
	user := userID(c)
	now := time.Now()
	summary, err := h.dashboard.Dashboard(user, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to collect dashboard stats"})
		return
	}
	recent := make([]gin.H, 0, len(summary.RecentFiles))
	for _, f := range summary.RecentFiles {
		recent = append(recent, gin.H{
			"id": f.ID, "name": f.Name, "parent_id": f.ParentID,
			"current_version_id": f.CurrentVersionID, "scope_type": f.ScopeType,
			"team_id": f.TeamID, "updated_at": f.UpdatedAt,
		})
	}
	resp := gin.H{
		"files":         summary.Files,
		"storage_bytes": summary.StorageBytes,
		"team_files":    summary.TeamFiles,
		"shares":        summary.Shares,
		"uploads_7d":    summary.Uploads7d,
		"recent_files":  recent,
	}
	if h.roles != nil {
		if role, rerr := h.roles.Role(user); rerr == nil && role == auth.RoleAdmin {
			if h.stats == nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "stats source is not configured"})
				return
			}
			stats, serr := h.stats.Stats()
			if serr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to collect stats"})
				return
			}
			resp["admin"] = stats
		}
	}
	c.JSON(http.StatusOK, resp)
}
