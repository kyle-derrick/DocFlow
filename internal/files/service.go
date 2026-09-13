package files

import (
	"errors"
	"strings"
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
	// ErrForbidden 表示用户对团队目录无写权限（如 viewer 或非成员）。
	ErrForbidden = errors.New("no permission to write this folder")
)

func NormalizeName(name string) (string, error) {
	name = norm.NFC.String(name)
	for _, r := range name {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return "", ErrInvalidName
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || len([]rune(name)) > 255 {
		return "", ErrInvalidName
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	reserved := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}
	if reserved[base] {
		return "", ErrInvalidName
	}
	return name, nil
}

// TeamWriter 判定用户能否写入团队空间（owner/editor），由 team 包注入实现；
// 未来可替换为 Casbin 等策略引擎。nil 表示团队权限源未配置（拒绝团队目录写入）。
type TeamWriter func(userID, teamID uuid.UUID) (bool, error)

// TeamReader 判定用户是否为团队在册成员（读权限，任意角色），由 team 包注入实现。
// nil 表示成员判定源未配置（拒绝团队文件读取，安全默认）。
type TeamReader func(userID, teamID uuid.UUID) (bool, error)

type Store struct {
	db          *gorm.DB
	teamWriter  TeamWriter
	teamReader  TeamReader
	maxVersions int
	// maxVersionsFn 为版本保留数的运行时提供器（settings 热读取）；nil 时用 maxVersions。
	maxVersionsFn func() int
}

func NewStore(db *gorm.DB) *Store { return &Store{db: db, maxVersions: defaultMaxVersions} }

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
// 个人文件仅 owner；团队文件（scope_type='team'）任意在册成员可读（viewer 同样可读）。
// 未注入成员判定器时团队文件一律拒绝（ErrForbidden，与写路径一致的安全默认）。
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
func (s *Store) List(owner uuid.UUID, parent *uuid.UUID, limit int) ([]File, error) {
	var out []File
	q := s.db.Where("owner_id = ? AND deleted_at IS NULL", owner)
	if parent == nil {
		q = q.Where("is_root = true")
	} else {
		q = q.Where("parent_id = ? AND is_root = false", *parent)
	}
	err := q.Order("lower(name), id").Limit(limit).Find(&out).Error
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
	f := File{ID: uuid.New(), Name: n, ParentID: &parent, OwnerID: owner, Type: "folder", ScopeType: "personal"}
	err = s.db.Create(&f).Error
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return File{}, ErrConflict
	}
	return f, err
}

// CreateFolderIn 在 parent 下创建子目录并继承其作用域：
// 个人目录要求 owner；团队目录要求成员写权限（owner/editor，viewer 403）。
func (s *Store) CreateFolderIn(user, parent uuid.UUID, name string) (File, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return File{}, err
	}
	p, err := authorizeParentFolder(s, user, parent, s.teamWriter)
	if err != nil {
		return File{}, err
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

// ListTeam 列出团队目录内容（根目录或子目录）；成员读权限由调用方（HTTP 层）校验。
func (s *Store) ListTeam(teamID, parent uuid.UUID, limit int) ([]File, error) {
	var out []File
	err := s.db.Where("team_id = ? AND parent_id = ? AND is_root = false AND deleted_at IS NULL", teamID, parent).
		Order("lower(name), id").Limit(limit).Find(&out).Error
	return out, err
}
func (s *Store) Rename(owner, id uuid.UUID, name string) (File, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return File{}, err
	}
	var f File
	if err = s.db.Where("id = ? AND owner_id = ? AND deleted_at IS NULL", id, owner).First(&f).Error; err != nil {
		return File{}, ErrNotFound
	}
	if f.IsRoot {
		return File{}, ErrRoot
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

// CreateUploadedFile 在 parent 下落库上传完成的文件（含 blob 与 v1 版本），
// 返回新建文件 ID（供上传完成钩子等后置处理使用）。
func (s *Store) CreateUploadedFile(owner, parent uuid.UUID, name, storageKey string, size int64, sha256 string, mimeType string) (uuid.UUID, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return uuid.Nil, err
	}
	p, err := authorizeParentFolder(s, owner, parent, s.teamWriter)
	if err != nil {
		return uuid.Nil, err
	}
	// 团队目录下创建的文件继承团队作用域；个人目录保持 personal。
	scopeType, teamID := "personal", (*uuid.UUID)(nil)
	if id := teamScope(p); id != nil {
		scopeType, teamID = "team", id
	}
	var created uuid.UUID
	err = s.db.Transaction(func(tx *gorm.DB) error {
		f := File{ID: uuid.New(), Name: n, ParentID: &parent, OwnerID: owner, TeamID: teamID, Type: "file", ScopeType: scopeType}
		if err := tx.Create(&f).Error; err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrConflict
			}
			return err
		}
		created = f.ID
		blob := ObjectBlob{ID: uuid.New(), SHA256: sha256, StorageKey: storageKey, Size: size, MimeType: mimeType, RefCount: 1, Status: "available"}
		if err := tx.Create(&blob).Error; err != nil {
			return err
		}
		version := FileVersion{ID: uuid.New(), FileID: f.ID, Version: 1, ObjectBlobID: blob.ID, ContentSHA256: sha256, Size: size, UserID: owner}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		return tx.Model(&f).Update("current_version_id", version.ID).Error
	})
	if err != nil {
		return uuid.Nil, err
	}
	return created, nil
}

func (s *Store) Delete(owner, id uuid.UUID) error {
	var f File
	if err := s.db.Where("id = ? AND owner_id = ? AND deleted_at IS NULL", id, owner).First(&f).Error; err != nil {
		return ErrNotFound
	}
	if f.IsRoot {
		return ErrRoot
	}
	return s.db.Model(&f).Update("deleted_at", gorm.Expr("CURRENT_TIMESTAMP")).Error
}
