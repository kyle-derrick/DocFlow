package tagging

import (
	"sort"
	"sync"

	"github.com/google/uuid"
)

// MemoryRepo 是 Repo 的内存实现，供单元测试与契约测试使用。
type MemoryRepo struct {
	mu    sync.Mutex
	tags  map[uuid.UUID]Tag
	links map[FileTag]bool
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{tags: map[uuid.UUID]Tag{}, links: map[FileTag]bool{}}
}

func (m *MemoryRepo) CreateTag(t Tag) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.tags {
		if existing.UserID == t.UserID && existing.Name == t.Name {
			return ErrConflict
		}
	}
	m.tags[t.ID] = t
	return nil
}

func (m *MemoryRepo) GetTag(id uuid.UUID) (Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tags[id]
	if !ok {
		return Tag{}, ErrNotFound
	}
	return t, nil
}

func (m *MemoryRepo) ListTags(user uuid.UUID) ([]Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Tag
	for _, t := range m.tags {
		if t.UserID == user {
			out = append(out, t)
		}
	}
	sortTags(out)
	return out, nil
}

func (m *MemoryRepo) DeleteTag(id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tags, id)
	for ft := range m.links {
		if ft.TagID == id {
			delete(m.links, ft)
		}
	}
	return nil
}

func (m *MemoryRepo) FileTagExists(tagID, fileID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.links[FileTag{TagID: tagID, FileID: fileID}], nil
}

func (m *MemoryRepo) AddFileTag(ft FileTag) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := FileTag{TagID: ft.TagID, FileID: ft.FileID}
	if m.links[key] {
		return ErrConflict
	}
	m.links[key] = true
	return nil
}

func (m *MemoryRepo) RemoveFileTag(tagID, fileID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := FileTag{TagID: tagID, FileID: fileID}
	existed := m.links[key]
	delete(m.links, key)
	return existed, nil
}

func (m *MemoryRepo) ListFileTags(user, fileID uuid.UUID) ([]Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Tag
	for ft := range m.links {
		if ft.FileID != fileID {
			continue
		}
		if t, ok := m.tags[ft.TagID]; ok && t.UserID == user {
			out = append(out, t)
		}
	}
	sortTags(out)
	return out, nil
}

func sortTags(tags []Tag) {
	sort.Slice(tags, func(i, j int) bool { return tags[i].Name < tags[j].Name })
}
