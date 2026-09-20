package files

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
)

var (
	ErrInvalidName = errors.New("invalid name")
	ErrConflict    = errors.New("name conflict")
	ErrNotFound    = errors.New("file not found")
	ErrRoot        = errors.New("root cannot be changed")
	ErrNoVersion   = errors.New("file has no current version")
	// ErrFolderCopy 仅在复制根目录（is_root）等不可复制目录时返回；
	// 普通目录子树复制见 CopyFolder。
	ErrFolderCopy = errors.New("folders cannot be copied")
	// ErrCopyLimit 单次目录复制的子树条目数超限。
	ErrCopyLimit = errors.New("folder exceeds copy limit")
	// ErrForbidden 表示用户对空间目录无写权限（如 guest 或非成员）。
	ErrForbidden = errors.New("no permission to write this folder")
	// ErrFolderDepth 目录嵌套深度超过上限（folder.max_depth，默认 32）。
	ErrFolderDepth = errors.New("folder depth limit exceeded")
)

func NormalizeName(name string) (string, error) {
	name = norm.NFC.String(name)
	for _, r := range name {
		// Cf（格式字符，如 RLO U+202E、零宽字符）可欺骗展示层排序/渲染，
		// 与控制字符一并拒绝。
		if r == '/' || r == '\\' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", ErrInvalidName
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.HasSuffix(name, ".") || len([]rune(name)) > 255 {
		return "", ErrInvalidName
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	reserved := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}
	if reserved[base] {
		return "", ErrInvalidName
	}
	return name, nil
}

// SpaceWriter 判定用户能否写入空间（角色矩阵含 write：owner/admin/
// member_share/member），由 space 包注入实现；nil 表示空间权限源未配置
// （拒绝空间目录写入）。
type SpaceWriter func(userID, spaceID uuid.UUID) (bool, error)

// SpaceReader 判定用户是否可读取空间（任意在册成员），由 space 包注入实现。
// nil 表示成员判定源未配置（拒绝空间文件读取，安全默认）。
type SpaceReader func(userID, spaceID uuid.UUID) (bool, error)

// SpaceDeleter 判定用户能否删除空间文件（角色矩阵含 delete），由 space
// 包注入实现；nil 表示未配置（拒绝，安全默认）。
type SpaceDeleter func(userID, spaceID uuid.UUID) (bool, error)

// ACLResolver 判定用户对空间作用域文件/目录的单个动作权限（路径级 ACL，
// acl 包注入实现）：沿 parent 链求值 folder_acl 条目，返回
// (allowed, matched)。matched=false 表示链上无适用条目（回退空间角色判定）；
// nil 表示未接线（无 ACL 行为不变）。perm 取 read/write/delete/share。
type ACLResolver func(fileOrFolderID, spaceID, userID uuid.UUID, perm string) (allowed, matched bool, err error)

// resolveACL 空间作用域资源的 ACL 求值入口：未接线直接未匹配；匹配则由
// 调用方按结果放行/拒绝（不再走空间角色判定）。
func resolveACL(acl ACLResolver, id, spaceID, user uuid.UUID, perm string) (bool, bool, error) {
	if acl == nil {
		return false, false, nil
	}
	return acl(id, spaceID, user, perm)
}

// defaultMaxFolderDepth 目录默认最大深度（根为 1；可经
// SetMaxFolderDepthProvider / folder.max_depth 覆盖）。
const defaultMaxFolderDepth = 32

type Store struct {
	db           *gorm.DB
	spaceWriter  SpaceWriter
	spaceReader  SpaceReader
	spaceDeleter SpaceDeleter
	// acl 空间作用域资源的路径级 ACL 求值器（acl 包注入；nil 未接线）。
	acl ACLResolver
	// maxVersions 为版本保留数的静态值；maxVersionsFn 为运行时提供器
	//（settings 热读取）；nil 时用 maxVersions。
	maxVersions   int
	maxVersionsFn func() int
	// retentionDaysFn 为版本保留时间窗（天）的运行时提供器（settings
	// 热读取，upload.version_retention_days）；nil 或 0 = 不启用时间窗。
	retentionDaysFn func() int
	// maxFolderDepthFn 为目录最大深度的运行时提供器（settings 热读取，
	// folder.max_depth）；nil 时用 maxFolderDepth。
	maxFolderDepthFn func() int
	maxFolderDepth   int
	// webpkgCleaner 清理网页包对象前缀（webpkg/<public_id>，webpkg.Remove 注入）；
	// Purge 事务提交后 best-effort 调用。nil 表示未接线（不清理）。
	webpkgCleaner func(prefix string) error
	// versionNotify 版本落库完成回调（AddVersion 事务提交后调用）：
	// main 注入空间 file.updated 通知逻辑；nil 表示未接线。回调不改变
	// 版本写入结果（错误由注入方自理）。
	versionNotify VersionNotifyFunc
	// versionDeletedNotify 历史版本删除完成回调（DeleteVersion 事务提交后
	// 调用）：main 注入空间 file.version.deleted 通知逻辑；nil 表示未接线。
	versionDeletedNotify VersionNotifyFunc
}

// VersionNotifyFunc 版本写入完成回调：f 为目标文件行、actor 为写入者、
// version 为新落库的版本（事务已提交）。main 接线：空间文件且 actor≠owner
// 时通知空间其他成员 file.updated。
type VersionNotifyFunc func(f File, actor uuid.UUID, version FileVersion)

func NewStore(db *gorm.DB) *Store {
	return &Store{db: db, maxVersions: defaultMaxVersions, maxFolderDepth: defaultMaxFolderDepth}
}

// SetSpaceWriter 注入空间写权限判定器（幂等）。
func (s *Store) SetSpaceWriter(w SpaceWriter) {
	if w != nil {
		s.spaceWriter = w
	}
}

// SetSpaceReader 注入空间成员读判定器（幂等）。
func (s *Store) SetSpaceReader(r SpaceReader) {
	if r != nil {
		s.spaceReader = r
	}
}

// SetSpaceDeleter 注入空间删除权限判定器（幂等）。
func (s *Store) SetSpaceDeleter(d SpaceDeleter) {
	if d != nil {
		s.spaceDeleter = d
	}
}

// SetACLResolver 注入路径级 ACL 求值器（幂等）：空间作用域文件/目录的
// read/write/delete 判定先走 ACL，链上无适用条目（matched=false）回退
// 既有空间角色判定；未注入时行为完全不变。
func (s *Store) SetACLResolver(a ACLResolver) {
	if a != nil {
		s.acl = a
	}
}

// SetWebpkgCleaner 注入网页包对象前缀清理回调（幂等；main 接线 webpkg.Remove，
// 不改 upload.Storage 接口）：Purge 彻底删除文件时删除其关联网页包的
// webpkg/<public_id>/ 前缀对象。
func (s *Store) SetWebpkgCleaner(fn func(prefix string) error) {
	if fn != nil {
		s.webpkgCleaner = fn
	}
}

// SetNotifyDispatcher 注入版本写入完成回调（幂等）：AddVersion 事务提交后
// 调用（空间 file.updated 通知的接线点）；回调自行决定同步/异步执行策略。
func (s *Store) SetNotifyDispatcher(fn VersionNotifyFunc) {
	if fn != nil {
		s.versionNotify = fn
	}
}

// SetVersionDeletedDispatcher 注入历史版本删除完成回调（幂等）：DeleteVersion
// 事务提交后调用（空间 file.version.deleted 通知文件 owner 的接线点）；
// 回调自行决定同步/异步执行策略，不改变删除结果。
func (s *Store) SetVersionDeletedDispatcher(fn VersionNotifyFunc) {
	if fn != nil {
		s.versionDeletedNotify = fn
	}
}

// SetMaxFolderDepthProvider 注入目录最大深度的运行时提供器（幂等；nil 不
// 覆盖）：每次建目录/移动前热读取（system_settings 的 folder.max_depth），
// 返回非正值（读取失败回退哨兵）时回退 maxFolderDepth（默认 32）。
func (s *Store) SetMaxFolderDepthProvider(fn func() int) {
	if fn != nil {
		s.maxFolderDepthFn = fn
	}
}

// effectiveMaxFolderDepth 返回当前生效的目录最大深度：提供器优先，异常回退静态值。
func (s *Store) effectiveMaxFolderDepth() int {
	if s.maxFolderDepthFn != nil {
		if n := s.maxFolderDepthFn(); n >= 1 {
			return n
		}
	}
	return s.maxFolderDepth
}

// folderDepthOf 沿 parent 链向上数层级（根目录为 1，parent 自身即第 1 层）。
// files.tree_path（ltree）列当前无维护方，深度按 parent 链遍历计数；
// 环/断链防御：上限 effectiveMaxFolderDepth+1 步。
func (s *Store) folderDepthOf(folderID uuid.UUID) (int, error) {
	cur := folderID
	for depth := 1; depth <= s.effectiveMaxFolderDepth()+1; depth++ {
		var f File
		if err := s.db.Select("id", "parent_id").Where("id = ?", cur).First(&f).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return 0, ErrNotFound
			}
			return 0, err
		}
		if f.ParentID == nil {
			return depth, nil
		}
		cur = *f.ParentID
	}
	return 0, ErrFolderDepth
}

// folderSubtreeHeight 返回目录子树高度（自身为 1；按子目录逐层下探）。
// 深度受 effectiveMaxFolderDepth 约束，遍历步数有界。
func (s *Store) folderSubtreeHeight(root uuid.UUID) (int, error) {
	height := 0
	level := []uuid.UUID{root}
	for len(level) > 0 && height < s.effectiveMaxFolderDepth()+1 {
		height++
		var next []uuid.UUID
		rows := make([]File, 0, len(level)*4)
		if err := s.db.Select("id").Where("parent_id IN ? AND type = 'folder' AND deleted_at IS NULL", level).Find(&rows).Error; err != nil {
			return 0, err
		}
		for _, r := range rows {
			next = append(next, r.ID)
		}
		level = next
	}
	return height, nil
}

// validateCreateDepth 校验在 parentDepth 层的目录下新建子项后不超上限
// （纯逻辑，供内存单测）：新子项深度 = parentDepth + 1 ≤ maxDepth。
func validateCreateDepth(parentDepth, maxDepth int) error {
	if parentDepth+1 > maxDepth {
		return ErrFolderDepth
	}
	return nil
}

// validateMoveDepth 校验把高度 subtreeHeight 的子树移动到 targetDepth 层
// 目录下不超上限（纯逻辑，供内存单测）：新子树最深深度 =
// targetDepth + subtreeHeight ≤ maxDepth。
func validateMoveDepth(targetDepth, subtreeHeight, maxDepth int) error {
	if targetDepth+subtreeHeight > maxDepth {
		return ErrFolderDepth
	}
	return nil
}

// getFolder 返回未删除的目录（不限 owner；统一空间模型下授权按空间角色判定）。
func (s *Store) getFolder(id uuid.UUID) (File, error) {
	var f File
	if err := s.db.Where("id = ? AND type = 'folder' AND deleted_at IS NULL", id).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	return f, nil
}

// folderAccessor 是目录访问的最小接口，validateFolder 纯逻辑部分供内存单测使用。
type folderAccessor interface {
	getFolder(id uuid.UUID) (File, error)
}

// authorizeParentFolder 判定 user 能否在 parent 下创建内容（统一空间模型）：
// 目录恒属某空间——先经路径级 ACL（write，链含 parent 自身，matched 则用
// 其结果），未匹配走空间成员写权限（owner/admin/member_share/member，
// guest 403）。
func authorizeParentFolder(repo folderAccessor, user, parent uuid.UUID, canWriteSpace SpaceWriter, acl ACLResolver) (File, error) {
	p, err := repo.getFolder(parent)
	if err != nil {
		return File{}, err
	}
	allowed, matched, aerr := resolveACL(acl, p.ID, p.SpaceID, user, "write")
	if aerr != nil {
		return File{}, aerr
	}
	if matched {
		if !allowed {
			return File{}, ErrForbidden
		}
		return p, nil
	}
	if canWriteSpace == nil {
		return File{}, ErrForbidden
	}
	ok, werr := canWriteSpace(user, p.SpaceID)
	if werr != nil {
		return File{}, werr
	}
	if !ok {
		return File{}, ErrForbidden
	}
	return p, nil
}

// ValidateFolder 校验 user 可在 parent 下上传/创建内容（空间成员写权限，
// 先经路径级 ACL write 判定，guest 与非成员 403）。
func (s *Store) ValidateFolder(user, parent uuid.UUID) error {
	_, err := authorizeParentFolder(s, user, parent, s.spaceWriter, s.acl)
	return err
}

// authorizeFileAccess 判定 user 能否读取 file（元数据/当前版本/下载/预览
// 共用入口）：文件行 owner 短路（上传者恒可读，与既有行为一致）；其余先经
// 路径级 ACL（read，链自父目录向上），matched 则用其结果（拒绝同非成员按
// ErrNotFound 处理，不泄露存在性），未匹配经注入的 CanRead 判定（空间
// 任意在册成员可读）。未注入判定器时一律拒绝（ErrForbidden，安全默认）。
// 非授权访问统一返回 ErrNotFound，不泄露资源存在性。
func authorizeFileAccess(f File, user uuid.UUID, isMember SpaceReader, acl ACLResolver) error {
	if f.OwnerID == user {
		return nil
	}
	allowed, matched, aerr := resolveACL(acl, f.ID, f.SpaceID, user, "read")
	if aerr != nil {
		return aerr
	}
	if matched {
		if !allowed {
			return ErrNotFound
		}
		return nil
	}
	if isMember == nil {
		return ErrForbidden
	}
	ok, err := isMember(user, f.SpaceID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// Get 返回 user 可访问的未删除单个文件（authorizeFileAccess 统一判定）：
// 空间内任意在册成员可读（下载/预览/元数据同规则）。
func (s *Store) Get(user, id uuid.UUID) (File, error) {
	var f File
	if err := s.db.Where("id = ? AND deleted_at IS NULL", id).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	if err := authorizeFileAccess(f, user, s.spaceReader, s.acl); err != nil {
		return File{}, err
	}
	now := time.Now().UTC()
	if err := s.db.Model(&File{}).Where("id = ?", id).UpdateColumn("last_access_at", now).Error; err == nil {
		f.LastAccessAt = &now
	}
	return f, nil
}

// GetFileByID 返回未删除文件行（不限 owner）。供内部集成使用（如 ONLYOFFICE
// 回调），调用方须自行保证授权（回调侧以 JWT 签名校验为准）。
func (s *Store) GetFileByID(id uuid.UUID) (File, error) {
	var f File
	if err := s.db.Where("id = ? AND deleted_at IS NULL", id).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	return f, nil
}

// CurrentVersion 返回文件当前版本及其关联的 ObjectBlob。
// 文件无当前版本时返回 ErrNoVersion。
func (s *Store) CurrentVersion(owner, fileID uuid.UUID) (FileVersion, ObjectBlob, error) {
	f, err := s.Get(owner, fileID)
	if err != nil {
		return FileVersion{}, ObjectBlob{}, err
	}
	if f.CurrentVersionID == nil {
		return FileVersion{}, ObjectBlob{}, ErrNoVersion
	}
	var version FileVersion
	if err := s.db.Where("id = ?", *f.CurrentVersionID).First(&version).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return FileVersion{}, ObjectBlob{}, ErrNoVersion
		}
		return FileVersion{}, ObjectBlob{}, err
	}
	var blob ObjectBlob
	if err := s.db.Where("id = ?", version.ObjectBlobID).First(&blob).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return FileVersion{}, ObjectBlob{}, ErrNoVersion
		}
		return FileVersion{}, ObjectBlob{}, err
	}
	return version, blob, nil
}

// IncrementDownloadCount 原子递增下载计数；user 须对文件有读权限
// （owner 或空间在册成员，经 authorizeFileAccess 判定），且文件未删除时生效。
func (s *Store) IncrementDownloadCount(user, fileID uuid.UUID) error {
	if _, err := s.Get(user, fileID); err != nil {
		return err
	}
	result := s.db.Model(&File{}).
		Where("id = ? AND deleted_at IS NULL", fileID).
		UpdateColumn("download_count", gorm.Expr("download_count + 1"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// IncrementViewCount 原子递增预览计数；user 须对文件有读权限
// （owner 或空间在册成员，经 authorizeFileAccess 判定），且文件未删除时生效。
func (s *Store) IncrementViewCount(user, fileID uuid.UUID) error {
	if _, err := s.Get(user, fileID); err != nil {
		return err
	}
	result := s.db.Model(&File{}).
		Where("id = ? AND deleted_at IS NULL", fileID).
		UpdateColumn("view_count", gorm.Expr("view_count + 1"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateFolderIn 在 parent 下创建子目录并继承其空间归属：
// 空间成员写权限（owner/admin/member_share/member，guest 403）。
// 深度校验（folder.max_depth）：parent 深度 + 1 不得超过上限。
func (s *Store) CreateFolderIn(user, parent uuid.UUID, name string) (File, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return File{}, err
	}
	p, err := authorizeParentFolder(s, user, parent, s.spaceWriter, s.acl)
	if err != nil {
		return File{}, err
	}
	if depth, derr := s.folderDepthOf(parent); derr == nil {
		if verr := validateCreateDepth(depth, s.effectiveMaxFolderDepth()); verr != nil {
			return File{}, verr
		}
	} else if !errors.Is(derr, ErrNotFound) {
		return File{}, derr
	}
	f := File{ID: uuid.New(), Name: n, ParentID: &parent, OwnerID: user, SpaceID: p.SpaceID, Type: "folder"}
	err = s.db.Create(&f).Error
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return File{}, ErrConflict
	}
	return f, err
}

// SpaceRoot 返回空间根目录（space_id、is_root=true）；不存在或已删除返回 ErrNotFound。
func (s *Store) SpaceRoot(spaceID uuid.UUID) (File, error) {
	var f File
	if err := s.db.Where("space_id = ? AND is_root = true AND deleted_at IS NULL", spaceID).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	return f, nil
}

// GetSpaceFolder 返回空间名下未删除的目录（含根目录）；读权限由调用方校验成员身份。
func (s *Store) GetSpaceFolder(spaceID, id uuid.UUID) (File, error) {
	var f File
	if err := s.db.Where("id = ? AND space_id = ? AND type = 'folder' AND deleted_at IS NULL", id, spaceID).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	return f, nil
}

// SpaceListFilter 空间目录列举过滤：tag（可选）/收藏（可选）+ 排序。
type SpaceListFilter struct {
	TagID   *uuid.UUID
	Starred *bool
	SortOptions
}

// ListSpace 列出空间目录内容（根目录或子目录）；成员读权限由调用方（HTTP 层）校验。
// tag_id/starred 过滤在目录范围内生效（EXISTS file_tags / is_starred）。
func (s *Store) ListSpace(spaceID, parent uuid.UUID, limit int, f SpaceListFilter) ([]File, error) {
	if f.Sort == "" {
		f.Sort = "name"
	}
	clause, ok := SortClause(f.Sort, f.Order)
	if !ok {
		clause = "lower(name) ASC, id ASC"
	}
	q := s.db.Where("space_id = ? AND parent_id = ? AND is_root = false AND deleted_at IS NULL", spaceID, parent)
	if f.TagID != nil {
		q = q.Where("EXISTS (SELECT 1 FROM file_tags ft WHERE ft.file_id = files.id AND ft.tag_id = ?)", *f.TagID)
	}
	if f.Starred != nil {
		q = q.Where("is_starred = ?", *f.Starred)
	}
	var out []File
	err := q.Order(clause).Limit(limit).Find(&out).Error
	return out, err
}

// authorizeSpaceDelete 判定 user 能否删除 file（软删除/彻底删除共用）：
// 文件行 owner（上传者）短路；其余先经路径级 ACL（delete），未匹配走
// CanDelete（角色矩阵含 delete）——delete 是独立于 write 的权限。
func authorizeSpaceDelete(f File, user uuid.UUID, canDeleteSpace SpaceDeleter, acl ACLResolver) error {
	if f.OwnerID == user {
		return nil
	}
	allowed, matched, aerr := resolveACL(acl, f.ID, f.SpaceID, user, "delete")
	if aerr != nil {
		return aerr
	}
	if matched {
		if !allowed {
			return ErrForbidden
		}
		return nil
	}
	if canDeleteSpace == nil {
		return ErrForbidden
	}
	ok, err := canDeleteSpace(user, f.SpaceID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// authorizeFileWrite 判定 user 能否写入 file（重命名/追加版本/恢复共用）：
// 文件行 owner 短路；其余先经路径级 ACL（write），未匹配走空间成员写权限。
func authorizeFileWrite(f File, user uuid.UUID, canWriteSpace SpaceWriter, acl ACLResolver) error {
	if f.OwnerID == user {
		return nil
	}
	allowed, matched, aerr := resolveACL(acl, f.ID, f.SpaceID, user, "write")
	if aerr != nil {
		return aerr
	}
	if matched {
		if !allowed {
			return ErrForbidden
		}
		return nil
	}
	if canWriteSpace == nil {
		return ErrForbidden
	}
	ok, err := canWriteSpace(user, f.SpaceID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// Rename 重命名文件或目录（根目录不可改名）。
// 空间文件走 authorizeFileWrite（文件行 owner 短路，其余成员按角色/ACL）。
func (s *Store) Rename(user, id uuid.UUID, name string) (File, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return File{}, err
	}
	var f File
	if err = s.db.Where("id = ? AND deleted_at IS NULL", id).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	if f.IsRoot {
		return File{}, ErrRoot
	}
	if err := authorizeFileWrite(f, user, s.spaceWriter, s.acl); err != nil {
		return File{}, err
	}
	result := s.db.Model(&f).Updates(map[string]any{"name": n})
	if result.Error != nil && strings.Contains(strings.ToLower(result.Error.Error()), "unique") {
		return File{}, ErrConflict
	}
	if result.Error != nil {
		return File{}, result.Error
	}
	f.Name = n
	return f, nil
}

// uploadBlobRepo 抽象 CreateUploadedFile 事务内 blob 内容去重所需的数据访问；
// gormVersionsRepo 满足（复用与 addVersionLogic 一致的复活/复用语义）。
type uploadBlobRepo interface {
	// GetBlobBySHA 按 sha256 返回 blob（不限状态，供复活判定）；不存在返回 ErrNotFound。
	GetBlobBySHA(sha256 string) (ObjectBlob, error)
	// IncrementBlobRef 引用计数 +1。
	IncrementBlobRef(id uuid.UUID) error
	// ResurrectBlob 将 deleting 且 ref_count=0 的 blob 复活为 available/ref_count=1；
	// 返回 false 表示已被 janitor 删除（应走新建分支）。
	ResurrectBlob(id uuid.UUID) (bool, error)
	// CreateBlob 新建 blob 行（调用方负责设置 RefCount/Status）。
	CreateBlob(b ObjectBlob) error
}

// resolveUploadBlobLogic 事务内按 sha256 解析落库应使用的 blob，分支与
// addVersionLogic 对齐：
//   - available → ref_count+1 复用（newBlob=false，调用方应删除本次上传的冗余对象）；
//   - deleting 且 ref_count=0（裁剪后待回收）→ 复活复用；janitor 恰已删行则新建；
//   - quarantined/failed 等其余状态 → ErrBlobUnavailable（sha256 唯一约束阻止新建）；
//   - 不存在 → 新建（newBlob=true）。
func resolveUploadBlobLogic(r uploadBlobRepo, storageKey, sha256 string, size int64, mimeType string) (ObjectBlob, bool, error) {
	blob, err := r.GetBlobBySHA(sha256)
	switch {
	case err == nil && blob.Status == BlobStatusAvailable:
		if err := r.IncrementBlobRef(blob.ID); err != nil {
			return ObjectBlob{}, false, err
		}
		return blob, false, nil
	case err == nil && blob.Status == BlobStatusDeleting && blob.RefCount == 0:
		// 裁剪遗留的待回收行：复活复用；若 janitor 恰好已删行则退回新建。
		resurrected, rerr := r.ResurrectBlob(blob.ID)
		if rerr != nil {
			return ObjectBlob{}, false, rerr
		}
		if resurrected {
			blob.Status, blob.RefCount = BlobStatusAvailable, 1
			return blob, false, nil
		}
		blob = ObjectBlob{ID: uuid.New(), SHA256: sha256, StorageKey: storageKey, Size: size, MimeType: mimeType, RefCount: 1, Status: BlobStatusAvailable}
		if cerr := r.CreateBlob(blob); cerr != nil {
			return ObjectBlob{}, false, cerr
		}
		return blob, true, nil
	case errors.Is(err, ErrNotFound):
		// 内容去重未命中：新建物理对象行。
		blob = ObjectBlob{ID: uuid.New(), SHA256: sha256, StorageKey: storageKey, Size: size, MimeType: mimeType, RefCount: 1, Status: BlobStatusAvailable}
		if cerr := r.CreateBlob(blob); cerr != nil {
			return ObjectBlob{}, false, cerr
		}
		return blob, true, nil
	case err == nil:
		// quarantined/failed 等状态：不可复用，且 sha256 唯一约束阻止新建。
		return ObjectBlob{}, false, ErrBlobUnavailable
	default:
		return ObjectBlob{}, false, err
	}
}

// CreateUploadedFile 在 parent 下落库上传完成的文件（含 blob 与 v1 版本），
// 返回新建文件 ID 与是否新建了 blob：事务内按 sha256 内容去重，同内容
// available blob 复用（ref_count+1，newBlob=false 表示本次上传的物理对象
// 冗余，调用方应删除——upload 包在 Complete 后按此清理，与 ReplaceFileVersion
// 的 newBlob 语义一致）。
func (s *Store) CreateUploadedFile(owner, parent uuid.UUID, name, storageKey string, size int64, sha256 string, mimeType string) (uuid.UUID, bool, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return uuid.Nil, false, err
	}
	p, err := authorizeParentFolder(s, owner, parent, s.spaceWriter, s.acl)
	if err != nil {
		return uuid.Nil, false, err
	}
	var created uuid.UUID
	var newBlob bool
	err = s.db.Transaction(func(tx *gorm.DB) error {
		f := File{ID: uuid.New(), Name: n, ParentID: &parent, OwnerID: owner, SpaceID: p.SpaceID, Type: "file"}
		if err := tx.Create(&f).Error; err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrConflict
			}
			return err
		}
		created = f.ID
		blob, isNew, berr := resolveUploadBlobLogic(&gormVersionsRepo{tx: tx}, storageKey, sha256, size, mimeType)
		if berr != nil {
			return berr
		}
		newBlob = isNew
		version := FileVersion{ID: uuid.New(), FileID: f.ID, Version: 1, ObjectBlobID: blob.ID, ContentSHA256: sha256, Size: size, UserID: owner}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		return tx.Model(&f).Update("current_version_id", version.ID).Error
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	return created, newBlob, nil
}

func (s *Store) Copy(user, id, parent uuid.UUID, name string) (File, error) {
	source, err := s.Get(user, id)
	if err != nil {
		return File{}, err
	}
	if source.Type == "folder" {
		return s.CopyFolder(user, id, parent, name)
	}
	parentFile, err := authorizeParentFolder(s, user, parent, s.spaceWriter, s.acl)
	if err != nil {
		return File{}, err
	}
	// 跨空间复制校验目标空间配额（超限 413 语义）。
	if err := s.checkSpaceQuota(parentFile.SpaceID, s.fileSize(source.ID)); err != nil {
		return File{}, err
	}
	if name == "" {
		name = source.Name + " copy"
	}
	n, err := NormalizeName(name)
	if err != nil {
		return File{}, err
	}
	if source.CurrentVersionID == nil {
		return File{}, ErrNoVersion
	}
	var copied File
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var version FileVersion
		if err := tx.Where("id = ?", *source.CurrentVersionID).First(&version).Error; err != nil {
			return err
		}
		var blob ObjectBlob
		if err := tx.Where("id = ?", version.ObjectBlobID).First(&blob).Error; err != nil {
			return err
		}
		copied = File{ID: uuid.New(), Name: n, ParentID: &parent, OwnerID: user, SpaceID: parentFile.SpaceID, Type: "file"}
		if err := tx.Create(&copied).Error; err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrConflict
			}
			return err
		}
		if err := tx.Model(&ObjectBlob{}).Where("id = ?", blob.ID).UpdateColumn("ref_count", gorm.Expr("ref_count + 1")).Error; err != nil {
			return err
		}
		newVersion := FileVersion{ID: uuid.New(), FileID: copied.ID, Version: 1, ObjectBlobID: blob.ID, ContentSHA256: version.ContentSHA256, Size: version.Size, Comment: version.Comment, UserID: user}
		if err := tx.Create(&newVersion).Error; err != nil {
			return err
		}
		copied.CurrentVersionID = &newVersion.ID
		return tx.Model(&copied).Update("current_version_id", newVersion.ID).Error
	})
	return copied, err
}

// copyFolderMaxEntries 单次目录子树复制的最大条目数（目录与文件合计）。
const copyFolderMaxEntries = 2000

// CopyFolder 把 source 目录子树整体复制到 parent 下（name 缺省「<原名> copy」）。
// 授权同 Copy：源读权限（Get）+ 目标目录写权限（authorizeParentFolder）——
// 跨空间复制时子树继承目标空间（与移动的继承语义一致）。文件经 blob
// ref_count 复用当前版本；无当前版本的文件跳过。单事务递归；条目数超限
// （ErrCopyLimit）或目标深度超限（ErrFolderDepth）时整体回滚；同名冲突
// 返回 ErrConflict。
func (s *Store) CopyFolder(user, id, parent uuid.UUID, name string) (File, error) {
	source, err := s.Get(user, id)
	if err != nil {
		return File{}, err
	}
	if source.Type != "folder" || source.IsRoot {
		return File{}, ErrFolderCopy
	}
	parentFile, err := authorizeParentFolder(s, user, parent, s.spaceWriter, s.acl)
	if err != nil {
		return File{}, err
	}
	if name == "" {
		name = source.Name + " copy"
	}
	n, err := NormalizeName(name)
	if err != nil {
		return File{}, err
	}
	// 跨空间复制校验目标空间配额：源子树全部文件当前版本字节合计。
	if parentFile.SpaceID != source.SpaceID {
		if bytes, serr := s.subtreeBytes(source.ID); serr == nil {
			if err := s.checkSpaceQuota(parentFile.SpaceID, bytes); err != nil {
				return File{}, err
			}
		} else if !errors.Is(serr, ErrNotFound) {
			return File{}, serr
		}
	}
	targetDepth, derr := s.folderDepthOf(parent)
	if derr != nil {
		return File{}, derr
	}
	height, herr := s.folderSubtreeHeight(source.ID)
	if herr != nil {
		return File{}, herr
	}
	if verr := validateMoveDepth(targetDepth, height, s.effectiveMaxFolderDepth()); verr != nil {
		return File{}, verr
	}
	var copied File
	count := 0
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var cerr error
		copied, cerr = copyFolderTree(tx, source, parent, n, parentFile.SpaceID, user, &count)
		return cerr
	})
	if err != nil {
		return File{}, err
	}
	return copied, nil
}

// copyFolderTree 事务内递归复制目录子树：目录行逐层新建（继承目标空间），
// 文件行复制当前版本（blob ref_count+1，与单文件 Copy 同语义）；
// 子项按 lower(name) 排序复制（结果顺序稳定）；软删子项排除。
func copyFolderTree(tx *gorm.DB, src File, parentID uuid.UUID, name string, spaceID uuid.UUID, user uuid.UUID, count *int) (File, error) {
	*count++
	if *count > copyFolderMaxEntries {
		return File{}, ErrCopyLimit
	}
	dest := File{ID: uuid.New(), Name: name, ParentID: &parentID, OwnerID: user, SpaceID: spaceID, Type: "folder", Description: src.Description}
	if err := tx.Create(&dest).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return File{}, ErrConflict
		}
		return File{}, err
	}
	var children []File
	if err := tx.Where("parent_id = ? AND deleted_at IS NULL", src.ID).Order("lower(name)").Find(&children).Error; err != nil {
		return File{}, err
	}
	for _, ch := range children {
		if ch.Type == "folder" {
			if _, err := copyFolderTree(tx, ch, dest.ID, ch.Name, spaceID, user, count); err != nil {
				return File{}, err
			}
			continue
		}
		if ch.CurrentVersionID == nil {
			continue
		}
		if _, err := copyFileRow(tx, ch, dest.ID, spaceID, user); err != nil {
			return File{}, err
		}
	}
	return dest, nil
}

// copyFileRow 复制单个文件行（当前版本 + blob 引用计数），与 Store.Copy
// 的事务体一致；供目录递归复制复用。
func copyFileRow(tx *gorm.DB, src File, parentID uuid.UUID, spaceID uuid.UUID, user uuid.UUID) (File, error) {
	var version FileVersion
	if err := tx.Where("id = ?", *src.CurrentVersionID).First(&version).Error; err != nil {
		return File{}, err
	}
	var blob ObjectBlob
	if err := tx.Where("id = ?", version.ObjectBlobID).First(&blob).Error; err != nil {
		return File{}, err
	}
	copied := File{ID: uuid.New(), Name: src.Name, ParentID: &parentID, OwnerID: user, SpaceID: spaceID, Type: "file"}
	if err := tx.Create(&copied).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return File{}, ErrConflict
		}
		return File{}, err
	}
	if err := tx.Model(&ObjectBlob{}).Where("id = ?", blob.ID).UpdateColumn("ref_count", gorm.Expr("ref_count + 1")).Error; err != nil {
		return File{}, err
	}
	newVersion := FileVersion{ID: uuid.New(), FileID: copied.ID, Version: 1, ObjectBlobID: blob.ID, ContentSHA256: version.ContentSHA256, Size: version.Size, Comment: version.Comment, UserID: user}
	if err := tx.Create(&newVersion).Error; err != nil {
		return File{}, err
	}
	copied.CurrentVersionID = &newVersion.ID
	return copied, tx.Model(&File{}).Where("id = ?", copied.ID).Update("current_version_id", newVersion.ID).Error
}

func (s *Store) Recent(owner uuid.UUID, limit int) ([]File, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var out []File
	cond, args := readableScopeSQL(owner)
	err := s.db.
		Where("deleted_at IS NULL AND last_access_at IS NOT NULL AND current_version_id IS NOT NULL AND "+cond, args...).
		Where("EXISTS (SELECT 1 FROM file_versions fv JOIN object_blobs ob ON ob.id = fv.object_blob_id WHERE fv.id = files.current_version_id AND fv.file_id = files.id AND ob.status = ?)", BlobStatusAvailable).
		Order("last_access_at DESC, id DESC").Limit(limit).Find(&out).Error
	return out, err
}

// Delete 软删除文件（移入回收站；根目录不可删）。
// 文件行 owner 短路；其余走空间 CanDelete（authorizeSpaceDelete）。
func (s *Store) Delete(user, id uuid.UUID) error {
	var f File
	if err := s.db.Where("id = ? AND deleted_at IS NULL", id).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if f.IsRoot {
		return ErrRoot
	}
	if err := authorizeSpaceDelete(f, user, s.spaceDeleter, s.acl); err != nil {
		return err
	}
	return s.db.Model(&f).Update("deleted_at", gorm.Expr("CURRENT_TIMESTAMP")).Error
}
