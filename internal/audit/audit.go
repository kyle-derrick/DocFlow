package audit

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// action 常量，对应最小写入需求。
const (
	ActionLoginSuccess   = "login_success"
	ActionLoginFailure   = "login_failure"
	ActionUploadComplete = "upload_complete"
	ActionPurge          = "purge"
	ActionShareCreate    = "share_create"
	ActionShareRevoke    = "share_revoke"
	ActionPublicDownload = "public_download"
	// 文件版本管理：新版本写入（上传覆盖）与 current_version 回滚。
	ActionVersionCreate  = "version.create"
	ActionVersionRestore = "version.restore"
	ActionVersionDelete  = "file.version.delete"
	// 系统设置与后台清理（janitor）。
	ActionSettingsUpdate = "settings.update"
	ActionJanitorUpload  = "janitor.upload"
	ActionJanitorBlob    = "janitor.blob"
	ActionJanitorTrash   = "janitor.trash"
	// ONLYOFFICE 集成：保存回调落新版本与编辑会话清理。
	ActionOnlyOfficeSave    = "onlyoffice.save"
	ActionOnlyOfficeCleanup = "onlyoffice.cleanup"
	// 邀请制认证与密码管理。
	ActionInviteCreate  = "invite.create"
	ActionInviteRevoke  = "invite.revoke"
	ActionInviteAccept  = "invite.accept"
	ActionPasswordReset = "auth.password_reset"
	// 个人访问令牌（PAT）管理：创建与撤销（PAT 认证本身不审计，噪音）。
	ActionTokenCreate = "token.create"
	ActionTokenRevoke = "token.revoke"
	// Webhook 通知渠道管理（v1.1）：注册与删除（启停不审计，噪音）。
	ActionWebhookCreate = "webhook.create"
	ActionWebhookDelete = "webhook.delete"
	// 两步验证（TOTP，v2）：启用（confirm）与禁用；登录第二段沿用
	// login_success/login_failure，不另设 action。
	ActionTOTPEnable  = "totp.enable"
	ActionTOTPDisable = "totp.disable"
	// OIDC 单点登录（v2）：自动开户（SSO 登录成功/失败沿用
	// login_success/login_failure，metadata.stage=oidc）。
	ActionOIDCProvision = "oidc.provision"
	// 管理端用户管理（C6，设计 6.2.1/9.1.1）：更新（禁用/启用/改配额/改角色）
	// 与重置密码；软禁用替代删除（数据完整性取舍）。
	ActionFileCopy          = "file.copy"
	ActionUserUpdate        = "user.update"
	ActionUserResetPassword = "user.reset_password"
	// 备份管理：POST /admin/backups/verify 的只读 sha256 复核（成功/失败均记录；
	// 备份执行在服务进程外，run 端点恒 501 不审计）。
	ActionBackupVerify = "backup.verify"
	// 隔离区管理（仅 admin，POST /admin/quarantine/:sha256/action）：
	// 重扫 / 解除隔离（release 须显式 confirm）/ 删除（解除引用并删对象）。
	ActionQuarantineRescan  = "quarantine.rescan"
	ActionQuarantineRelease = "quarantine.release"
	ActionQuarantineDelete  = "quarantine.delete"
	// 用户组管理（仅 admin，/admin/groups）：组 CRUD 与成员增删
	//（migration 035；组为纯组织维度，删除不级联影响用户）。
	ActionGroupCreate       = "group.create"
	ActionGroupUpdate       = "group.update"
	ActionGroupDelete       = "group.delete"
	ActionGroupMemberAdd    = "group.member.add"
	ActionGroupMemberRemove = "group.member.remove"
)

// resource_type 常量。
const (
	ResourceSession = "session"
	ResourceUpload  = "upload"
	ResourceFile    = "file"
	ResourceShare   = "share"
	// ResourceFolder 为目录（路径级 ACL 管理等目录维度操作）。
	ResourceFolder = "folder"
	// ResourceSettings 系统设置键；ResourceBlob 为 object_blobs 行。
	ResourceSettings = "settings"
	ResourceBlob     = "blob"
	// ResourceInvitation 邀请（invitations 行）；ResourceUser 为 users 行。
	ResourceInvitation = "invitation"
	ResourceUser       = "user"
	// ResourceToken 为个人访问令牌（api_tokens 行）。
	ResourceToken = "token"
	// ResourceWebhook 为出站 webhook（webhooks 行）。
	ResourceWebhook = "webhook"
	// ResourceBackup 为备份产物（BACKUP_DIR 下的 docflow-backup-<ts> 目录）。
	ResourceBackup = "backup"
	// ResourceGroup 为用户组（groups 行，migration 035）。
	ResourceGroup = "group"
)

// status 常量。
const (
	StatusSuccess = "success"
	StatusFailure = "failure"
)

// Entry 表示一条审计日志（对应 audit_logs 表）。
// json tag 必须齐全：缺少时 Go 按字段名输出帕斯卡命名（CreatedAt 等），
// 前端按 snake_case 读取得到 undefined（审计页时间列全显 Invalid Date）。
type Entry struct {
	ID           int64      `gorm:"primaryKey" json:"id"`
	UserID       *uuid.UUID `gorm:"type:uuid" json:"user_id"`
	Action       string     `gorm:"size:64;not null" json:"action"`
	ResourceType string     `gorm:"size:32" json:"resource_type"`
	ResourceID   string     `gorm:"size:64" json:"resource_id"`
	IP           *string    `gorm:"size:45" json:"ip"`
	UserAgent    string     `gorm:"size:512" json:"user_agent"`
	Status       string     `gorm:"size:16" json:"status"`
	Metadata     string     `gorm:"type:jsonb" json:"metadata"`
	CreatedAt    time.Time  `gorm:"not null" json:"created_at"`
}

// TableName 显式映射到 audit_logs（gorm 默认复数化为 entries）。
func (Entry) TableName() string { return "audit_logs" }

// Store 提供最小写入能力；查询接口不在本里程碑范围。
type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) Record(e Entry) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	// metadata 列为 jsonb：空串不是合法 JSON（PostgreSQL 22P02），统一落
	// "{}"；调用方传入的必须已是合法 JSON 串。
	if e.Metadata == "" {
		e.Metadata = "{}"
	}
	return s.db.Create(&e).Error
}

// Recorder 抽象最小写入接口，便于测试注入内存实现。
type Recorder interface {
	Record(Entry) error
}

// NopRecorder 供审计未启用/测试时使用。
type NopRecorder struct{}

func (NopRecorder) Record(Entry) error { return nil }
