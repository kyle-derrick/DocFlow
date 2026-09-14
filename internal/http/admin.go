package http

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// settingsService 抽象管理端所需的系统能力（生产实现为 *settings.Store）。
type settingsService interface {
	GetAll() ([]settings.SettingView, error)
	Set(key string, value any, actor uuid.UUID) (any, error)
}

// statsSource 抽象基础统计计数（生产实现为 gormStats）。
type statsSource interface {
	Stats() (AdminStats, error)
}

// AdminStats 是 GET /api/v1/admin/stats 的基础统计（各表行数）。
type AdminStats struct {
	Users    int64 `json:"users"`
	Files    int64 `json:"files"`
	Uploads  int64 `json:"uploads"`
	Sessions int64 `json:"sessions"`
	Shares   int64 `json:"shares"`
	Tokens   int64 `json:"tokens"`
}

// gormStats 用 COUNT 查询汇总基础统计。
type gormStats struct{ db *gorm.DB }

// NewAdminStats 构造基于 GORM 的统计源。
func NewAdminStats(db *gorm.DB) *gormStats { return &gormStats{db: db} }

func (g *gormStats) Stats() (AdminStats, error) {
	var s AdminStats
	counts := []struct {
		table string
		dst   *int64
	}{
		{"users", &s.Users},
		{"files", &s.Files},
		{"upload_sessions", &s.Uploads},
		{"sessions", &s.Sessions},
		{"shares", &s.Shares},
		{"api_tokens", &s.Tokens},
	}
	for _, c := range counts {
		if err := g.db.Table(c.table).Count(c.dst).Error; err != nil {
			return AdminStats{}, err
		}
	}
	return s, nil
}

// SetSettingsService 注入系统设置服务；nil 保持缺省（请求返回 500）。
func (h *Handler) SetSettingsService(s settingsService) {
	if s != nil {
		h.settings = s
	}
}

// SetStatsSource 注入统计源；nil 保持缺省（请求返回 500）。
func (h *Handler) SetStatsSource(s statsSource) {
	if s != nil {
		h.stats = s
	}
}

// SetRoleLookup 注入角色查询源（RequireRole 用，生产实现为 *auth.UserStore）。
func (h *Handler) SetRoleLookup(l auth.RoleLookup) {
	if l != nil {
		h.roles = l
	}
}

// listAdminSettings GET /api/v1/admin/settings 返回全部内置键的
// 当前值（未设置取默认值）+ 类型 + 描述。
func (h *Handler) listAdminSettings(c *gin.Context) {
	if h.settings == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings service is not configured"})
		return
	}
	views, err := h.settings.GetAll()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load settings"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": views})
}

// updateAdminSetting PUT /api/v1/admin/settings/{key} body {value}：
// 校验类型与范围后写入；未知键 404、类型/范围不符 400。
// 审计由 settings.Store.Set 写入（settings.update，含 actor 与新值）。
func (h *Handler) updateAdminSetting(c *gin.Context) {
	if h.settings == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings service is not configured"})
		return
	}
	var request struct {
		Value any `json:"value"`
	}
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	key := c.Param("key")
	value, err := h.settings.Set(key, request.Value, userID(c))
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"key": key, "value": value})
	case errors.Is(err, settings.ErrUnknownKey):
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown settings key"})
	case errors.Is(err, settings.ErrInvalidType), errors.Is(err, settings.ErrInvalidValue):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to update setting"})
	}
}

// adminStats GET /api/v1/admin/stats 返回基础统计计数。
type backupStatus struct {
	Configured bool        `json:"configured"`
	Latest     *backupFile `json:"latest,omitempty"`
}

type backupFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified_at"`
	Manifest string    `json:"manifest"`
}

func (h *Handler) adminBackupStatus(c *gin.Context) {
	dir := h.backupDir
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("BACKUP_DIR"))
	}
	result := backupStatus{Configured: dir != ""}
	if dir != "" {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasPrefix(entry.Name(), "docflow-") || !strings.HasSuffix(entry.Name(), ".sql") {
					continue
				}
				info, err := entry.Info()
				if err != nil || result.Latest != nil && !info.ModTime().After(result.Latest.Modified) {
					continue
				}
				result.Latest = &backupFile{Name: entry.Name(), Size: info.Size(), Modified: info.ModTime(), Manifest: filepath.Join(dir, entry.Name()+".sha256")}
			}
		}
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) adminBackupRun(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{"error": "backup execution is disabled; run scripts/backup.sh or scripts/backup.ps1 under controlled operations", "code": "BACKUP_MANUAL_ONLY"})
}

func (h *Handler) adminStats(c *gin.Context) {
	if h.stats == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "stats source is not configured"})
		return
	}
	stats, err := h.stats.Stats()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to collect stats"})
		return
	}
	c.JSON(http.StatusOK, stats)
}
