package files

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrInvalidName = errors.New("invalid name")
	ErrConflict    = errors.New("name conflict")
	ErrNotFound    = errors.New("file not found")
	ErrRoot        = errors.New("root cannot be changed")
	ErrNoVersion   = errors.New("file has no current version")
	ErrFolderCopy  = errors.New("folders cannot be copied")
	// ErrForbidden 表示用户对团队目录无写权限（如 viewer 或非成员）。
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

// TeamWriter 判定用户能否写入团队空间（owner/editor/含 write 权限的自定义角色），
// 由 team 包注入实现；未来可替换为 Casbin 等策略引擎。nil 表示团队权限源未配置
// （拒绝团队目录写入）。
type TeamWriter func(userID, teamID uuid.UUID) (bool, error)

// TeamReader 判定用户是否可读取团队空间（成员 + read 权限），由 team 包注入实现。
// nil 表示成员判定源未配置（拒绝团队文件读取，安全默认）。
type TeamReader func(userID, teamID uuid.UUID) (bool, error)

// TeamDeleter 判定用户能否删除团队文件（CanDelete：系统仅 owner、自定义角色
// 按 delete 勾选且未被 deny），由 team 包注入实现；nil 表示未配置（拒绝，安全默认）。
type TeamDeleter func(userID, teamID uuid.UUID) (bool, error)

// defaultMaxFolderDepth 目录默认最大深度（根为 1；可经
// SetMaxFolderDepthProvider / folder.max_depth 覆盖）。
const defaultMaxFolderDepth = 32

type Store struct {
	db          *gorm.DB
	teamWriter  TeamWriter
	teamReader  TeamReader
	teamDeleter TeamDeleter
	maxVersions int
	// maxVersionsFn 为版本保留数的运行时提供器（settings 热读取）；nil 时用 maxVersions。
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
	// main 注入团队 file.updated 通知逻辑；nil 表示未接线。回调不改变
	// 版本写入结果（错误由注入方自理）。
	versionNotify VersionNotifyFunc
	// versionDeletedNotify 历史版本删除完成回调（DeleteVersion 事务提交后
	// 调用）：main 注入团队 file.version.deleted 通知逻辑；nil 表示未接线。
	versionDeletedNotify VersionNotifyFunc
}

// VersionNotifyFunc 版本写入完成回调：f 为目标文件行、actor 为写入者、
// version 为新落库的版本（事务已提交）。main 接线：团队文件且 actor≠owner
// 时通知团队其他成员 file.updated。
type VersionNotifyFunc func(f File, actor uuid.UUID, version FileVersion)

func NewStore(db *gorm.DB) *Store {
	return &Store{db: db, maxVersions: defaultMaxVersions, maxFolderDepth: defaultMaxFolderDepth}
}

// SetTeamWriter 注入团队写权限判定器（幂等）。
func (s *Store) SetTeamWriter(w TeamWriter) {
	if w != nil {
		s.teamWriter = w
	}
}

// SetTeamReader 注入团队成员读判定器（幂等）。
func (s *Store) SetTeamReader(r TeamReader) {
	if r != nil {
		s.teamReader = r
	}
}

// SetTeamDeleter 注入团队删除权限判定器（幂等）。
func (s *Store) SetTeamDeleter(d TeamDeleter) {
	if d != nil {
		s.teamDeleter = d
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
// 调用（团队 file.updated 通知的接线点）；回调自行决定同步/异步执行策略。
func (s *Store) SetNotifyDispatcher(fn VersionNotifyFunc) {
	if fn != nil {
		s.versionNotify = fn
	}
}

// SetVersionDeletedDispatcher 注入历史版本删除完成回调（幂等）：DeleteVersion
// 事务提交后调用（团队 file.version.deleted 通知文件 owner 的接线点）；
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

// getFolder 返回未删除的目录（不限 owner，供团队/个人作用域分支判定）。
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

// teamScope 返回目录的团队作用域；非团队目录返回 nil。
func teamScope(f File) *uuid.UUID {
	if f.ScopeType == "team" && f.TeamID != nil {
		return f.TeamID
	}
	return nil
}

// authorizeParentFolder 判定 user 能否在 parent 下创建内容：
// 个人目录要求 owner；团队目录要求成员写权限（owner/editor，viewer 403）。
func authorizeParentFolder(repo folderAccessor, user, parent uuid.UUID, canWriteTeam TeamWriter) (File, error) {
	p, err := repo.getFolder(parent)
	if err != nil {
		return File{}, err
	}
	if teamID := teamScope(p); teamID != nil {
		if canWriteTeam == nil {
			return File{}, ErrForbidden
		}
		ok, werr := canWriteTeam(user, *teamID)
		if werr != nil {
			return File{}, werr
		}
		if !ok {
			return File{}, ErrForbidden
		}
		return p, nil
	}
	if p.OwnerID != user {
		return File{}, ErrNotFound
	}
	return p, nil
}

// ValidateFolder 校验 user 可在 parent 下上传/创建内容。
// 个人目录 owner 可写；团队目录成员 editor/owner 可写，viewer 与非成员 403。
func (s *Store) ValidateFolder(user, parent uuid.UUID) error {
	_, err := authorizeParentFolder(s, user, parent, s.teamWriter)
	return err
}

// authorizeFileAccess 判定 user 能否读取 file（元数据/当前版本/下载/预览共用入口）：
// 个人文件仅 owner；团队文件（scope_type='team'）经注入的 CanRead 判定
// （系统角色任意在册成员可读；自定义角色按 read 勾选且未被 deny）。
// 未注入判定器时团队文件一律拒绝（ErrForbidden，与写路径一致的安全默认）。
// 非授权访问统一返回 ErrNotFound，不泄露资源存在性；owner 判定短路，行为与旧版一致。
func authorizeFileAccess(f File, user uuid.UUID, isMember TeamReader) error {
	if f.OwnerID == user {
		return nil
	}
	teamID := teamScope(f)
	if teamID == nil {
		return ErrNotFound
	}
	if isMember == nil {
		return ErrForbidden
	}
	ok, err := isMember(user, *teamID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// Get 返回 user 可访问的未删除单个文件（authorizeFileAccess 统一判定）：
// 个人文件 owner 可读；团队文件任意在册成员可读（下载/预览/元数据同规则）。
func (s *Store) Get(user, id uuid.UUID) (File, error) {
	var f File
	if err := s.db.Where("id = ? AND deleted_at IS NULL", id).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	if err := authorizeFileAccess(f, user, s.teamReader); err != nil {
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
// （owner 或团队在册成员，经 authorizeFileAccess 判定），且文件未删除时生效。
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
// （owner 或团队在册成员，经 authorizeFileAccess 判定），且文件未删除时生效。
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
func (s *Store) EnsureRoot(owner uuid.UUID) (File, error) {
	var root File
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("owner_id = ? AND is_root = true", owner).First(&root).Error; err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		root = File{ID: uuid.New(), Name: "根目录", OwnerID: owner, Type: "folder", IsRoot: true, ScopeType: "personal"}
		if err := tx.Create(&root).Error; err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return tx.Where("owner_id = ? AND is_root = true", owner).First(&root).Error
			}
			return err
		}
		return nil
	})
	return root, err
}
func (s *Store) List(owner uuid.UUID, parent *uuid.UUID, limit int, sort SortOptions) ([]File, error) {
	if sort.Sort == "" {
		sort.Sort = "name"
	}
	clause, ok := SortClause(sort.Sort, sort.Order)
	if !ok {
		clause = "lower(name) ASC, id ASC"
	}
	var out []File
	q := s.db.Where("owner_id = ? AND deleted_at IS NULL", owner)
	if parent == nil {
		q = q.Where("is_root = true")
	} else {
		q = q.Where("parent_id = ? AND is_root = false", *parent)
	}
	err := q.Order(clause).Limit(limit).Find(&out).Error
	return out, err
}
func (s *Store) CreateFolder(owner, parent uuid.UUID, name string) (File, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return File{}, err
	}
	var parentFile File
	if err := s.db.Where("id = ? AND owner_id = ? AND type = 'folder' AND deleted_at IS NULL", parent, owner).First(&parentFile).Error; err != nil {
		return File{}, ErrNotFound
	}
	if depth, derr := s.folderDepthOf(parent); derr == nil {
		if verr := validateCreateDepth(depth, s.effectiveMaxFolderDepth()); verr != nil {
			return File{}, verr
		}
	} else if !errors.Is(derr, ErrNotFound) {
		return File{}, derr
	}
	f := File{ID: uuid.New(), Name: n, ParentID: &parent, OwnerID: owner, Type: "folder", ScopeType: "personal"}
	err = s.db.Create(&f).Error
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return File{}, ErrConflict
	}
	return f, err
}

// CreateFolderIn 在 parent 下创建子目录并继承其作用域：
// 个人目录要求 owner；团队目录要求成员写权限（owner/editor，viewer 403）。
// 深度校验（folder.max_depth）：parent 深度 + 1 不得超过上限。
func (s *Store) CreateFolderIn(user, parent uuid.UUID, name string) (File, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return File{}, err
	}
	p, err := authorizeParentFolder(s, user, parent, s.teamWriter)
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
	f := File{ID: uuid.New(), Name: n, ParentID: &parent, OwnerID: user, Type: "folder", ScopeType: "personal"}
	if id := teamScope(p); id != nil {
		f.ScopeType, f.TeamID = "team", id
	}
	err = s.db.Create(&f).Error
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return File{}, ErrConflict
	}
	return f, err
}

// TeamRoot 返回团队根目录（scope_type='team'、is_root=true）；不存在或已删除返回 ErrNotFound。
func (s *Store) TeamRoot(teamID uuid.UUID) (File, error) {
	var f File
	if err := s.db.Where("team_id = ? AND is_root = true AND deleted_at IS NULL", teamID).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	return f, nil
}

// GetTeamFolder 返回团队名下未删除的目录（含根目录）；读权限由调用方校验成员身份。
func (s *Store) GetTeamFolder(teamID, id uuid.UUID) (File, error) {
	var f File
	if err := s.db.Where("id = ? AND team_id = ? AND type = 'folder' AND deleted_at IS NULL", id, teamID).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	return f, nil
}

// TeamListFilter 团队目录列举过滤：tag（可选）/收藏（可选）+ 排序。
type TeamListFilter struct {
	TagID   *uuid.UUID
	Starred *bool
	SortOptions
}

// ListTeam 列出团队目录内容（根目录或子目录）；成员读权限由调用方（HTTP 层）校验。
// tag_id/starred 过滤在目录范围内生效（EXISTS file_tags / is_starred）。
func (s *Store) ListTeam(teamID, parent uuid.UUID, limit int, f TeamListFilter) ([]File, error) {
	if f.Sort == "" {
		f.Sort = "name"
	}
	clause, ok := SortClause(f.Sort, f.Order)
	if !ok {
		clause = "lower(name) ASC, id ASC"
	}
	q := s.db.Where("team_id = ? AND parent_id = ? AND is_root = false AND deleted_at IS NULL", teamID, parent)
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

// authorizeTeamDelete 判定 user 能否删除 file（软删除/彻底删除共用）：
// 个人文件仅 owner（非 owner 统一 ErrNotFound，不泄露存在性）；
// 团队文件要求 CanDelete（系统仅 owner；自定义角色按 delete 勾选且未被 deny）——
// 文件行 owner（上传者）不短路：delete 是独立于 write 的权限（设计 6.5.2）。
func authorizeTeamDelete(f File, user uuid.UUID, canDeleteTeam TeamDeleter) error {
	teamID := teamScope(f)
	if teamID == nil {
		if f.OwnerID != user {
			return ErrNotFound
		}
		return nil
	}
	if canDeleteTeam == nil {
		return ErrForbidden
	}
	ok, err := canDeleteTeam(user, *teamID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// Rename 重命名文件或目录（根目录不可改名）。
// 个人文件仅 owner；团队文件走 CanWrite（authorizeFileWrite：文件行 owner 短路，
// 其余成员 owner/editor/含 write 权限的自定义角色可改）。
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
	if err := authorizeFileWrite(f, user, s.teamWriter); err != nil {
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
	p, err := authorizeParentFolder(s, owner, parent, s.teamWriter)
	if err != nil {
		return uuid.Nil, false, err
	}
	// 团队目录下创建的文件继承团队作用域；个人目录保持 personal。
	scopeType, teamID := "personal", (*uuid.UUID)(nil)
	if id := teamScope(p); id != nil {
		scopeType, teamID = "team", id
	}
	var created uuid.UUID
	var newBlob bool
	err = s.db.Transaction(func(tx *gorm.DB) error {
		f := File{ID: uuid.New(), Name: n, ParentID: &parent, OwnerID: owner, TeamID: teamID, Type: "file", ScopeType: scopeType}
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
		return File{}, ErrFolderCopy
	}
	parentFile, err := authorizeParentFolder(s, user, parent, s.teamWriter)
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
		copied = File{ID: uuid.New(), Name: n, ParentID: &parent, OwnerID: user, Type: "file", ScopeType: parentFile.ScopeType, TeamID: parentFile.TeamID}
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

func (s *Store) Recent(owner uuid.UUID, limit int) ([]File, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var out []File
	err := s.db.Where("owner_id = ? AND deleted_at IS NULL AND last_access_at IS NOT NULL", owner).Order("last_access_at DESC, id DESC").Limit(limit).Find(&out).Error
	return out, err
}

// Delete 软删除文件（移入回收站；根目录不可删）。
// 个人文件仅 owner；团队文件走 CanDelete（authorizeTeamDelete，含自定义角色）。
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
	if err := authorizeTeamDelete(f, user, s.teamDeleter); err != nil {
		return err
	}
	return s.db.Model(&f).Update("deleted_at", gorm.Expr("CURRENT_TIMESTAMP")).Error
}
