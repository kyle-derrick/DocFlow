package acl

import (
	"sort"
	"sync"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

var _ Repo = (*MemoryRepo)(nil)

// MemoryRepo 是 Repo 的内存实现，供单测与 HTTP 契约测试注入使用。
type MemoryRepo struct {
	mu      sync.RWMutex
	folders map[uuid.UUID]files.File
	entries map[uuid.UUID][]Entry // folder_id -> entries
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{
		folders: make(map[uuid.UUID]files.File),
		entries: make(map[uuid.UUID][]Entry),
	}
}

// PutFolder 写入/覆盖一条目录行（测试构造数据）。
func (m *MemoryRepo) PutFolder(f files.File) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.folders[f.ID] = f
}

func (m *MemoryRepo) Folder(id uuid.UUID) (files.File, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.folders[id]
	if !ok || f.Type != "folder" || f.DeletedAt != nil {
		return files.File{}, ErrNotFound
	}
	return f, nil
}

func (m *MemoryRepo) ListByFolder(folderID uuid.UUID) ([]Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Entry, 0, len(m.entries[folderID]))
	out = append(out, m.entries[folderID]...)
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

func (m *MemoryRepo) Replace(folderID uuid.UUID, entries []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.folders[folderID]; !ok {
		return ErrNotFound
	}
	m.entries[folderID] = append([]Entry(nil), entries...)
	return nil
}

// ChainForFile 与 GormRepo 同语义：文件自父目录起、目录含自身，向上到根；
// 断链截断（best-effort）。
func (m *MemoryRepo) ChainForFile(fileOrFolderID uuid.UUID) ([]ChainNode, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	start, ok := m.folders[fileOrFolderID]
	if !ok {
		// 目标可能是文件：文件行不在 folders 表中，仅当有父目录时继续。
		start = files.File{Type: "file"}
		start.ID = fileOrFolderID
	}
	cur := fileOrFolderID
	if start.Type != "folder" {
		if start.ParentID == nil {
			return nil, nil
		}
		cur = *start.ParentID
	}
	var chain []ChainNode
	for depth := 0; depth < maxChainDepth; depth++ {
		f, ok := m.folders[cur]
		if !ok || f.Type != "folder" || f.DeletedAt != nil {
			break
		}
		chain = append(chain, ChainNode{FolderID: f.ID, Entries: append([]Entry(nil), m.entries[f.ID]...)})
		if f.IsRoot || f.ParentID == nil {
			break
		}
		cur = *f.ParentID
	}
	return chain, nil
}
