package tagging

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"

	"github.com/docflow/docflow/internal/files"
)

var (
	// ErrInvalidName 标签名非法（空、超 64 rune 或含控制字符）。
	ErrInvalidName = errors.New("invalid tag name")
	// ErrNotFound 标签不存在（或不属于当前用户，不泄露存在性）。
	ErrNotFound = errors.New("tag not found")
	// ErrConflict 同名标签已存在（每用户内唯一）。
	ErrConflict = errors.New("tag already exists")
)

// NormalizeTagName 标签名归一：NFC、去首尾空白、拒绝控制字符、
// 非空且 ≤64 rune（与 files.NormalizeName 同思路，但不限制路径字符）。
func NormalizeTagName(name string) (string, error) {
	name = norm.NFC.String(name)
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", ErrInvalidName
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return "", ErrInvalidName
	}
	return name, nil
}

// FileSource 抽象标签服务对文件元数据的读访问（授权由实现保证）：
// 生产实现为 *files.Store（Get 即 authorizeFileAccess：个人 owner、
// 团队任意在册成员可读）。
type FileSource interface {
	Get(user, id uuid.UUID) (files.File, error)
}

// Repo 是标签持久化接口；GormRepo 为 PostgreSQL 实现，MemoryRepo 供测试使用。
type Repo interface {
	// CreateTag 新建标签；(user_id, name) 冲突返回 ErrConflict。
	CreateTag(t Tag) error
	// GetTag 按 ID 返回标签；不存在返回 ErrNotFound。
	GetTag(id uuid.UUID) (Tag, error)
	// ListTags 返回 user 的全部标签，按名称排序。
	ListTags(user uuid.UUID) ([]Tag, error)
	// DeleteTag 删除标签行（file_tags 由 FK 级联清理）。
	DeleteTag(id uuid.UUID) error
	// FileTagExists 判断关联是否已存在。
	FileTagExists(tagID, fileID uuid.UUID) (bool, error)
	// AddFileTag 写入关联；已存在（复合主键冲突）返回 ErrConflict。
	AddFileTag(ft FileTag) error
	// RemoveFileTag 删除关联，返回是否存在（幂等删除）。
	RemoveFileTag(tagID, fileID uuid.UUID) (bool, error)
	// ListFileTags 返回 user 拥有且挂在 fileID 上的标签，按名称排序。
	ListFileTags(user, fileID uuid.UUID) ([]Tag, error)
}

// Service 提供标签 CRUD 与文件打/去标签；file 读授权经 FileSource 判定。
type Service struct {
	repo  Repo
	files FileSource
}

func NewService(repo Repo, fileSource FileSource) *Service {
	return &Service{repo: repo, files: fileSource}
}

// ListTags 列出当前用户标签（按名称排序）。
func (s *Service) ListTags(user uuid.UUID) ([]Tag, error) {
	return s.repo.ListTags(user)
}

// CreateTag 创建标签（同名冲突 409）。
func (s *Service) CreateTag(user uuid.UUID, name string) (Tag, error) {
	n, err := NormalizeTagName(name)
	if err != nil {
		return Tag{}, err
	}
	now := time.Now().UTC()
	t := Tag{ID: uuid.New(), UserID: user, Name: n, CreatedAt: now}
	if err := s.repo.CreateTag(t); err != nil {
		return Tag{}, err
	}
	return t, nil
}

// GetOwnedTag 返回属于 user 的标签；他人标签按不存在处理（不泄露存在性）。
func (s *Service) GetOwnedTag(user, id uuid.UUID) (Tag, error) {
	t, err := s.repo.GetTag(id)
	if err != nil {
		return Tag{}, err
	}
	if t.UserID != user {
		return Tag{}, ErrNotFound
	}
	return t, nil
}

// DeleteTag 删除自己的标签并解除其全部文件关联（file_tags 级联）。
func (s *Service) DeleteTag(user, id uuid.UUID) error {
	if _, err := s.GetOwnedTag(user, id); err != nil {
		return err
	}
	return s.repo.DeleteTag(id)
}

// AddFileTag 把 user 自己的标签打到 fileID 上（读权限即可：标签是
// 用户维度的组织视图，不改动文件内容）；已存在时幂等返回。
func (s *Service) AddFileTag(user, fileID, tagID uuid.UUID) (FileTag, error) {
	if _, err := s.GetOwnedTag(user, tagID); err != nil {
		return FileTag{}, err
	}
	if _, err := s.files.Get(user, fileID); err != nil {
		return FileTag{}, err
	}
	if exists, err := s.repo.FileTagExists(tagID, fileID); err != nil {
		return FileTag{}, err
	} else if exists {
		return FileTag{TagID: tagID, FileID: fileID}, nil
	}
	ft := FileTag{TagID: tagID, FileID: fileID, CreatedAt: time.Now().UTC()}
	if err := s.repo.AddFileTag(ft); err != nil {
		// 并发重复打标：复合主键冲突按幂等成功处理。
		if errors.Is(err, ErrConflict) {
			return ft, nil
		}
		return FileTag{}, err
	}
	return ft, nil
}

// RemoveFileTag 解除 user 自己的标签与文件的关联（幂等）。
func (s *Service) RemoveFileTag(user, fileID, tagID uuid.UUID) error {
	if _, err := s.GetOwnedTag(user, tagID); err != nil {
		return err
	}
	if _, err := s.files.Get(user, fileID); err != nil {
		return err
	}
	_, err := s.repo.RemoveFileTag(tagID, fileID)
	return err
}

// ListFileTags 列出 user 拥有且挂在文件上的标签（文件须可读）。
func (s *Service) ListFileTags(user, fileID uuid.UUID) ([]Tag, error) {
	if _, err := s.files.Get(user, fileID); err != nil {
		return nil, err
	}
	return s.repo.ListFileTags(user, fileID)
}
