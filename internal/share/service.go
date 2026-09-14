package share

import (
	"crypto/rand"
	"crypto/sha256"
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
	// defaultExpiryHours 为「创建请求未指定有效期」时的默认时长（小时）
	// 热读取（system_settings 的 share.default_expiry_hours，main 注入）；
	// nil 或返回非正值时回退既有行为（不设默认，即永久）。
	defaultExpiryHours func() int
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

// SetTeamMembership 注入团队成员关系判定器；未注入时 share_teams 授权不可达（安全默认拒绝）。
func (s *Service) SetTeamMembership(m TeamMembership) {
	if m != nil {
		s.membership = m
	}
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

// Create 为 owner 名下未删除、当前版本对象为 available 的文件创建公开分享，
// 返回分享记录与明文 token（仅此一次可见，数据库只保存哈希）。
func (s *Service) Create(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int) (Share, string, error) {
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
	f, err := s.files.Get(owner, fileID)
	if err != nil {
		return Share{}, "", ErrFileNotFound
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
	sh := Share{ID: uuid.New(), OwnerID: owner, FileID: fileID, TokenHash: HashToken(token), Visibility: VisibilityPublic, Permission: permission, MaxDownloads: maxDownloads, CreatedAt: now}
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
// userIds/teamIds 自动去重；私有分享不影响 files.is_public。
func (s *Service) CreatePrivate(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int, userIds, teamIds []uuid.UUID) (Share, error) {
	if permission != PermissionView && permission != PermissionDownload {
		return Share{}, ErrInvalidPermission
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
	f, err := s.files.Get(owner, fileID)
	if err != nil {
		return Share{}, ErrFileNotFound
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
	sh := Share{ID: uuid.New(), OwnerID: owner, FileID: fileID, Visibility: VisibilityPrivate, Permission: permission, MaxDownloads: maxDownloads, CreatedAt: now}
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
