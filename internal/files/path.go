package files

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
)

// 命名空间类型（resolve / raw 路径型访问）。
const (
	NamespacePersonal = "personal"
	NamespaceTeam     = "team"
)

// MaxPathSegments 路径解析的段数上限（与默认目录深度 32 对齐，含环防御语义）。
const MaxPathSegments = 32

// ErrInvalidNamespace 表示 nsType 不是 personal|team。
var ErrInvalidNamespace = errors.New("invalid namespace")

// encodedSeparators 为路由层解码一次后仍出现的百分号编码分隔符序列：
// 说明客户端试图二次注入路径分隔符（%2F/%5C 或双重编码的 %252F/%255C），
// 一律拒绝（文件名中的字面 % 不受影响——仅匹配这些完整序列）。
var encodedSeparators = []string{"%2f", "%5c", "%252f", "%255c"}

// SplitPathSegments 把 URL 路径（"/"分隔，可带首尾斜杠）拆为规范化段：
//   - 每段经 NormalizeName 同套校验（NFC、拒绝空/斜杠/反斜杠/控制字符/Cf/
//     保留设备名/尾点、≤255 rune），保证与上传建名规则一致；
//   - 路由层已做一次百分号解码，此处不再解码，且拒绝仍含 %2F/%5C/%252F/%255C
//     的段（二次编码注入防御）；
//   - 段数超过 MaxPathSegments 返回 ErrFolderDepth。
//
// 空路径（根目录）返回 nil, nil。
func SplitPathSegments(path string) ([]string, error) {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil, nil
	}
	segments := strings.Split(path, "/")
	if len(segments) > MaxPathSegments {
		return nil, ErrFolderDepth
	}
	out := make([]string, len(segments))
	for i, seg := range segments {
		lower := strings.ToLower(seg)
		for _, bad := range encodedSeparators {
			if strings.Contains(lower, bad) {
				return nil, ErrInvalidName
			}
		}
		n, err := NormalizeName(seg)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

// findChildByLowerName 返回 parent 下未删除、lower(name) 精确匹配的非根子项
// （与唯一索引 idx_files_parent_name_active 的判定口径一致）；
// 不存在返回 ErrNotFound。
func (s *Store) findChildByLowerName(parent uuid.UUID, lowerName string) (File, error) {
	var f File
	err := s.db.Where("parent_id = ? AND lower(name) = ? AND is_root = false AND deleted_at IS NULL", parent, lowerName).First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, err
	}
	return f, nil
}

// FindChildByName 返回 parent 下未删除的同名子项（名称 NFC 归一后小写匹配），
// 供 zip 解包幂等复用与目录 index.html 解析等场景；不存在返回 ErrNotFound。
func (s *Store) FindChildByName(parent uuid.UUID, name string) (File, error) {
	return s.findChildByLowerName(parent, strings.ToLower(norm.NFC.String(name)))
}

// resolvePathLogic 从 root 起逐段解析（纯逻辑，供内存单测）：
//   - 每段经 findChild 按 parent_id + lower(name) 匹配未删除子项；
//   - 中间段必须为目录（type=folder），否则 ErrNotFound（与断链同样不泄露细节）；
//   - visited 集合做环检测（脏数据 parent 指向后代时终止）；
//   - 任何断链/不存在统一 ErrNotFound，其余错误原样透传。
//
// 返回命中行与路径链（chain[0]=root，之后每段为实际命中的行，供 canonical
// path 以存储的真实名称重建）。
func resolvePathLogic(findChild func(parent uuid.UUID, lowerName string) (File, error), root File, segments []string) (File, []File, error) {
	if root.Type != "folder" {
		return File{}, nil, ErrInvalidTarget
	}
	cur := root
	chain := []File{root}
	visited := map[uuid.UUID]bool{root.ID: true}
	for i, seg := range segments {
		child, err := findChild(cur.ID, strings.ToLower(seg))
		if err != nil {
			return File{}, nil, err
		}
		if visited[child.ID] {
			return File{}, nil, ErrNotFound
		}
		if i < len(segments)-1 && child.Type != "folder" {
			return File{}, nil, ErrNotFound
		}
		visited[child.ID] = true
		chain = append(chain, child)
		cur = child
	}
	return cur, chain, nil
}

// pathEnv 抽象命名空间路径解析所需的数据访问与权限判定（Store 为生产实现，
// 闭包字段便于内存单测注入）。
type pathEnv struct {
	findChild    func(parent uuid.UUID, lowerName string) (File, error)
	personalRoot func(actor uuid.UUID) (File, error)
	teamRoot     func(teamID uuid.UUID) (File, error)
	teamReader   TeamReader
	teamWriter   TeamWriter
	acl          ACLResolver
}

// resolveNamespaceRoot 解析 actor 可访问的命名空间根目录（纯逻辑）：
// personal 要求 scopeID==actor（个人根目录，EnsureRoot 语义，缺省自动创建）；
// team 要求 actor 为该团队成员（teamReader/CanRead 判定，未接线一律拒绝
// fail closed，非成员按 ErrNotFound 不泄露存在性），根目录须存在且未删除。
func resolveNamespaceRoot(env pathEnv, actor uuid.UUID, nsType string, scopeID uuid.UUID) (File, error) {
	switch nsType {
	case NamespacePersonal:
		if scopeID != actor {
			return File{}, ErrNotFound
		}
		return env.personalRoot(actor)
	case NamespaceTeam:
		if env.teamReader == nil {
			return File{}, ErrForbidden
		}
		ok, merr := env.teamReader(actor, scopeID)
		if merr != nil {
			return File{}, merr
		}
		if !ok {
			return File{}, ErrNotFound
		}
		return env.teamRoot(scopeID)
	default:
		return File{}, ErrInvalidNamespace
	}
}

// resolveNamespacePath 解析命名空间内相对路径并做读/写授权（纯逻辑）：
// 根目录解析见 resolveNamespaceRoot；路径段校验见 SplitPathSegments；
// 解析后授权复用 authorizeFileAccess/authorizeFileWrite，不重复实现权限。
func resolveNamespacePath(env pathEnv, actor uuid.UUID, nsType string, scopeID uuid.UUID, path string, write bool) (File, []File, error) {
	root, err := resolveNamespaceRoot(env, actor, nsType, scopeID)
	if err != nil {
		return File{}, nil, err
	}
	segments, serr := SplitPathSegments(path)
	if serr != nil {
		return File{}, nil, serr
	}
	f, chain, rerr := resolvePathLogic(env.findChild, root, segments)
	if rerr != nil {
		return File{}, nil, rerr
	}
	if write {
		if err := authorizeFileWrite(f, actor, env.teamWriter, env.acl); err != nil {
			return File{}, nil, err
		}
	} else if err := authorizeFileAccess(f, actor, env.teamReader, env.acl); err != nil {
		return File{}, nil, err
	}
	return f, chain, nil
}

// pathEnvOf 构造 Store 的生产 pathEnv。
func (s *Store) pathEnvOf() pathEnv {
	return pathEnv{
		findChild:    s.findChildByLowerName,
		personalRoot: s.EnsureRoot,
		teamRoot:     s.TeamRoot,
		teamReader:   s.teamReader,
		teamWriter:   s.teamWriter,
		acl:          s.acl,
	}
}

// ResolveNamespaceRoot 返回 actor 可访问的命名空间根目录（见
// resolveNamespaceRoot）：personal 要求 scopeID==actor（个人根目录，
// 缺省自动创建，与 EnsureRoot 一致且仅匹配 team_id IS NULL 的个人根）；
// team 要求 actor 为成员（teamReader/CanRead），根目录须存在且未删除。
func (s *Store) ResolveNamespaceRoot(actor uuid.UUID, nsType string, scopeID uuid.UUID) (File, error) {
	return resolveNamespaceRoot(s.pathEnvOf(), actor, nsType, scopeID)
}

// ResolveReadablePath 解析命名空间内相对路径并做读授权（authorizeFileAccess
// 复用：个人仅 owner、团队任意在册成员/ACL）。path 为 "/" 分隔的相对路径，
// 空路径即根目录。返回命中文件与路径链（chain[0]=根目录，canonical path 取
// 链上各段实际存储名称）。
func (s *Store) ResolveReadablePath(actor uuid.UUID, nsType string, scopeID uuid.UUID, path string) (File, []File, error) {
	return resolveNamespacePath(s.pathEnvOf(), actor, nsType, scopeID, path, false)
}

// ResolveWritablePath 同 ResolveReadablePath，但按写权限判定
// （authorizeFileWrite：个人仅 owner、团队 owner/editor/ACL write，viewer 403）。
func (s *Store) ResolveWritablePath(actor uuid.UUID, nsType string, scopeID uuid.UUID, path string) (File, []File, error) {
	return resolveNamespacePath(s.pathEnvOf(), actor, nsType, scopeID, path, true)
}

// ResolveSubpath 在 base 目录下解析相对路径（供目录分享树与子资源 raw 使用；
// 不做用户鉴权，调用方负责分享有效性等校验）。段校验与环检测同
// SplitPathSegments/resolvePathLogic，越出子树（不存在/断链）统一 ErrNotFound。
func (s *Store) ResolveSubpath(base File, path string) (File, []File, error) {
	segments, err := SplitPathSegments(path)
	if err != nil {
		return File{}, nil, err
	}
	return resolvePathLogic(s.findChildByLowerName, base, segments)
}

// ListFolderChildren 列举目录直接子项（目录在前、lower(name) 排序），
// 供目录分享树清单；不做鉴权（调用方负责）。limit 缺省/越界取 100/1000。
func (s *Store) ListFolderChildren(folderID uuid.UUID, limit int) ([]File, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var out []File
	err := s.db.Where("parent_id = ? AND is_root = false AND deleted_at IS NULL", folderID).
		Order("type DESC, lower(name) ASC, id ASC").Limit(limit).Find(&out).Error
	return out, err
}
