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
	// ErrBundleTooFew 表示打包分享（一个链接多个文件）至少需要两个条目。
	ErrBundleTooFew = errors.New("bundle share requires at least two files")
	// ErrInvalidTitle 表示打包分享标题非法（>200 rune 或含控制字符）。
	ErrInvalidTitle = errors.New("invalid share title")
	// ErrNotRevoked 表示分享尚未撤销（清除记录仅对已撤销分享开放）。
	ErrNotRevoked = errors.New("share is not revoked")
	// ErrPurgeRetention 表示撤销未满 30 天保留期，暂不可清除记录。
	ErrPurgeRetention = errors.New("share record is within the 30-day retention window")
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

// RenderWatermark 渲染公开访问水印文案：替换 {user} → 访问者标识（viewer
// 非空为登录用户名/ID，公开匿名访问回退脱敏 IP 前缀）、{email}/{ip} → 脱敏
// IP 前缀（公开访问无登录身份）、{date} → 当地日期（YYYY-MM-DD）、{name} →
// 文件名；未知占位符原样保留。模板为空时回退 DefaultWatermarkTemplate。
func RenderWatermark(template, fileName, ip, viewer string, now time.Time) string {
	if template == "" {
		template = DefaultWatermarkTemplate
	}
	prefix := IPPrefix(ip)
	if prefix == "" {
		prefix = "unknown"
	}
	user := strings.TrimSpace(viewer)
	if user == "" {
		user = prefix
	}
	return strings.NewReplacer(
		"{user}", user,
		"{email}", prefix,
		"{ip}", prefix,
		"{date}", now.Format("2006-01-02"),
		"{name}", fileName,
	).Replace(template)
}

// validateTitle 校验打包分享标题：≤200 rune 且不含控制字符（空串合法 =
// 未自定义，公开页回退默认命名）。
func validateTitle(title string) error {
	if title == "" {
		return nil
	}
	n := 0
	for _, r := range title {
		if r < 0x20 || r == 0x7f {
			return ErrInvalidTitle
		}
		n++
	}
	if n > 200 {
		return ErrInvalidTitle
	}
	return nil
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

// OwnerListFilter 是「我的分享」列表的过滤条件（GET /shares 查询参数映射）。
type OwnerListFilter struct {
	// Q 文件名子串（大小写不敏感）；空串不过滤。
	Q string
	// Visibility public|private；空串不过滤。
	Visibility string
	// Status active|revoked|expired；空串不过滤（active = 未撤销未过期；
	// revoked 优先于 expired）。判定基准时间 Now。
	Status string
	Now    time.Time
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
	// ListByOwnerFiltered 分页返回 owner 的分享（created_at 倒序），支持
	// 文件名子串 / 可见性 / 状态过滤（「我的分享」页服务端分页），返回
	// 当页数据与过滤后总数。
	ListByOwnerFiltered(owner uuid.UUID, f OwnerListFilter, limit, offset int) ([]Share, int64, error)
	// ListSharedWithUser 返回分享给 user 的有效私有分享（share_users 显式授权，
	// 或 user 属于 share_spaces 任一授权空间的成员；公开分享不含），
	// 按 created_at 倒序。空间判定由实现方实时完成（GormStore 判定
	// space_members/space_group_members）。
	ListSharedWithUser(user uuid.UUID, now time.Time, limit int) ([]Share, error)
	Revoke(id uuid.UUID, now time.Time) error
	// Delete 物理删除分享行（「清除记录」：仅撤销满保留期的记录；关联表
	// 经 FK 级联清理）。行不存在静默成功。
	Delete(id uuid.UUID) error
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
	// 私有分享显式授权（share_users / share_spaces，见 migrations/040_unified_spaces.sql）。
	AddShareUsers(shareID uuid.UUID, userIDs []uuid.UUID, now time.Time) error
	AddShareSpaces(shareID uuid.UUID, spaceIDs []uuid.UUID, now time.Time) error
	ListShareUserIDs(shareID uuid.UUID) ([]uuid.UUID, error)
	ListShareSpaceIDs(shareID uuid.UUID) ([]uuid.UUID, error)
	// 打包分享可见条目（share_files，见 migrations/036_share_files.sql）。
	AddShareFiles(shareID uuid.UUID, fileIDs []uuid.UUID, now time.Time) error
	ListShareFileIDs(shareID uuid.UUID) ([]uuid.UUID, error)
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

// SpaceMembership 实时判定用户是否属于给定空间之一（由 space 包实现，
// 直接成员或经用户组命中；未来可替换为 Casbin 等策略引擎适配器）。
type SpaceMembership interface {
	UserInAnySpace(userID uuid.UUID, spaceIDs []uuid.UUID) (bool, error)
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
	membership SpaceMembership
	directory  UserDirectory
	// spaceSharer 空间文件分享门控（main 注入 space.CanShare）：nil 时空间
	// 文件分享一律拒绝（fail closed）。
	spaceSharer SpaceSharer
	// acl 空间文件 share 判定的路径级 ACL 求值器（main 注入 acl.Service.
	// ResolveForFile）；nil 未接线（行为不变）。
	acl files.ACLResolver
	// defaultExpiryHours 为「创建请求未指定有效期」时的默认时长（小时）
	// 热读取（system_settings 的 share.default_expiry_hours，main 注入）；
	// nil 或返回非正值时回退既有行为（不设默认，即永久）。
	defaultExpiryHours func() int
	// watermarkDefaults 为「创建请求未显式指定水印开关/模板」时的默认值
	// 热读取（system_settings 的 share.default_watermark / share.watermark_text，
	// main 注入）；nil 时回退内置默认（开启 + DefaultWatermarkTemplate）。
	watermarkDefaults func() (enabled bool, text string)
	publicEnabled     func() bool
	// tree 目录分享树源（main 注入 *files.Store；nil 时 ResolveTree 返回
	// ErrTreeUnavailable，HTTP 层 503）。
	tree TreeSource
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

// SetSpaceMembership 注入空间成员关系判定器；未注入时 share_spaces 授权不可达（安全默认拒绝）。
func (s *Service) SetSpaceMembership(m SpaceMembership) {
	if m != nil {
		s.membership = m
	}
}

// SpaceSharer 判定用户能否为空间文件创建分享（space 包 CanShare 注入实现：
// 角色矩阵含 share：owner/admin/member_share）。
type SpaceSharer func(userID, spaceID uuid.UUID) (bool, error)

// SetSpaceSharer 注入空间文件分享门控（幂等）：
// 未注入时空间文件分享一律拒绝（fail closed）。
func (s *Service) SetSpaceSharer(fn SpaceSharer) {
	if fn != nil {
		s.spaceSharer = fn
	}
}

// SetACLResolver 注入路径级 ACL 求值器（幂等）：
// 空间文件的 share 判定先走 ACL（沿 parent 链求值 folder_acl 条目），
// 链上无适用条目（matched=false）回退 spaceSharer；未注入时行为不变。
func (s *Service) SetACLResolver(a files.ACLResolver) {
	if a != nil {
		s.acl = a
	}
}

// authorizeShare 空间文件分享门控：先经路径级 ACL（share，matched 则用其
// 结果），未匹配走 CanShare（owner/admin/member_share）；个人语义不存在
// （统一空间模型，Get 已校验文件行 owner）。
func (s *Service) authorizeShare(f files.File, user uuid.UUID) error {
	if f.SpaceID == uuid.Nil {
		return nil
	}
	if s.acl != nil {
		allowed, matched, err := s.acl(f.ID, f.SpaceID, user, "share")
		if err != nil {
			return err
		}
		if matched {
			if !allowed {
				return ErrForbidden
			}
			return nil
		}
	}
	if s.spaceSharer == nil {
		return ErrForbidden
	}
	ok, err := s.spaceSharer(user, f.SpaceID)
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
// Password 仅对公开分享生效（4-64 字符，存 SHA-256(password||id) 哈希；
// 041 起明文另存 password_plain 供分享者再次查看复制）；私有分享不受密码
// 影响，显式传入返回 ErrInvalidPassword。
// WatermarkEnabled / WatermarkText 未指定（nil）时采用水印默认值热读取。
// Title 为打包分享自定义标题（≤200 rune；空串 = 未自定义）。
type ShareOptions struct {
	Password         string
	WatermarkEnabled *bool
	WatermarkText    *string
	Title            string
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
	switch f.Type {
	case "file":
		_, blob, err := s.files.CurrentVersion(owner, fileID)
		if err != nil {
			return Share{}, "", ErrFileNotShareable
		}
		if blob.Status != files.BlobStatusAvailable {
			return Share{}, "", ErrFileNotShareable
		}
	case "folder":
		// 目录分享根：不要求当前版本（目录无 blob），子树经 tree/raw-share 访问。
	default:
		return Share{}, "", ErrFileNotShareable
	}
	token, err := NewToken()
	if err != nil {
		return Share{}, "", err
	}
	now := s.now()
	sh := Share{ID: uuid.New(), OwnerID: owner, FileID: fileID, TokenHash: HashToken(token), Visibility: VisibilityPublic, Permission: permission, MaxDownloads: maxDownloads, WatermarkEnabled: watermarkEnabled, WatermarkText: &watermarkText, Token: token, CreatedAt: now}
	if opts.Password != "" {
		// 密码哈希依赖 share_id 作盐，须在生成 ID 后计算。
		sh.PasswordHash = HashSharePassword(sh.ID, opts.Password)
		sh.PasswordPlain = opts.Password
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

// CreateBundle 为多个文件/目录创建一个「打包」公开分享（批量分享 = 一个
// 目录式链接）：分享锚点取首个条目的父目录（复用目录分享的 tree/raw/zip
// 访问链路），对外可见条目由 fileIDs 限定（锚点目录的其余子项不暴露）。
// 仅公开分享；逐条目校验归属（owner 读权限 + 团队 CanShare 门控）与
// 可用性（文件当前版本 available；目录无版本要求）；其余选项同 CreatePublic。
func (s *Service) CreateBundle(owner uuid.UUID, fileIDs []uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int, opts ShareOptions) (Share, string, error) {
	if s.publicEnabled != nil && !s.publicEnabled() {
		return Share{}, "", ErrPublicDisabled
	}
	if permission != PermissionView && permission != PermissionDownload {
		return Share{}, "", ErrInvalidPermission
	}
	if expiresIn < 0 {
		return Share{}, "", ErrInvalidExpiry
	}
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
	items := dedupeIDs(fileIDs)
	if len(items) < 2 {
		return Share{}, "", ErrBundleTooFew
	}
	title := strings.TrimSpace(opts.Title)
	if err := validateTitle(title); err != nil {
		return Share{}, "", err
	}
	var anchor *uuid.UUID
	for _, id := range items {
		f, ferr := s.files.Get(owner, id)
		if ferr != nil {
			return Share{}, "", ErrFileNotFound
		}
		if ferr := s.authorizeShare(f, owner); ferr != nil {
			return Share{}, "", ferr
		}
		if f.IsRoot {
			return Share{}, "", ErrFileNotShareable
		}
		switch f.Type {
		case "file":
			_, blob, verr := s.files.CurrentVersion(owner, id)
			if verr != nil {
				return Share{}, "", ErrFileNotShareable
			}
			if blob.Status != files.BlobStatusAvailable {
				return Share{}, "", ErrFileNotShareable
			}
		case "folder":
			// 目录条目：子树经 tree 访问，无版本要求。
		default:
			return Share{}, "", ErrFileNotShareable
		}
		if anchor == nil {
			if f.ParentID == nil {
				return Share{}, "", ErrFileNotShareable
			}
			a := *f.ParentID
			anchor = &a
		}
	}
	anchorFile, aerr := s.files.Get(owner, *anchor)
	if aerr != nil {
		return Share{}, "", ErrFileNotFound
	}
	if aerr := s.authorizeShare(anchorFile, owner); aerr != nil {
		return Share{}, "", aerr
	}
	token, terr := NewToken()
	if terr != nil {
		return Share{}, "", terr
	}
	now := s.now()
	sh := Share{ID: uuid.New(), OwnerID: owner, FileID: *anchor, TokenHash: HashToken(token), Visibility: VisibilityPublic, Permission: permission, MaxDownloads: maxDownloads, WatermarkEnabled: watermarkEnabled, WatermarkText: &watermarkText, IsBundle: true, Token: token, Title: title, CreatedAt: now}
	if opts.Password != "" {
		sh.PasswordHash = HashSharePassword(sh.ID, opts.Password)
		sh.PasswordPlain = opts.Password
	}
	if expiresIn > 0 {
		expiresAt := now.Add(expiresIn)
		sh.ExpiresAt = &expiresAt
	}
	if err := s.repo.Create(sh); err != nil {
		return Share{}, "", err
	}
	if err := s.repo.AddShareFiles(sh.ID, items, now); err != nil {
		return Share{}, "", err
	}
	_ = s.repo.SetFilePublic(*anchor, true)
	return sh, token, nil
}

// BundleItems 返回打包分享的可见条目元数据（已删除/不可读条目静默跳过）。
func (s *Service) BundleItems(sh Share) ([]files.File, error) {
	ids, err := s.repo.ListShareFileIDs(sh.ID)
	if err != nil {
		return nil, err
	}
	out := make([]files.File, 0, len(ids))
	for _, id := range ids {
		if f, ferr := s.files.Get(sh.OwnerID, id); ferr == nil && f.DeletedAt == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

// AttachReferenceFiles 把富文本文档引用的资源附加为分享的可见 grant 条目
//（share_files，与打包分享同一张表；单文件分享亦支持）：逐条要求 owner 可读
// 且未删除、条目本身须为文件；不可读/已删的越界引用静默跳过（公开页渲染
// 占位）。已存在的条目去重跳过。返回实际新增条目数；持久化失败返回错误
//（调用方决定是否吞掉——附加资源失败不应阻断分享本身）。
func (s *Service) AttachReferenceFiles(sh Share, owner uuid.UUID, fileIDs []uuid.UUID) (int, error) {
	if len(fileIDs) == 0 || sh.ID == uuid.Nil {
		return 0, nil
	}
	existing, err := s.repo.ListShareFileIDs(sh.ID)
	if err != nil {
		return 0, err
	}
	seen := make(map[uuid.UUID]struct{}, len(existing)+len(fileIDs))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	seen[sh.FileID] = struct{}{} // 分享根自身不重复附加
	added := make([]uuid.UUID, 0, len(fileIDs))
	for _, id := range fileIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		f, ferr := s.files.Get(owner, id)
		if ferr != nil || f.DeletedAt != nil || f.Type != "file" {
			continue
		}
		seen[id] = struct{}{}
		added = append(added, id)
	}
	if len(added) == 0 {
		return 0, nil
	}
	if err := s.repo.AddShareFiles(sh.ID, added, s.now()); err != nil {
		return 0, err
	}
	return len(added), nil
}

// CreatePrivate 为 owner 名下文件创建私有分享：不生成公开 token（token_hash 为空），
// 访问仅限 share_users 显式授权用户与 share_spaces 授权空间的成员。
// userIds/spaceIds 自动去重；私有分享不受密码影响（显式传入密码返回错误）。
func (s *Service) CreatePrivate(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int, userIds, spaceIds []uuid.UUID) (Share, error) {
	return s.CreatePrivateWithOptions(owner, fileID, permission, expiresIn, maxDownloads, userIds, spaceIds, ShareOptions{})
}

// CreatePrivateWithOptions 在 CreatePrivate 基础上支持水印字段；
// 密码仅适用于公开分享，显式传入返回 ErrInvalidPassword。
// 空间文件须经 CanShare 门控（同 CreatePublic）。
func (s *Service) CreatePrivateWithOptions(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int, userIds, spaceIds []uuid.UUID, opts ShareOptions) (Share, error) {
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
	if ids := dedupeIDs(spaceIds); len(ids) > 0 {
		if err := s.repo.AddShareSpaces(sh.ID, ids, now); err != nil {
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
// 或属于 share_spaces 任一空间的成员（实时判定：直接成员或经用户组，
// 成员变动立即生效）。
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
	spaceIDs, err := s.repo.ListShareSpaceIDs(sh.ID)
	if err != nil || len(spaceIDs) == 0 || s.membership == nil {
		return false
	}
	ok, err := s.membership.UserInAnySpace(user, spaceIDs)
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
// v2.4 起「删除」措辞回归「撤销」：撤销仅置 revoked_at，记录保留（不做自动
// 清理），满 30 天后可经 Purge 手动清除。
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

// PurgeRetention 为撤销记录的手动清除保留期：撤销满 30 天后才可清除。
const PurgeRetention = 30 * 24 * time.Hour

// Purge 物理删除 owner 名下已撤销且撤销满 30 天的分享记录（手动「清除
// 记录」；分享行删除经 FK 级联清理 share_users/share_spaces/share_files
// 等关联）。未撤销返回 ErrNotRevoked；保留期内返回 ErrPurgeRetention。
func (s *Service) Purge(owner, shareID uuid.UUID) error {
	sh, err := s.repo.GetByOwner(owner, shareID)
	if err != nil {
		return err
	}
	if sh.RevokedAt == nil {
		return ErrNotRevoked
	}
	if s.now().Sub(*sh.RevokedAt) < PurgeRetention {
		return ErrPurgeRetention
	}
	return s.repo.Delete(sh.ID)
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
	return s.withFileNames(owner, shares), nil
}

// withFileNames 为分享行批量补齐关联文件名：*files.Store 实现
// GetFileNamesForOwner 时走单条 IN 查询（每页一次）；测试 fake 未实现时
// 回退逐条 Get（保持原行为）。注意不能统一走 Get——它会更新
// last_access_at，列表展示将污染「最近访问」。
func (s *Service) withFileNames(owner uuid.UUID, shares []Share) []ShareWithFile {
	out := make([]ShareWithFile, 0, len(shares))
	if bs, ok := s.files.(interface {
		GetFileNamesForOwner(owner uuid.UUID, ids []uuid.UUID) map[uuid.UUID]string
	}); ok {
		ids := make([]uuid.UUID, 0, len(shares))
		for _, sh := range shares {
			ids = append(ids, sh.FileID)
		}
		names := bs.GetFileNamesForOwner(owner, ids)
		for _, sh := range shares {
			item := ShareWithFile{Share: sh, FileName: names[sh.FileID]}
			out = append(out, item)
		}
		return out
	}
	for _, sh := range shares {
		item := ShareWithFile{Share: sh}
		if f, ferr := s.files.Get(sh.OwnerID, sh.FileID); ferr == nil {
			item.FileName = f.Name
		}
		out = append(out, item)
	}
	return out
}

// ListPage 分页返回 owner 的分享并附关联文件名（created_at 倒序），支持
// 文件名子串 / 可见性 / 状态过滤；返回当页数据与过滤后总数（「我的分享」
// 页服务端分页，v1.7.1）。
func (s *Service) ListPage(owner uuid.UUID, f OwnerListFilter, limit, offset int) ([]ShareWithFile, int64, error) {
	if f.Now.IsZero() {
		f.Now = s.now()
	}
	shares, total, err := s.repo.ListByOwnerFiltered(owner, f, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return s.withFileNames(owner, shares), total, nil
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
// share_users 显式授权或 share_spaces 空间成员命中（repo 实时判定），
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
// 文件已被删除或当前版本缺失时同样视为失效；目录分享根（type=folder）无版本
// 语义，直接返回（子树访问经 ResolveTree）。
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
	if f.Type == "folder" {
		return Resolved{Share: sh, File: f}, nil
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

// ResolveForFolderDownload 为目录分享的打包下载（流式 zip）入口：分享根须为
// 目录且分享具 download 权限，随后与 ResolveForDownload 相同语义地原子消耗
// 一次下载额度（达上限 ErrDownloadLimit、刚失效 ErrGone）并递增根目录的
// files.download_count。子树内容读取由调用方完成（HTTP 层流式 zip）。
func (s *Service) ResolveForFolderDownload(token string) (Resolved, error) {
	r, err := s.Resolve(token)
	if err != nil {
		return Resolved{}, err
	}
	if r.File.Type != "folder" {
		return Resolved{}, ErrFileNotShareable
	}
	if r.Share.Permission != PermissionDownload {
		return Resolved{}, ErrDownloadForbidden
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
	// 公开打包下载无访问者身份（downloader 传零值）：恒通知分享 owner。
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
