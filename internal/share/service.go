package share

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

var (
	ErrInvalidPermission   = errors.New("invalid permission")
	ErrInvalidExpiry       = errors.New("invalid expires_in")
	ErrInvalidMaxDownloads = errors.New("invalid max_downloads")
	ErrInvalidVisibility   = errors.New("invalid visibility")
	ErrPublicDisabled      = errors.New("public sharing is disabled")
	ErrNotFound            = errors.New("share not found")
	// ErrGone 表示分享已失效（过期、撤销或达到下载上限），公开访问应返回 410。
	ErrGone              = errors.New("share is no longer available")
	ErrFileNotFound      = errors.New("file not found")
	ErrFileNotShareable  = errors.New("file cannot be shared")
	ErrDownloadForbidden = errors.New("share does not allow download")
	ErrFileNotAvailable  = errors.New("file is not available for download")
	ErrDownloadLimit     = errors.New("download limit exceeded")
	// ErrForbidden 表示当前用户未被授权访问该私有分享（HTTP 层映射 403）。
	ErrForbidden = errors.New("access to share denied")
	// ErrInvalidPassword 表示创建公开分享时密码长度非法（须 4-64 字符）。
	ErrInvalidPassword = errors.New("invalid password")
	// ErrInvalidCredentials 表示分享密码校验未通过（HTTP 层映射 401）。
	ErrInvalidCredentials = errors.New("invalid share password")
	// ErrPasswordNotSet 表示分享未启用密码保护（verify 端点不应被调用）。
	ErrPasswordNotSet = errors.New("share does not require a password")
	// ErrInvalidWatermarkText 表示自定义水印模板超长（> MaxWatermarkTextLen）。
	ErrInvalidWatermarkText = errors.New("invalid watermark text")
)

// NewToken 生成明文分享 token：32 字节随机数的 URL-safe base64（43 字符）。
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken 返回 token 的 SHA-256 十六进制小写哈希（64 字符），对应 shares.token_hash CHAR(64)。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// shareSessionTTL 为密码校验通过后公开访问会话的有效期（1 小时）。
const shareSessionTTL = time.Hour

// HashSharePassword 计算 SHA-256(password || share_id) 的十六进制小写哈希
// （64 字符），对应 shares.password_hash CHAR(64)：share_id 充当盐，
// 防止同密码跨分享的彩虹表比对；明文密码不落库。
func HashSharePassword(shareID uuid.UUID, password string) string {
	buf := append([]byte(password), shareID.String()...)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// verifySharePassword 以常数时间比较提交密码与存储哈希（防时序侧信道）。
func verifySharePassword(shareID uuid.UUID, password, storedHash string) bool {
	got := HashSharePassword(shareID, password)
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}

// hashAccessSession 计算 SHA-256(share_id || random) 的十六进制小写哈希，
// 对应 share_access_sessions.session_hash；明文随机值（cookie 值）不落库。
func hashAccessSession(shareID uuid.UUID, value string) string {
	buf := append([]byte(shareID.String()), value...)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// IPPrefix 返回展示用脱敏 IP 前缀：IPv4 取前两组 + ".*"（如 203.0.*），
// IPv6 取前两组 + ":*"，无法解析时返回空串。水印 {email}/{ip} 占位符与
// 访问统计 recent 记录共用。
func IPPrefix(ip string) string {
	if ip == "" {
		return ""
	}
	if parts := strings.Split(ip, "."); len(parts) == 4 {
		return parts[0] + "." + parts[1] + ".*"
	}
	if parts := strings.Split(ip, ":"); len(parts) >= 2 {
		return parts[0] + ":" + parts[1] + ":*"
	}
	return ""
}

// RenderWatermark 渲染公开访问水印文案：替换 {email}/{ip} → 脱敏 IP 前缀
// （公开访问无登录身份）、{date} → 当地日期、{name} → 文件名；未知占位符
// 原样保留。模板为空时回退 DefaultWatermarkTemplate。
func RenderWatermark(template, fileName, ip string, now time.Time) string {
	if template == "" {
		template = DefaultWatermarkTemplate
	}
	prefix := IPPrefix(ip)
	if prefix == "" {
		prefix = "unknown"
	}
	return strings.NewReplacer(
		"{email}", prefix,
		"{ip}", prefix,
		"{date}", now.Format("2006-01-02"),
		"{name}", fileName,
	).Replace(template)
}

// validateWatermarkText 校验自定义水印模板：1..MaxWatermarkTextLen 个 rune
// 且不含控制字符。
func validateWatermarkText(text string) error {
	if text == "" {
		return nil
	}
	n := 0
	for _, r := range text {
		if r < 0x20 || r == 0x7f {
			return ErrInvalidWatermarkText
		}
		n++
	}
	if n > MaxWatermarkTextLen {
		return ErrInvalidWatermarkText
	}
	return nil
}

// validateSharePassword 校验公开分享密码长度（4-64 字节）。
func validateSharePassword(password string) error {
	if n := len(password); n < 4 || n > 64 {
		return ErrInvalidPassword
	}
	return nil
}

// FileSource 抽象分享服务对文件元数据的访问与下载计数；owner 隔离由实现保证。
type FileSource interface {
	Get(owner, id uuid.UUID) (files.File, error)
	CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
	IncrementDownloadCount(owner, fileID uuid.UUID) error
	IncrementViewCount(owner, fileID uuid.UUID) error
}

var _ FileSource = (*files.Store)(nil)

// Repo 是分享持久化接口；GormStore 为 PostgreSQL 实现，MemoryStore 供测试使用。
type Repo interface {
	Create(v Share) error
	Get(id uuid.UUID) (Share, error)
	GetByOwner(owner, id uuid.UUID) (Share, error)
	GetByTokenHash(hash string) (Share, error)
	ListByOwner(owner uuid.UUID, limit int) ([]Share, error)
	// ListSharedWithUser 返回分享给 user 的有效私有分享（share_users 显式授权，
	// 或 user 属于 share_teams 任一授权团队的成员；公开分享不含），
	// 按 created_at 倒序。团队判定由实现方实时完成（GormStore JOIN team_members）。
	ListSharedWithUser(user uuid.UUID, now time.Time, limit int) ([]Share, error)
	Revoke(id uuid.UUID, now time.Time) error
	// ConsumeDownload 在分享仍有效（未撤销、未过期、未达下载上限）时原子递增 download_count，
	// 返回是否成功；失败时由调用方重新读取以区分原因。
	ConsumeDownload(id uuid.UUID, now time.Time) (bool, error)
	// DecrementDownload 补偿回退一次已消耗的下载计数（内容读取失败时调用）：
	// shares.download_count 与关联 files.download_count 同步 -1，条件
	// download_count > 0 保证不为负；分享不存在或计数为 0 时静默成功。
	DecrementDownload(id uuid.UUID) error
	// CountActiveByFile 统计文件的有效「公开」分享数（is_public 辅助字段只反映公开分享）。
	CountActiveByFile(fileID uuid.UUID, now time.Time) (int64, error)
	// SetFilePublic 维护 files.is_public 冗余辅助字段。
	SetFilePublic(fileID uuid.UUID, value bool) error
	// 私有分享显式授权（share_users / share_teams，见 migrations/008_teams_shares.sql）。
	AddShareUsers(shareID uuid.UUID, userIDs []uuid.UUID, now time.Time) error
	AddShareTeams(shareID uuid.UUID, teamIDs []uuid.UUID, now time.Time) error
	ListShareUserIDs(shareID uuid.UUID) ([]uuid.UUID, error)
	ListShareTeamIDs(shareID uuid.UUID) ([]uuid.UUID, error)
	// CreateSession / GetSessionByHash 维护公开访问会话（share_access_sessions，
	// migration 022）：密码校验通过后写入，后续公开访问按 cookie 值哈希校验。
	CreateSession(v AccessSession) error
	GetSessionByHash(hash string) (AccessSession, error)
	// UpdateFields 按列名更新分享行；仅允许 expires_at / max_downloads /
	// watermark_enabled / watermark_text（服务层已解析为最终值），
	// 值为 nil 表示清空为 NULL。行不存在返回 ErrNotFound。
	UpdateFields(id uuid.UUID, fields map[string]any) error
	// RecordAccessEvent 写入公开访问事件（file_access_events，migration 023）。
	RecordAccessEvent(e AccessEvent) error
	// ShareAccessStats 聚合分享访问统计：事件总数、独立访客（distinct
	// ip_hash）与最近 20 条事件（created_at 倒序）。
	ShareAccessStats(shareID uuid.UUID) (AccessStats, error)
}

// shareActive 判断分享在 now 时刻是否仍有效。
func shareActive(sh Share, now time.Time) bool {
	if sh.RevokedAt != nil {
		return false
	}
	if sh.ExpiresAt != nil && !now.Before(*sh.ExpiresAt) {
		return false
	}
	if sh.MaxDownloads != nil && sh.DownloadCount >= *sh.MaxDownloads {
		return false
	}
	return true
}

// TeamMembership 实时判定用户是否属于给定团队之一（由 team 包实现，
// JOIN team_members 查询；未来可替换为 Casbin 等策略引擎适配器）。
type TeamMembership interface {
	UserInAnyTeam(userID uuid.UUID, teamIDs []uuid.UUID) (bool, error)
}

// UserDirectory 按用户 ID 解析用户名（auth.UserStore 实现），
// 供「与我共享」列表展示分享者；不暴露 email 等敏感字段。
type UserDirectory interface {
	Username(id uuid.UUID) (string, error)
}

// Service 提供分享的创建、撤销、列表、公开 token 解析与私有分享授权访问。
type Service struct {
	repo       Repo
	files      FileSource
	membership TeamMembership
	directory  UserDirectory
	// teamSharer 团队文件分享门控（main 注入 team.CanShare）：nil 时团队文件
	// 分享一律拒绝（fail closed），个人文件分享不受影响。
	teamSharer TeamSharer
	// defaultExpiryHours 为「创建请求未指定有效期」时的默认时长（小时）
	// 热读取（system_settings 的 share.default_expiry_hours，main 注入）；
	// nil 或返回非正值时回退既有行为（不设默认，即永久）。
	defaultExpiryHours func() int
	// watermarkDefaults 为「创建请求未显式指定水印开关/模板」时的默认值
	// 热读取（system_settings 的 share.default_watermark / share.watermark_text，
	// main 注入）；nil 时回退内置默认（开启 + DefaultWatermarkTemplate）。
	watermarkDefaults func() (enabled bool, text string)
	publicEnabled     func() bool
	// notifyDispatcher 站内通知回调（main 注入 notify.Dispatcher 适配器）：
	// 公开/私有分享下载成功（计数已消耗）后通知分享 owner share.accessed。
	// 私有分享 owner 本人下载不通知（避免噪音）。
	notifyDispatcher NotifyFunc
	now              func() time.Time
}

// NotifyFunc 站内通知回调签名（main 注入 notify 包 Dispatcher 的适配器；
// share 包不依赖 notify 以避免环）：resourceID 为 uuid.Nil 表示无关联资源。
type NotifyFunc func(userID uuid.UUID, eventType, title, body string, resourceID uuid.UUID)

// SetNotifyDispatcher 注入站内通知回调（幂等）：公开/私有分享下载成功时
// 通知分享 owner（share.accessed，标题含文件名）。回调不影响下载流程。
func (s *Service) SetNotifyDispatcher(fn NotifyFunc) {
	if fn != nil {
		s.notifyDispatcher = fn
	}
}

// notifyAccessed 尽力通知分享 owner（回调未注入或 owner 本人下载均跳过）。
func (s *Service) notifyAccessed(r Resolved, downloader uuid.UUID) {
	if s.notifyDispatcher == nil || r.Share.OwnerID == downloader {
		return
	}
	visText := "公开分享"
	if r.Share.Visibility == VisibilityPrivate {
		visText = "私有分享"
	}
	s.notifyDispatcher(r.Share.OwnerID, "share.accessed", "分享文件被下载："+r.File.Name,
		"你的文件「"+r.File.Name+"」通过"+visText+"被下载。", r.Share.FileID)
}

func NewService(repo Repo, source FileSource) *Service {
	return &Service{repo: repo, files: source, now: time.Now}
}

// SetDefaultExpiryProvider 注入默认有效期（小时）热读取：Create/CreatePrivate
// 收到 expiresIn==0（缺省或显式 0）时采用该默认；返回 0（未设置/读失败）
// 维持既有「永久」语义。幂等（nil 不覆盖）。
func (s *Service) SetPublicEnabledProvider(fn func() bool) {
	if fn != nil {
		s.publicEnabled = fn
	}
}

func (s *Service) SetDefaultExpiryProvider(fn func() int) {
	if fn != nil {
		s.defaultExpiryHours = fn
	}
}

// defaultExpiry 返回默认有效期时长；无 provider 或非正值时为 0（永久）。
func (s *Service) defaultExpiry() time.Duration {
	if s.defaultExpiryHours == nil {
		return 0
	}
	if n := s.defaultExpiryHours(); n > 0 {
		return time.Duration(n) * time.Hour
	}
	return 0
}

// SetWatermarkDefaultsProvider 注入水印默认值热读取（幂等）：Create /
// CreatePublic / CreatePrivate 收到未显式指定的水印开关或模板时采用该默认；
// 未注入或返回空文本时回退内置默认（开启 + DefaultWatermarkTemplate）。
func (s *Service) SetWatermarkDefaultsProvider(fn func() (bool, string)) {
	if fn != nil {
		s.watermarkDefaults = fn
	}
}

// resolveWatermark 解析创建请求的水印字段：显式值优先，缺省采用 provider
// 默认（再回退内置默认）；模板非法（超长/含控制字符）返回错误。
func (s *Service) resolveWatermark(enabled *bool, text *string) (bool, string, error) {
	defEnabled, defText := true, DefaultWatermarkTemplate
	if s.watermarkDefaults != nil {
		if e, t := s.watermarkDefaults(); t != "" {
			defEnabled, defText = e, t
		}
	}
	outEnabled := defEnabled
	if enabled != nil {
		outEnabled = *enabled
	}
	outText := defText
	if text != nil && *text != "" {
		outText = *text
	}
	if err := validateWatermarkText(outText); err != nil {
		return false, "", err
	}
	return outEnabled, outText, nil
}

// SetTeamMembership 注入团队成员关系判定器；未注入时 share_teams 授权不可达（安全默认拒绝）。
func (s *Service) SetTeamMembership(m TeamMembership) {
	if m != nil {
		s.membership = m
	}
}

// TeamSharer 判定用户能否为团队文件创建分享（team 包 CanShare 注入实现：
// 系统角色 owner/editor、自定义角色按 share 勾选且未被 deny）。
type TeamSharer func(userID, teamID uuid.UUID) (bool, error)

// SetTeamSharer 注入团队文件分享门控（设计 6.5.5；幂等）：
// 未注入时团队文件分享一律拒绝（fail closed），个人文件不受影响。
func (s *Service) SetTeamSharer(fn TeamSharer) {
	if fn != nil {
		s.teamSharer = fn
	}
}

// authorizeShare 团队文件分享门控：仅 CanShare（系统 owner/editor 或含 share
// 权限的自定义角色）可创建分享；个人文件不经过本判定（Get 已校验 owner）。
func (s *Service) authorizeShare(f files.File, user uuid.UUID) error {
	if f.ScopeType != "team" || f.TeamID == nil {
		return nil
	}
	if s.teamSharer == nil {
		return ErrForbidden
	}
	ok, err := s.teamSharer(user, *f.TeamID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// SetUserDirectory 注入用户目录解析器（幂等）。
func (s *Service) SetUserDirectory(d UserDirectory) {
	if d != nil {
		s.directory = d
	}
}

// Resolved 是公开访问的解析结果：分享 + 文件 + 当前版本 + 对象元数据。
// 对外序列化时不得包含 owner、storage key 等内部信息。
type Resolved struct {
	Share   Share
	File    files.File
	Version files.FileVersion
	Blob    files.ObjectBlob
}

// ShareOptions 聚合创建分享的可选字段（密码保护与水印，见设计 6.6.2/6.17）。
// Password 仅对公开分享生效（4-64 字符，存 SHA-256(password||id) 哈希，
// 明文不落库）；私有分享不受密码影响，显式传入返回 ErrInvalidPassword。
// WatermarkEnabled / WatermarkText 未指定（nil）时采用水印默认值热读取。
type ShareOptions struct {
	Password         string
	WatermarkEnabled *bool
	WatermarkText    *string
}

// Create 为 owner 名下未删除、当前版本对象为 available 的文件创建公开分享，
// 返回分享记录与明文 token（仅此一次可见，数据库只保存哈希）。
func (s *Service) Create(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int) (Share, string, error) {
	return s.CreatePublic(owner, fileID, permission, expiresIn, maxDownloads, ShareOptions{})
}

// CreatePublic 在 Create 基础上支持可选的密码保护与水印字段（ShareOptions）。
// 团队文件须经 CanShare 门控（设计 6.5.5：owner/editor/含 share 权限的自定义角色）。
func (s *Service) CreatePublic(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int, opts ShareOptions) (Share, string, error) {
	if s.publicEnabled != nil && !s.publicEnabled() {
		return Share{}, "", ErrPublicDisabled
	}
	if permission != PermissionView && permission != PermissionDownload {
		return Share{}, "", ErrInvalidPermission
	}
	if expiresIn < 0 {
		return Share{}, "", ErrInvalidExpiry
	}
	// 未指定有效期（0）时采用热读取默认（share.default_expiry_hours），
	// 未注入/读失败回退 0（永久）。
	if expiresIn == 0 {
		expiresIn = s.defaultExpiry()
	}
	if maxDownloads != nil && *maxDownloads < 0 {
		return Share{}, "", ErrInvalidMaxDownloads
	}
	if opts.Password != "" {
		if err := validateSharePassword(opts.Password); err != nil {
			return Share{}, "", err
		}
	}
	watermarkEnabled, watermarkText, err := s.resolveWatermark(opts.WatermarkEnabled, opts.WatermarkText)
	if err != nil {
		return Share{}, "", err
	}
	f, err := s.files.Get(owner, fileID)
	if err != nil {
		return Share{}, "", ErrFileNotFound
	}
	if err := s.authorizeShare(f, owner); err != nil {
		return Share{}, "", err
	}
	if f.Type != "file" {
		return Share{}, "", ErrFileNotShareable
	}
	_, blob, err := s.files.CurrentVersion(owner, fileID)
	if err != nil {
		return Share{}, "", ErrFileNotShareable
	}
	if blob.Status != files.BlobStatusAvailable {
		return Share{}, "", ErrFileNotShareable
	}
	token, err := NewToken()
	if err != nil {
		return Share{}, "", err
	}
	now := s.now()
	sh := Share{ID: uuid.New(), OwnerID: owner, FileID: fileID, TokenHash: HashToken(token), Visibility: VisibilityPublic, Permission: permission, MaxDownloads: maxDownloads, WatermarkEnabled: watermarkEnabled, WatermarkText: &watermarkText, CreatedAt: now}
	if opts.Password != "" {
		// 密码哈希依赖 share_id 作盐，须在生成 ID 后计算。
		sh.PasswordHash = HashSharePassword(sh.ID, opts.Password)
	}
	if expiresIn > 0 {
		expiresAt := now.Add(expiresIn)
		sh.ExpiresAt = &expiresAt
	}
	if err := s.repo.Create(sh); err != nil {
		return Share{}, "", err
	}
	// is_public 为冗余辅助字段，更新失败不影响分享本身。
	_ = s.repo.SetFilePublic(fileID, true)
	return sh, token, nil
}

// CreatePrivate 为 owner 名下文件创建私有分享：不生成公开 token（token_hash 为空），
// 访问仅限 share_users 显式授权用户与 share_teams 授权团队的成员。
// userIds/teamIds 自动去重；私有分享不受密码影响（显式传入密码返回错误）。
func (s *Service) CreatePrivate(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int, userIds, teamIds []uuid.UUID) (Share, error) {
	return s.CreatePrivateWithOptions(owner, fileID, permission, expiresIn, maxDownloads, userIds, teamIds, ShareOptions{})
}

// CreatePrivateWithOptions 在 CreatePrivate 基础上支持水印字段；
// 密码仅适用于公开分享，显式传入返回 ErrInvalidPassword。
// 团队文件须经 CanShare 门控（同 CreatePublic）。
func (s *Service) CreatePrivateWithOptions(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int, userIds, teamIds []uuid.UUID, opts ShareOptions) (Share, error) {
	if permission != PermissionView && permission != PermissionDownload {
		return Share{}, ErrInvalidPermission
	}
	if opts.Password != "" {
		// 设计 6.6.2：密码保护面向公开链接；私有分享已有显式授权，不叠加密码。
		return Share{}, ErrInvalidPassword
	}
	if expiresIn < 0 {
		return Share{}, ErrInvalidExpiry
	}
	// 同 Create：未指定有效期（0）时采用热读取默认，回退 0（永久）。
	if expiresIn == 0 {
		expiresIn = s.defaultExpiry()
	}
	if maxDownloads != nil && *maxDownloads < 0 {
		return Share{}, ErrInvalidMaxDownloads
	}
	watermarkEnabled, watermarkText, err := s.resolveWatermark(opts.WatermarkEnabled, opts.WatermarkText)
	if err != nil {
		return Share{}, err
	}
	f, err := s.files.Get(owner, fileID)
	if err != nil {
		return Share{}, ErrFileNotFound
	}
	if err := s.authorizeShare(f, owner); err != nil {
		return Share{}, err
	}
	if f.Type != "file" {
		return Share{}, ErrFileNotShareable
	}
	_, blob, err := s.files.CurrentVersion(owner, fileID)
	if err != nil {
		return Share{}, ErrFileNotShareable
	}
	if blob.Status != files.BlobStatusAvailable {
		return Share{}, ErrFileNotShareable
	}
	now := s.now()
	sh := Share{ID: uuid.New(), OwnerID: owner, FileID: fileID, Visibility: VisibilityPrivate, Permission: permission, MaxDownloads: maxDownloads, WatermarkEnabled: watermarkEnabled, WatermarkText: &watermarkText, CreatedAt: now}
	if expiresIn > 0 {
		expiresAt := now.Add(expiresIn)
		sh.ExpiresAt = &expiresAt
	}
	if err := s.repo.Create(sh); err != nil {
		return Share{}, err
	}
	if ids := dedupeIDs(userIds); len(ids) > 0 {
		if err := s.repo.AddShareUsers(sh.ID, ids, now); err != nil {
			return Share{}, err
		}
	}
	if ids := dedupeIDs(teamIds); len(ids) > 0 {
		if err := s.repo.AddShareTeams(sh.ID, ids, now); err != nil {
			return Share{}, err
		}
	}
	return sh, nil
}

// dedupeIDs 去重并过滤零值 ID。
func dedupeIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// CanAccess 判定用户能否访问分享：分享 owner、share_users 显式授权用户，
// 或属于 share_teams 任一团队的成员（实时 JOIN team_members 判定，成员变动立即生效）。
// 公开分享不走此入口（按 token 解析），非 owner 一律无显式授权记录故返回 false。
func (s *Service) CanAccess(sh Share, user uuid.UUID) bool {
	if user == uuid.Nil {
		return false
	}
	if sh.OwnerID == user {
		return true
	}
	if sh.Visibility != VisibilityPrivate {
		return false
	}
	if userIDs, err := s.repo.ListShareUserIDs(sh.ID); err == nil {
		for _, id := range userIDs {
			if id == user {
				return true
			}
		}
	}
	teamIDs, err := s.repo.ListShareTeamIDs(sh.ID)
	if err != nil || len(teamIDs) == 0 || s.membership == nil {
		return false
	}
	ok, err := s.membership.UserInAnyTeam(user, teamIDs)
	return err == nil && ok
}

// ResolveForUser 私有分享访问入口（登录用户）：按分享 ID + 文件 ID 解析，
// 先做授权判定（owner 或 CanAccess），再校验分享有效性与文件可用性。
// 分享不存在、文件不匹配、文件已删除统一返回 ErrNotFound/ErrGone，避免泄露；
// 公开分享不走用户入口（按 token 解析）：非 owner 访问一律 ErrNotFound
// （对外呈现「不存在」，不泄露分享可枚举性，HTTP 层映射 404 而非 403）。
func (s *Service) ResolveForUser(shareID, fileID, user uuid.UUID) (Resolved, error) {
	sh, err := s.repo.Get(shareID)
	if err != nil {
		return Resolved{}, ErrNotFound
	}
	if sh.FileID != fileID {
		return Resolved{}, ErrNotFound
	}
	if !s.CanAccess(sh, user) {
		if sh.Visibility != VisibilityPrivate {
			return Resolved{}, ErrNotFound
		}
		return Resolved{}, ErrForbidden
	}
	if !shareActive(sh, s.now()) {
		return Resolved{}, ErrGone
	}
	f, err := s.files.Get(sh.OwnerID, sh.FileID)
	if err != nil {
		return Resolved{}, ErrGone
	}
	version, blob, err := s.files.CurrentVersion(sh.OwnerID, sh.FileID)
	if err != nil {
		return Resolved{}, ErrGone
	}
	return Resolved{Share: sh, File: f, Version: version, Blob: blob}, nil
}

// ResolveForUserForPreview 在 ResolveForUser 基础上校验对象可用性（view/download 均可预览）。
func (s *Service) ResolveForUserForPreview(shareID, fileID, user uuid.UUID) (Resolved, error) {
	r, err := s.ResolveForUser(shareID, fileID, user)
	if err != nil {
		return Resolved{}, err
	}
	if r.Blob.Status != files.BlobStatusAvailable {
		return Resolved{}, ErrFileNotAvailable
	}
	return r, nil
}

// ResolveForUserForDownload 在 ResolveForUser 基础上校验下载权限与对象可用性，
// 并原子递增 shares.download_count 与 files.download_count。
func (s *Service) ResolveForUserForDownload(shareID, fileID, user uuid.UUID) (Resolved, error) {
	r, err := s.ResolveForUser(shareID, fileID, user)
	if err != nil {
		return Resolved{}, err
	}
	if r.Share.Permission != PermissionDownload {
		return Resolved{}, ErrDownloadForbidden
	}
	if r.Blob.Status != files.BlobStatusAvailable {
		return Resolved{}, ErrFileNotAvailable
	}
	consumed, err := s.repo.ConsumeDownload(r.Share.ID, s.now())
	if err != nil {
		return Resolved{}, err
	}
	if !consumed {
		latest, gerr := s.repo.Get(r.Share.ID)
		if gerr != nil {
			return Resolved{}, ErrNotFound
		}
		if latest.MaxDownloads != nil && latest.DownloadCount >= *latest.MaxDownloads {
			return Resolved{}, ErrDownloadLimit
		}
		return Resolved{}, ErrGone
	}
	if err := s.files.IncrementDownloadCount(r.Share.OwnerID, r.Share.FileID); err != nil {
		return Resolved{}, err
	}
	// 下载成功（计数已消耗）：尽力通知分享 owner（owner 本人下载跳过）。
	s.notifyAccessed(r, user)
	return r, nil
}

// DecrementDownload 补偿回退一次已消耗的下载计数：ResolveForDownload /
// ResolveForUserForDownload 已原子递增计数、但调用方读取存储内容失败
// （未能向用户交付文件）时调用；shares.download_count 与 files.download_count
// 同步 -1（下限 0）。补偿失败由调用方忽略——计数偏保守多记一次，不影响安全性。
func (s *Service) DecrementDownload(r Resolved) error {
	return s.repo.DecrementDownload(r.Share.ID)
}

// Revoke 撤销 owner 名下的分享（幂等）；文件再无其他有效分享时清除 files.is_public。
func (s *Service) Revoke(owner, shareID uuid.UUID) (Share, error) {
	sh, err := s.repo.GetByOwner(owner, shareID)
	if err != nil {
		return Share{}, err
	}
	if sh.RevokedAt == nil {
		if err := s.repo.Revoke(sh.ID, s.now()); err != nil {
			return Share{}, err
		}
		revokedAt := s.now()
		sh.RevokedAt = &revokedAt
	}
	if active, err := s.repo.CountActiveByFile(sh.FileID, s.now()); err == nil && active == 0 {
		_ = s.repo.SetFilePublic(sh.FileID, false)
	}
	return sh, nil
}

// List 返回 owner 的分享列表（created_at 倒序）。
func (s *Service) List(owner uuid.UUID, limit int) ([]Share, error) {
	return s.repo.ListByOwner(owner, limit)
}

// ShareWithFile 是我的分享列表条目：分享记录 + 关联文件名
// （文件已删除或不可读时 FileName 为空串）。
type ShareWithFile struct {
	Share    Share
	FileName string
}

// ListWithFileNames 返回 owner 的分享列表并附关联文件名（created_at 倒序）。
// 文件元数据按分享 owner 读取（Create 时已校验归属），读取失败不阻断列表。
func (s *Service) ListWithFileNames(owner uuid.UUID, limit int) ([]ShareWithFile, error) {
	shares, err := s.repo.ListByOwner(owner, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ShareWithFile, 0, len(shares))
	for _, sh := range shares {
		item := ShareWithFile{Share: sh}
		if f, ferr := s.files.Get(sh.OwnerID, sh.FileID); ferr == nil {
			item.FileName = f.Name
		}
		out = append(out, item)
	}
	return out, nil
}

// SharedWithMeItem 是「与我共享」列表条目：有效私有分享 + 文件元数据 + 分享者用户名。
type SharedWithMeItem struct {
	Share     Share
	FileName  string
	FileSize  int64
	FileMime  string
	OwnerName string
}

// SharedWithMe 返回分享给 user 的有效私有分享列表（created_at 倒序）：
// share_users 显式授权或 share_teams 团队成员命中（repo 实时判定），
// 公开分享、已撤销、已过期、达下载上限的分享，以及文件已删除/不可用的条目不含。
// OwnerName 经 UserDirectory 解析；未注入或解析失败时为空串，不影响列表。
func (s *Service) SharedWithMe(user uuid.UUID, limit int) ([]SharedWithMeItem, error) {
	if limit <= 0 {
		limit = 100
	}
	shares, err := s.repo.ListSharedWithUser(user, s.now(), limit)
	if err != nil {
		return nil, err
	}
	out := make([]SharedWithMeItem, 0, len(shares))
	for _, sh := range shares {
		f, ferr := s.files.Get(sh.OwnerID, sh.FileID)
		if ferr != nil {
			continue
		}
		item := SharedWithMeItem{Share: sh, FileName: f.Name}
		if _, blob, verr := s.files.CurrentVersion(sh.OwnerID, sh.FileID); verr == nil {
			item.FileSize = blob.Size
			item.FileMime = blob.MimeType
		}
		if s.directory != nil {
			if name, derr := s.directory.Username(sh.OwnerID); derr == nil {
				item.OwnerName = name
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// Resolve 通过明文 token 解析有效分享（未过期、未撤销、未达下载上限），返回 share+file+blob。
// 文件已被删除或当前版本缺失时同样视为失效。
func (s *Service) Resolve(token string) (Resolved, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Resolved{}, ErrNotFound
	}
	sh, err := s.repo.GetByTokenHash(HashToken(token))
	if err != nil {
		return Resolved{}, ErrNotFound
	}
	if !shareActive(sh, s.now()) {
		return Resolved{}, ErrGone
	}
	f, err := s.files.Get(sh.OwnerID, sh.FileID)
	if err != nil {
		return Resolved{}, ErrGone
	}
	version, blob, err := s.files.CurrentVersion(sh.OwnerID, sh.FileID)
	if err != nil {
		return Resolved{}, ErrGone
	}
	return Resolved{Share: sh, File: f, Version: version, Blob: blob}, nil
}

// ResolveForPreview 在 Resolve 基础上校验对象可用性；view 与 download 权限均允许预览，
// 不消耗分享 download_count。view_count 由调用方在类型白名单通过、确定输出预览后
// 通过 IncrementPreviewView 递增，避免不可预览的请求虚增计数。
func (s *Service) ResolveForPreview(token string) (Resolved, error) {
	r, err := s.Resolve(token)
	if err != nil {
		return Resolved{}, err
	}
	if r.Blob.Status != files.BlobStatusAvailable {
		return Resolved{}, ErrFileNotAvailable
	}
	return r, nil
}

// IncrementPreviewView 原子递增分享所属文件的 view_count（与 ResolveForPreview 搭配使用）。
func (s *Service) IncrementPreviewView(r Resolved) error {
	return s.files.IncrementViewCount(r.Share.OwnerID, r.File.ID)
}

// ResolveForDownload 在 Resolve 基础上校验下载权限与对象可用性，
// 并原子递增 shares.download_count 与 files.download_count。
func (s *Service) ResolveForDownload(token string) (Resolved, error) {
	r, err := s.Resolve(token)
	if err != nil {
		return Resolved{}, err
	}
	if r.Share.Permission != PermissionDownload {
		return Resolved{}, ErrDownloadForbidden
	}
	if r.Blob.Status != files.BlobStatusAvailable {
		return Resolved{}, ErrFileNotAvailable
	}
	consumed, err := s.repo.ConsumeDownload(r.Share.ID, s.now())
	if err != nil {
		return Resolved{}, err
	}
	if !consumed {
		// 区分并发失效原因：达上限返回 ErrDownloadLimit，其余（刚撤销/刚过期）返回 ErrGone。
		latest, gerr := s.repo.Get(r.Share.ID)
		if gerr != nil {
			return Resolved{}, ErrNotFound
		}
		if latest.MaxDownloads != nil && latest.DownloadCount >= *latest.MaxDownloads {
			return Resolved{}, ErrDownloadLimit
		}
		return Resolved{}, ErrGone
	}
	if err := s.files.IncrementDownloadCount(r.Share.OwnerID, r.Share.FileID); err != nil {
		return Resolved{}, err
	}
	// 公开下载无访问者身份（downloader 传零值）：恒通知分享 owner。
	s.notifyAccessed(r, uuid.Nil)
	return r, nil
}

// ---------- 密码保护（设计 6.6.2 / US-003） ----------

// VerifySharePassword 校验公开分享密码：通过时创建 1 小时有效的访问会话，
// 返回会话 cookie 值（随机数 URL-safe base64，明文不落库；库中只存
// SHA-256(share_id||value)）。token 不存在 404 语义（ErrNotFound）、
// 已失效 ErrGone、未设密码 ErrPasswordNotSet、密码错误 ErrInvalidCredentials。
func (s *Service) VerifySharePassword(token, password string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrNotFound
	}
	sh, err := s.repo.GetByTokenHash(HashToken(token))
	if err != nil {
		return "", ErrNotFound
	}
	if !shareActive(sh, s.now()) {
		return "", ErrGone
	}
	if !sh.HasPassword() {
		return "", ErrPasswordNotSet
	}
	if !verifySharePassword(sh.ID, password, sh.PasswordHash) {
		return "", ErrInvalidCredentials
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	value := base64.RawURLEncoding.EncodeToString(buf)
	now := s.now()
	session := AccessSession{ID: uuid.New(), ShareID: sh.ID, SessionHash: hashAccessSession(sh.ID, value), ExpiresAt: now.Add(shareSessionTTL), CreatedAt: now}
	if err := s.repo.CreateSession(session); err != nil {
		return "", err
	}
	return value, nil
}

// ValidateShareSession 校验公开访问会话 cookie 值是否对分享有效：
// 按 SHA-256(share_id||value) 命中且未过期。任何失败一律 false（不区分
// 不存在/已过期/属其他分享，避免向匿名调用方泄露会话状态）。
func (s *Service) ValidateShareSession(shareID uuid.UUID, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	session, err := s.repo.GetSessionByHash(hashAccessSession(shareID, value))
	if err != nil || session.ShareID != shareID {
		return false
	}
	return s.now().Before(session.ExpiresAt)
}

// ---------- 分享更新（PATCH /api/v1/shares/:id）与详情统计 ----------

// SharePatch 为 owner 可修改的字段集合（设计 6.6.3）：有效期、下载上限与
// 水印；permission / visibility / 授权名单不可改。nil 指针表示不修改该字段；
// ExpiresIn/MaxDownloads 取 0 表示「清除限制」（永久 / 不限），
// WatermarkText 取空串表示恢复默认模板（NULL）。
type SharePatch struct {
	ExpiresIn        *time.Duration
	MaxDownloads     *int
	WatermarkEnabled *bool
	WatermarkText    *string
}

// Update 校验并应用 owner 的分享字段更新，返回更新后的分享记录。
// 非 owner / 不存在统一 ErrNotFound（不泄露存在性）。
func (s *Service) Update(owner, shareID uuid.UUID, patch SharePatch) (Share, error) {
	if _, err := s.repo.GetByOwner(owner, shareID); err != nil {
		return Share{}, err
	}
	fields := map[string]any{}
	if patch.ExpiresIn != nil {
		if *patch.ExpiresIn < 0 {
			return Share{}, ErrInvalidExpiry
		}
		if *patch.ExpiresIn == 0 {
			fields["expires_at"] = nil
		} else {
			expiresAt := s.now().Add(*patch.ExpiresIn)
			fields["expires_at"] = &expiresAt
		}
	}
	if patch.MaxDownloads != nil {
		if *patch.MaxDownloads < 0 {
			return Share{}, ErrInvalidMaxDownloads
		}
		if *patch.MaxDownloads == 0 {
			fields["max_downloads"] = nil
		} else {
			fields["max_downloads"] = *patch.MaxDownloads
		}
	}
	if patch.WatermarkEnabled != nil {
		fields["watermark_enabled"] = *patch.WatermarkEnabled
	}
	if patch.WatermarkText != nil {
		if err := validateWatermarkText(*patch.WatermarkText); err != nil {
			return Share{}, err
		}
		if *patch.WatermarkText == "" {
			fields["watermark_text"] = nil
		} else {
			fields["watermark_text"] = *patch.WatermarkText
		}
	}
	if len(fields) > 0 {
		if err := s.repo.UpdateFields(shareID, fields); err != nil {
			return Share{}, err
		}
	}
	return s.repo.GetByOwner(owner, shareID)
}

// ShareDetail 为 GET /api/v1/shares/:id 响应聚合：分享记录（脱敏后由
// HTTP 层序列化）+ 关联文件名 + 访问统计。文件已删除时 FileName 为空串
// （不阻断详情）；stats 读取失败时返回空统计（统计为辅助信息，不阻断）。
type ShareDetail struct {
	Share    Share
	FileName string
	Stats    AccessStats
	HasStats bool
}

// GetDetail 返回 owner 名下分享的详情与访问统计（C8，设计 6.6.3）。
func (s *Service) GetDetail(owner, shareID uuid.UUID) (ShareDetail, error) {
	sh, err := s.repo.GetByOwner(owner, shareID)
	if err != nil {
		return ShareDetail{}, err
	}
	detail := ShareDetail{Share: sh}
	if f, ferr := s.files.Get(sh.OwnerID, sh.FileID); ferr == nil {
		detail.FileName = f.Name
	}
	if stats, serr := s.repo.ShareAccessStats(shareID); serr == nil {
		detail.Stats = stats
		detail.HasStats = true
	}
	return detail, nil
}

// RecordAccessEvent 写入公开访问事件（file_access_events）；调用方（HTTP 层）
// 已完成 IP 哈希/前缀脱敏与 UA 截断。写入失败由调用方决定忽略（尽力而为）。
func (s *Service) RecordAccessEvent(e AccessEvent) error {
	return s.repo.RecordAccessEvent(e)
}
