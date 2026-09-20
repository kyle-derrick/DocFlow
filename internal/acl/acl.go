// Package acl 提供空间的路径级访问控制：folder_acl 每行一条
// (folder, subject, effect, permissions) 规则。求值时沿 parent 链自目标
// 文件/目录向上到空间根收集全部条目（由近及远），规则为最近节点优先；
// 同节点内 user 条目优先于 space 条目、同主体 deny 优先于 allow；链上无
// 适用条目（matched=false）时回退空间角色判定（files/share 包经注入的
// ACLResolver 接线，未注入或无条目时行为与既有权限完全一致）。
package acl

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

var (
	// ErrNotFound 目录不存在（或已软删）。
	ErrNotFound = errors.New("folder not found")
	// ErrNotSpaceFolder ACL 仅支持空间作用域目录（统一空间模型下恒满足，
	// 保留判定以防御无空间归属的脏数据）。
	ErrNotSpaceFolder = errors.New("acl is only available for space folders")
	// ErrInvalidEntry 条目字段非法（subject/effect/permissions）。
	ErrInvalidEntry = errors.New("invalid acl entry")
	// ErrDuplicateEntry 同一请求中 (subject_type, subject_id) 重复。
	ErrDuplicateEntry = errors.New("duplicate acl subject")
)

// Subject/effect 取值（与 folder_acl CHECK 约束一致，migration 040 起
// subject_type 为 user/space）。
const (
	SubjectUser  = "user"
	SubjectSpace = "space"
	EffectAllow  = "allow"
	EffectDeny   = "deny"
)

// ValidPermissions 全部合法权限动作（folder_acl.permissions CHECK 一致；
// 不含空间管理动作——角色/成员管理恒走空间角色判定）。
var ValidPermissions = []string{"read", "write", "delete", "share"}

// Entry 对应 folder_acl 一行。
type Entry struct {
	ID          uuid.UUID `json:"-"`
	FolderID    uuid.UUID `json:"-"`
	SubjectType string    `json:"subject_type"`
	SubjectID   uuid.UUID `json:"subject_id"`
	Effect      string    `json:"effect"`
	Permissions []string  `json:"permissions"`
	CreatedBy   uuid.UUID `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

// EntryInput PUT /folders/:id/acl 的条目入参（ID/时间戳由服务端生成）。
type EntryInput struct {
	SubjectType string
	SubjectID   uuid.UUID
	Effect      string
	Permissions []string
}

// ChainNode 求值链上单个节点：一个目录及其全部 ACL 条目。
type ChainNode struct {
	FolderID uuid.UUID
	Entries  []Entry
}

// Repo 抽象 folder_acl 的持久化与链查询；GormRepo 为 PostgreSQL 实现，
// MemoryRepo 供测试使用。
type Repo interface {
	// Folder 返回未删除目录行（不限作用域）；不存在返回 ErrNotFound。
	Folder(id uuid.UUID) (files.File, error)
	// ListByFolder 返回目录的全部条目（created_at, id 稳定排序）。
	ListByFolder(folderID uuid.UUID) ([]Entry, error)
	// Replace 整体替换目录条目（事务内删旧插新；空数组即清空）。
	Replace(folderID uuid.UUID, entries []Entry) error
	// ChainForFile 沿 parent 链自 file/folder 向上到空间根收集条目，
	// 返回有序由近及远（文件自身无条目，链自其父目录开始）。
	ChainForFile(fileOrFolderID uuid.UUID) ([]ChainNode, error)
}

// Service 提供 ACL 求值（授权链接线）与条目管理（HTTP 端点）。
type Service struct {
	repo Repo
	now  func() time.Time
}

func NewService(repo Repo) *Service {
	return &Service{repo: repo, now: time.Now}
}

// ResolveForFile 求值用户对空间作用域文件/目录的 perm 权限：
// 链查询 + Resolve 求值；目标行不存在时返回空链（matched=false，
// 调用方回退空间角色判定——授权链在文件行上已先行校验存在性）。
func (s *Service) ResolveForFile(fileOrFolderID, spaceID, userID uuid.UUID, perm string) (bool, bool, error) {
	chain, err := s.repo.ChainForFile(fileOrFolderID)
	if err != nil {
		return false, false, err
	}
	allowed, matched := Resolve(chain, userID, spaceID, perm)
	return allowed, matched, nil
}

// List 返回目录行与其当前 ACL 条目（GET /folders/:id/acl）。
// 目录须为空间作用域（ErrNotSpaceFolder），由 HTTP 层映射 400。
func (s *Service) List(folderID uuid.UUID) (files.File, []Entry, error) {
	f, err := s.repo.Folder(folderID)
	if err != nil {
		return files.File{}, nil, err
	}
	if !isSpaceFolder(f) {
		return files.File{}, nil, ErrNotSpaceFolder
	}
	entries, err := s.repo.ListByFolder(folderID)
	if err != nil {
		return files.File{}, nil, err
	}
	return f, entries, nil
}

// Replace 整体替换目录的 ACL 条目（PUT /folders/:id/acl）：
// 校验目录为空间作用域、条目合法且 (subject_type, subject_id) 唯一后，
// 事务内删旧插新；返回目录行（供审计/响应）。
func (s *Service) Replace(folderID uuid.UUID, inputs []EntryInput, actor uuid.UUID) (files.File, error) {
	f, err := s.repo.Folder(folderID)
	if err != nil {
		return files.File{}, err
	}
	if !isSpaceFolder(f) {
		return files.File{}, ErrNotSpaceFolder
	}
	now := s.now()
	entries := make([]Entry, 0, len(inputs))
	seen := make(map[string]bool, len(inputs))
	for _, in := range inputs {
		if err := validateInput(in); err != nil {
			return files.File{}, err
		}
		key := in.SubjectType + ":" + in.SubjectID.String()
		if seen[key] {
			return files.File{}, ErrDuplicateEntry
		}
		seen[key] = true
		entries = append(entries, Entry{
			ID: uuid.New(), FolderID: folderID, SubjectType: in.SubjectType,
			SubjectID: in.SubjectID, Effect: in.Effect, Permissions: in.Permissions,
			CreatedBy: actor, CreatedAt: now,
		})
	}
	if err := s.repo.Replace(folderID, entries); err != nil {
		return files.File{}, err
	}
	return f, nil
}

// validateInput 校验单条入参：subject/effect 合法、permissions 非空且
// ⊆ ValidPermissions（fail closed，未知动作一律拒绝）。
func validateInput(in EntryInput) error {
	if in.SubjectType != SubjectUser && in.SubjectType != SubjectSpace {
		return ErrInvalidEntry
	}
	if in.SubjectID == uuid.Nil {
		return ErrInvalidEntry
	}
	if in.Effect != EffectAllow && in.Effect != EffectDeny {
		return ErrInvalidEntry
	}
	if len(in.Permissions) == 0 {
		return ErrInvalidEntry
	}
	valid := make(map[string]bool, len(ValidPermissions))
	for _, p := range ValidPermissions {
		valid[p] = true
	}
	for _, p := range in.Permissions {
		if !valid[p] {
			return ErrInvalidEntry
		}
	}
	return nil
}

func isSpaceFolder(f files.File) bool {
	return f.Type == "folder" && f.SpaceID != uuid.Nil
}

// Resolve 求值路径级 ACL 链（纯函数，规则见包注释）：
//   - 仅 permissions 含 perm 的条目参与求值；
//   - 节点由近及远遍历，首个有适用条目的节点决定结果（近覆盖远）；
//   - 同一节点内 user 条目优先于 space 条目；同主体 deny 优先于 allow
//     （UNIQUE(folder_id, subject_type, subject_id) 保证同主体仅一条，
//     deny/allow 并存只出现在 user 与 space 之间，按 user 优先裁断）；
//   - 链为空或无适用条目时 matched=false（回退空间角色判定）。
func Resolve(chain []ChainNode, subjectUserID, spaceID uuid.UUID, perm string) (allowed, matched bool) {
	for _, node := range chain {
		var userDeny, userAllow, spaceDeny, spaceAllow bool
		for _, e := range node.Entries {
			if !permIncludes(e.Permissions, perm) {
				continue
			}
			switch {
			case e.SubjectType == SubjectUser && e.SubjectID == subjectUserID && e.Effect == EffectDeny:
				userDeny = true
			case e.SubjectType == SubjectUser && e.SubjectID == subjectUserID:
				userAllow = true
			case e.SubjectType == SubjectSpace && e.SubjectID == spaceID && e.Effect == EffectDeny:
				spaceDeny = true
			case e.SubjectType == SubjectSpace && e.SubjectID == spaceID:
				spaceAllow = true
			}
		}
		switch {
		case userDeny:
			return false, true
		case userAllow:
			return true, true
		case spaceDeny:
			return false, true
		case spaceAllow:
			return true, true
		}
	}
	return false, false
}

// permIncludes 判定条目权限数组是否包含 perm（元素均为固定权限词，直接比较）。
func permIncludes(perms []string, perm string) bool {
	for _, p := range perms {
		if p == perm {
			return true
		}
	}
	return false
}
