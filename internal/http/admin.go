package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/backup"
	"github.com/docflow/docflow/internal/group"
	"github.com/docflow/docflow/internal/mail"
	"github.com/docflow/docflow/internal/metrics"
	"github.com/docflow/docflow/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type settingsService interface {
	GetAll() ([]settings.SettingView, error)
	Set(key string, value any, actor uuid.UUID) (any, error)
	// GetInt 为 int 键的热读取（batch.max_items 等请求路径消费方使用）；
	// *settings.Store 天然满足。
	GetInt(key string) (int, error)
	// SMTPOverrides / SetSMTP 为 SMTP 运行时配置（system_settings 的 smtp.*
	// 键；GET/PUT /admin/settings/smtp 专用，不经通用键值端点）。
	SMTPOverrides() (settings.SMTPOverride, error)
	SetSMTP(in settings.SMTPSettings, env settings.SMTPSettings, actor uuid.UUID) (settings.SMTPSettings, error)
}
type statsSource interface{ Stats() (AdminStats, error) }
type AdminStats struct {
	Users    int64 `json:"users"`
	Files    int64 `json:"files"`
	Uploads  int64 `json:"uploads"`
	Sessions int64 `json:"sessions"`
	Shares   int64 `json:"shares"`
	Tokens   int64 `json:"tokens"`
	// Spaces/Groups 为空间（未软删）与用户组计数；StorageBytes 为对象存储
	// 用量（object_blobs.size 合计，字节）——概览页统计卡片展示。
	Spaces       int64 `json:"spaces"`
	Groups       int64 `json:"groups"`
	StorageBytes int64 `json:"storage_bytes"`
}
type gormStats struct{ db *gorm.DB }

func NewAdminStats(db *gorm.DB) *gormStats { return &gormStats{db: db} }
func (g *gormStats) Stats() (AdminStats, error) {
	var s AdminStats
	// spaces 软删除（migration 040），计数排除已删空间；
	// groups 为硬删除（migration 035），直接计数。
	for _, c := range []struct {
		table string
		where string
		dst   *int64
	}{
		{"users", "", &s.Users}, {"files", "", &s.Files}, {"upload_sessions", "", &s.Uploads},
		{"sessions", "", &s.Sessions}, {"shares", "", &s.Shares}, {"api_tokens", "", &s.Tokens},
		{"spaces", "deleted_at IS NULL", &s.Spaces}, {"groups", "", &s.Groups},
	} {
		query := g.db.Table(c.table)
		if c.where != "" {
			query = query.Where(c.where)
		}
		if err := query.Count(c.dst).Error; err != nil {
			return AdminStats{}, err
		}
	}
	// 对象存储用量：全部 blob 尺寸合计（含隔离中的对象；空表归一为 0）。
	if err := g.db.Table("object_blobs").Select("COALESCE(SUM(size), 0)").Scan(&s.StorageBytes).Error; err != nil {
		return AdminStats{}, err
	}
	return s, nil
}
func (h *Handler) SetSettingsService(s settingsService) {
	if s != nil {
		h.settings = s
	}
}
func (h *Handler) SetStatsSource(s statsSource) {
	if s != nil {
		h.stats = s
		if g, ok := s.(*gormStats); ok {
			h.statsDB = g.db
		}
	}
}
func (h *Handler) SetRoleLookup(l auth.RoleLookup) {
	if l != nil {
		h.roles = l
	}
}

// SetGroups 注入管理端用户组服务（幂等）；nil 不覆盖。未注入时组端点 503。
func (h *Handler) SetGroups(svc *group.Service) {
	if svc != nil {
		h.groups = svc
	}
}

// secretEnvProbe 把「凭据状态」的语义键映射到承载它的环境变量名：
// 只列真实存在的密钥类 env（JWT/SMTP/S3/ONLYOFFICE）；CLAMAV 地址非密钥、
// DRAWIO 无凭据，均不列。状态只报 configured（env 非空），绝不回显值。
var secretEnvProbe = map[string]string{
	"jwt_secret":            "JWT_SECRET",
	"smtp_password":         "SMTP_PASS",
	"s3_secret_key":         "S3_SECRET_KEY",
	"onlyoffice_jwt_secret": "ONLYOFFICE_JWT_SECRET",
}

// secretConfiguredStatus 返回各密钥类配置的已配置状态（env 非空 = true）。
// 密钥值一律走环境变量（settings 包的非密钥原则），此处仅提供只读探针。
func secretConfiguredStatus() map[string]bool {
	out := make(map[string]bool, len(secretEnvProbe))
	for key, env := range secretEnvProbe {
		out[key] = strings.TrimSpace(os.Getenv(env)) != ""
	}
	return out
}

// mailEnvStatus 为 GET /admin/settings 附带的邮件通道只读状态：SMTP 在
// 架构上为 env-only（邮件器启动时装配，见 internal/mail 与 config 非密钥
// 原则），不提供运行时修改端点；此处仅回显非密钥连接参数与配置状态
// （SMTP_PASS 只报 password_configured，不回显值），供管理页「邮件」
// 分区展示「邮件通道由 .env 配置」的只读状态。
type mailEnvStatus struct {
	Enabled            bool   `json:"enabled"`
	Host               string `json:"host"`
	Port               int    `json:"port"`
	User               string `json:"user"`
	From               string `json:"from"`
	PasswordConfigured bool   `json:"password_configured"`
	PublicBaseURL      string `json:"public_base_url"`
}

func mailStatus() mailEnvStatus {
	enabled := false
	if v := strings.TrimSpace(os.Getenv("SMTP_ENABLED")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			enabled = b
		}
	}
	port := 587
	if v := strings.TrimSpace(os.Getenv("SMTP_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	return mailEnvStatus{
		Enabled:            enabled,
		Host:               strings.TrimSpace(os.Getenv("SMTP_HOST")),
		Port:               port,
		User:               strings.TrimSpace(os.Getenv("SMTP_USER")),
		From:               strings.TrimSpace(os.Getenv("SMTP_FROM")),
		PasswordConfigured: strings.TrimSpace(os.Getenv("SMTP_PASS")) != "",
		PublicBaseURL:      strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")),
	}
}

func (h *Handler) listAdminSettings(c *gin.Context) {
	if h.settings == nil {
		c.JSON(500, gin.H{"error": "settings service is not configured"})
		return
	}
	views, err := h.settings.GetAll()
	if err != nil {
		c.JSON(500, gin.H{"error": "unable to load settings"})
		return
	}
	c.JSON(200, gin.H{"settings": views, "secrets": secretConfiguredStatus(), "mail": mailStatus()})
}
func (h *Handler) updateAdminSetting(c *gin.Context) {
	if h.settings == nil {
		c.JSON(500, gin.H{"error": "settings service is not configured"})
		return
	}
	var req struct {
		Value any `json:"value"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "invalid request"})
		return
	}
	key := c.Param("key")
	value, err := h.settings.Set(key, req.Value, userID(c))
	switch {
	case err == nil:
		c.JSON(200, gin.H{"key": key, "value": value})
	case errors.Is(err, settings.ErrUnknownKey):
		c.JSON(404, gin.H{"error": "unknown settings key"})
	case errors.Is(err, settings.ErrInvalidType), errors.Is(err, settings.ErrInvalidValue):
		c.JSON(400, gin.H{"error": err.Error()})
	default:
		c.JSON(500, gin.H{"error": "unable to update setting"})
	}
}

// smtpEnvBaseline 从环境变量读 SMTP 基线配置（.env 部署值；system_settings
// 的 smtp.* 入库覆盖在此基础上生效）。解析逻辑与 config.Load 一致。
func smtpEnvBaseline() settings.SMTPSettings {
	enabled := false
	if v := strings.TrimSpace(os.Getenv("SMTP_ENABLED")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			enabled = b
		}
	}
	port := 587
	if v := strings.TrimSpace(os.Getenv("SMTP_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	return settings.SMTPSettings{
		Enabled: enabled,
		Host:    strings.TrimSpace(os.Getenv("SMTP_HOST")),
		Port:    port,
		User:    strings.TrimSpace(os.Getenv("SMTP_USER")),
		Pass:    strings.TrimSpace(os.Getenv("SMTP_PASS")),
		From:    strings.TrimSpace(os.Getenv("SMTP_FROM")),
		TLSMode: settings.SMTPTLSModeAuto,
	}
}

// smtpSettingsResponse 为 GET/PUT /admin/settings/smtp 的响应：生效配置
// （DB 覆盖 → env 回退）+ env 基线对照 + 密码配置状态（只报 configured，
// 永不回显值）。public_base_url 仅供邮件链接拼接参考（env-only）。
type smtpSettingsResponse struct {
	Enabled            bool   `json:"enabled"`
	Host               string `json:"host"`
	Port               int    `json:"port"`
	User               string `json:"user"`
	From               string `json:"from"`
	TLSMode            string `json:"tls_mode"`
	PasswordConfigured bool   `json:"password_configured"`
	Env                struct {
		Enabled            bool   `json:"enabled"`
		Host               string `json:"host"`
		Port               int    `json:"port"`
		User               string `json:"user"`
		From               string `json:"from"`
		PasswordConfigured bool   `json:"password_configured"`
	} `json:"env"`
	PublicBaseURL string `json:"public_base_url"`
}

func smtpSettingsView(effective, env settings.SMTPSettings, passSet, envPassSet bool) smtpSettingsResponse {
	out := smtpSettingsResponse{
		Enabled: effective.Enabled, Host: effective.Host, Port: effective.Port,
		User: effective.User, From: effective.From, TLSMode: effective.TLSMode,
		PasswordConfigured: passSet,
		PublicBaseURL:      strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")),
	}
	out.Env.Enabled = env.Enabled
	out.Env.Host = env.Host
	out.Env.Port = env.Port
	out.Env.User = env.User
	out.Env.From = env.From
	out.Env.PasswordConfigured = envPassSet
	return out
}

// getSMTPSettings 读当前生效 SMTP 配置（DB 覆盖合并 env 基线）。
func (h *Handler) getSMTPSettings(c *gin.Context) {
	if h.settings == nil {
		c.JSON(500, gin.H{"error": "settings service is not configured"})
		return
	}
	env := smtpEnvBaseline()
	effective := env
	ov, err := h.settings.SMTPOverrides()
	if err == nil {
		effective = ov.Apply(env)
	}
	c.JSON(200, smtpSettingsView(effective, env, effective.Pass != "", env.Pass != ""))
}

// putSMTPSettings 写入 SMTP 配置（保存即时生效：邮件发送处每次读库）。
// 请求 pass 留空 = 保持现值；校验失败 400。
func (h *Handler) putSMTPSettings(c *gin.Context) {
	if h.settings == nil {
		c.JSON(500, gin.H{"error": "settings service is not configured"})
		return
	}
	var req settings.SMTPSettings
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "invalid request"})
		return
	}
	env := smtpEnvBaseline()
	_, err := h.settings.SetSMTP(req, env, userID(c))
	switch {
	case err == nil:
		effective := env
		if ov, ovErr := h.settings.SMTPOverrides(); ovErr == nil {
			effective = ov.Apply(env)
		}
		c.JSON(200, smtpSettingsView(effective, env, effective.Pass != "", env.Pass != ""))
	case errors.Is(err, settings.ErrInvalidType), errors.Is(err, settings.ErrInvalidValue):
		c.JSON(400, gin.H{"error": err.Error()})
	default:
		c.JSON(500, gin.H{"error": "unable to update smtp settings"})
	}
}

// testSMTPSettings 用当前生效配置发送一封测试邮件（POST /admin/settings/smtp/test
// {to}）：读生效 SMTP 配置（DB 覆盖 → env 回退，与真实投递同源）后就地装配
// SMTPMailer 投递。请求体非法/配置缺失 400，投递失败 502（返回错误详情），
// 成功 200。管理页「发送测试邮件」按钮消费。
func (h *Handler) testSMTPSettings(c *gin.Context) {
	if h.settings == nil {
		c.JSON(500, gin.H{"error": "settings service is not configured"})
		return
	}
	var req struct {
		To string `json:"to"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "invalid request"})
		return
	}
	to := strings.TrimSpace(req.To)
	if to == "" || len(to) > 254 || !strings.Contains(to, "@") || strings.HasPrefix(to, "@") || strings.HasSuffix(to, "@") {
		c.JSON(400, gin.H{"error": "请输入有效的收件邮箱"})
		return
	}
	env := smtpEnvBaseline()
	effective := env
	if ov, err := h.settings.SMTPOverrides(); err == nil {
		effective = ov.Apply(env)
	}
	if !effective.Enabled {
		c.JSON(400, gin.H{"error": "SMTP 通道未启用（当前为日志通道）：请先在上方开启并保存配置"})
		return
	}
	if effective.Host == "" || effective.From == "" {
		c.JSON(400, gin.H{"error": "服务器地址与发件人不能为空：请先保存配置"})
		return
	}
	mailer := mail.NewSMTPMailer(effective.Host, effective.Port, effective.User, effective.Pass, effective.From, effective.TLSMode)
	body := fmt.Sprintf("这是一封 DocFlow SMTP 配置测试邮件。\n\n收到本邮件说明当前 SMTP 配置可正常投递。\n\n服务器：%s:%d（加密方式 %s）\n发件人：%s", effective.Host, effective.Port, effective.TLSMode, effective.From)
	if err := mailer.SendNotification(to, "SMTP 测试邮件", body); err != nil {
		c.JSON(502, gin.H{"error": fmt.Sprintf("发送失败：%v", err)})
		return
	}
	c.JSON(200, gin.H{"ok": true, "message": fmt.Sprintf("测试邮件已发送至 %s，请查收（记得检查垃圾箱）", to)})
}

// backupStatusResponse 为 GET /admin/backups/status 的结构化响应：
// enabled=BACKUP_DIR 是否配置；last_backup 取最近的 manifest.json（v2 目录
// 布局 docflow-backup-<ts>/，兼容 v1 顶层散落清单）；verified 读取备份目录
// 内 verify.json 标记（脚本 --verify / POST verify / cmd/backup-verify 写入，
// 未验证时为 null）；files 为清单内每文件 sha256 与大小。
type backupStatusResponse struct {
	Enabled    bool              `json:"enabled"`
	LastBackup *backupLastInfo   `json:"last_backup"`
	Verified   *bool             `json:"verified"`
	VerifiedAt string            `json:"verified_at,omitempty"`
	Files      []backupFileEntry `json:"files"`
}

// backupLastInfo 为最近备份的摘要（组件/大小/时间）。
type backupLastInfo struct {
	Name        string    `json:"name"`
	Manifest    string    `json:"manifest"`
	Timestamp   string    `json:"timestamp,omitempty"`
	ModifiedAt  time.Time `json:"modified_at"`
	Size        int64     `json:"size"`
	Components  []string  `json:"components,omitempty"`
	ObjectStore string    `json:"object_store,omitempty"`
	Encryption  string    `json:"encryption,omitempty"`
}

// backupFileEntry 为清单内的单文件条目视图。
type backupFileEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (h *Handler) backupDirectory() string {
	if h.backupDir != "" {
		return h.backupDir
	}
	return strings.TrimSpace(os.Getenv("BACKUP_DIR"))
}
func (h *Handler) adminBackupStatus(c *gin.Context) {
	dir := h.backupDirectory()
	result := backupStatusResponse{Enabled: dir != "", Files: []backupFileEntry{}}
	if dir == "" {
		c.JSON(200, result)
		return
	}
	// 目录缺失或从未备份：enabled=true、last_backup=null、files=[]。
	manifestPath, backupDir, err := backup.Latest(dir)
	if err != nil {
		c.JSON(200, result)
		return
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		c.JSON(500, gin.H{"error": "unable to read backup manifest"})
		return
	}
	var m backup.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		c.JSON(500, gin.H{"error": "unable to parse backup manifest"})
		return
	}
	info, infoErr := os.Stat(manifestPath)
	if infoErr != nil {
		c.JSON(500, gin.H{"error": "unable to stat backup manifest"})
		return
	}
	// v2 目录布局以备份目录名为标识；v1 顶层散落清单以清单文件名为标识。
	name := filepath.Base(manifestPath)
	if backupDir != dir {
		name = filepath.Base(backupDir)
	}
	var total int64
	for _, e := range m.Entries {
		total += e.Size
		result.Files = append(result.Files, backupFileEntry{Path: e.Path, Type: e.Type, SHA256: e.SHA256, Size: e.Size})
	}
	result.LastBackup = &backupLastInfo{Name: name, Manifest: manifestPath, Timestamp: m.Timestamp, ModifiedAt: info.ModTime(), Size: total, Components: m.Components, ObjectStore: m.ObjectStore, Encryption: m.Encryption}
	if marker, ok := backup.ReadVerifyMarker(backupDir); ok {
		verified := marker.Verified
		result.Verified = &verified
		result.VerifiedAt = marker.VerifiedAt
	}
	c.JSON(200, result)
}
func (h *Handler) adminBackupRun(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{"error": "backup execution is disabled because the service must not execute shell commands or receive backup encryption keys", "code": "BACKUP_MANUAL_ONLY", "command": "scripts/backup.sh or scripts/backup.ps1"})
}
func (h *Handler) adminBackupVerify(c *gin.Context) {
	dir := h.backupDirectory()
	if dir == "" {
		c.JSON(400, gin.H{"error": "BACKUP_DIR is not configured"})
		return
	}
	// 只读复核：不执行脚本、不触达数据库（backup.Verify 仅读备份文件）。
	result, err := backup.Verify(dir)
	if errors.Is(err, backup.ErrNoBackup) {
		c.JSON(404, gin.H{"error": "no backup manifest found"})
		return
	}
	ok := err == nil
	if ok {
		metrics.IncBackupVerification(metrics.BackupResultSuccess)
		metrics.SetBackupLastSuccessTimestamp(time.Now().UTC())
	} else {
		metrics.IncBackupVerification(metrics.BackupResultFailed)
	}
	// 审计 backup.verify（成功/失败均记录；ResourceID 为备份目录名）。
	actor := userID(c)
	status := audit.StatusSuccess
	if !ok {
		status = audit.StatusFailure
	}
	metadata, _ := json.Marshal(map[string]any{"manifest": result.Manifest, "files": result.Files, "errors": result.Errors})
	_ = h.audit.Record(audit.Entry{UserID: &actor, Action: audit.ActionBackupVerify, ResourceType: audit.ResourceBackup, ResourceID: filepath.Base(result.Backup), Status: status, Metadata: string(metadata)})
	// 尽力写回 verify.json 标记（BACKUP_DIR 对服务只读时忽略失败），
	// 供 GET /admin/backups/status 展示「是否验证」。
	if result.Backup != "" {
		_ = backup.WriteVerifyMarker(result.Backup, backup.VerifyMarker{Verified: result.Verified, VerifiedAt: time.Now().UTC().Format(time.RFC3339), Manifest: result.Manifest, Files: result.Files, Errors: result.Errors})
	}
	if !ok {
		c.JSON(422, result)
		return
	}
	c.JSON(200, result)
}
func (h *Handler) adminStats(c *gin.Context) {
	if h.stats == nil {
		c.JSON(500, gin.H{"error": "stats source is not configured"})
		return
	}
	s, err := h.stats.Stats()
	if err != nil {
		c.JSON(500, gin.H{"error": "unable to collect stats"})
		return
	}
	c.JSON(200, s)
}
