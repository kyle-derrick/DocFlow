package search

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var _ Repo = (*MemoryRepo)(nil)

// MemoryFile 模拟 files 行的检索相关实时状态（名称/类型/软删/收藏等）。
// 导出供跨包测试（如 internal/http 的端点测试）构造数据。
type MemoryFile struct {
	Name      string
	Type      string
	ParentID  *uuid.UUID
	UpdatedAt time.Time
	Deleted   bool
	Starred   bool
}

// MemoryRepo 是 Repo 的内存实现：语义与 GormRepo 对齐（访问控制、软删排除、
// tag/starred 过滤、名称命中优先排序、limit、snippet 生成），供单测验证
// Store 语义与 HTTP 契约测试注入使用（不依赖 PostgreSQL）。内容匹配以
// 大小写不敏感子串包含模拟 tsv 命中（内存场景等价英文词命中）。
type MemoryRepo struct {
	mu      sync.RWMutex
	docs    map[uuid.UUID]Doc
	files   map[uuid.UUID]MemoryFile
	tags    map[uuid.UUID]map[uuid.UUID]bool // file_id -> tag_id
	members map[uuid.UUID]map[uuid.UUID]bool // space_id -> user_id
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{
		docs:    make(map[uuid.UUID]Doc),
		files:   make(map[uuid.UUID]MemoryFile),
		tags:    make(map[uuid.UUID]map[uuid.UUID]bool),
		members: make(map[uuid.UUID]map[uuid.UUID]bool),
	}
}

// PutFile 写入/覆盖一条文件实时状态（测试构造数据）。
func (m *MemoryRepo) PutFile(fileID uuid.UUID, f MemoryFile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[fileID] = f
}

// PutDoc 直接写入一条索引文档（测试构造数据）。
func (m *MemoryRepo) PutDoc(d Doc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs[d.FileID] = d
}

// AddTag 给文件打标签；AddMember 登记空间成员（访问判定数据源）。
func (m *MemoryRepo) AddTag(fileID, tagID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tags[fileID] == nil {
		m.tags[fileID] = make(map[uuid.UUID]bool)
	}
	m.tags[fileID][tagID] = true
}

func (m *MemoryRepo) AddMember(spaceID, userID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.members[spaceID] == nil {
		m.members[spaceID] = make(map[uuid.UUID]bool)
	}
	m.members[spaceID][userID] = true
}

func (m *MemoryRepo) UpsertDoc(d Doc) error {
	m.PutDoc(d)
	return nil
}

func (m *MemoryRepo) RemoveDoc(fileID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, fileID)
	return nil
}

// QueryDocs 与 GormRepo.QueryDocs 语义一致（见其注释）；差异仅在于内容
// 匹配用子串包含而非 tsvector。
func (m *MemoryRepo) QueryDocs(user uuid.UUID, opts QueryOptions) ([]Result, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Result, 0)
	for _, d := range m.docs {
		f, ok := m.files[d.FileID]
		if !ok || f.Deleted {
			continue // 文件不存在（孤儿由清理路径负责）或软删：排除
		}
		// 访问控制（统一空间模型）：owner 命中或空间在册成员。
		readable := d.OwnerID == user
		if !readable && d.SpaceID != nil && m.members[*d.SpaceID][user] {
			readable = true
		}
		if !readable {
			continue
		}
		// 名称实时值命中（大小写不敏感子串）或内容命中。
		nameMatch := containsFold(f.Name, opts.Q)
		contentMatch := !nameMatch && d.Content != "" && containsFold(d.Content, opts.Q)
		if !nameMatch && !contentMatch {
			continue
		}
		if opts.TagID != nil && !m.tags[d.FileID][*opts.TagID] {
			continue
		}
		if opts.Starred != nil && f.Starred != *opts.Starred {
			continue
		}
		snippet := ""
		if nameMatch {
			snippet = f.Name
		} else if contentMatch {
			// 模拟 ts_headline：截取首个命中附近的片段，标记 [[..]]。
			idx := strings.Index(strings.ToLower(d.Content), strings.ToLower(opts.Q))
			start := idx - 30
			if start < 0 {
				start = 0
			}
			end := idx + len(opts.Q) + 30
			if end > len(d.Content) {
				end = len(d.Content)
			}
			snippet = d.Content[start:idx] + "[[" + d.Content[idx:idx+len(opts.Q)] + "]]" + d.Content[idx+len(opts.Q):end]
		}
		parent := f.ParentID
		out = append(out, Result{ID: d.FileID, Name: f.Name, Type: f.Type, ParentID: parent, UpdatedAt: f.UpdatedAt, Snippet: snippet})
	}
	// 名称命中优先，其次 updated_at 倒序，id 稳定次序（与 SQL ORDER BY 对齐）。
	sort.Slice(out, func(i, j int) bool {
		ni, nj := containsFold(out[i].Name, opts.Q), containsFold(out[j].Name, opts.Q)
		if ni != nj {
			return ni
		}
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}
