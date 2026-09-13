package webpkg

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Repo 抽象 web_packages 持久化；GormRepo 为 PostgreSQL 实现，
// MemoryRepo 供测试使用（模式同 share 包）。
type Repo interface {
	// GetByFileID 按 file_id 取行；不存在返回 ErrNotFound。
	GetByFileID(fileID uuid.UUID) (Package, error)
	// GetByPublicID 按 public_id 取行；不存在返回 ErrNotFound。
	GetByPublicID(publicID string) (Package, error)
	// Create 新建行（file_id 唯一冲突由实现方报错）。
	Create(p Package) error
	// SetResult 写入解包结果（状态、原因、条目数、总量）并刷新 updated_at。
	SetResult(id uuid.UUID, status, runErr string, entries int, totalSize int64) error
}

// GormRepo 是 Repo 的 GORM/PostgreSQL 实现。
type GormRepo struct{ db *gorm.DB }

func NewGormRepo(db *gorm.DB) *GormRepo { return &GormRepo{db: db} }

func (g *GormRepo) GetByFileID(fileID uuid.UUID) (Package, error) {
	var p Package
	if err := g.db.Where("file_id = ?", fileID).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Package{}, ErrNotFound
		}
		return Package{}, err
	}
	return p, nil
}

func (g *GormRepo) GetByPublicID(publicID string) (Package, error) {
	var p Package
	if err := g.db.Where("public_id = ?", publicID).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Package{}, ErrNotFound
		}
		return Package{}, err
	}
	return p, nil
}

func (g *GormRepo) Create(p Package) error { return g.db.Create(&p).Error }

func (g *GormRepo) SetResult(id uuid.UUID, status, runErr string, entries int, totalSize int64) error {
	return g.db.Model(&Package{}).Where("id = ?", id).Updates(map[string]any{
		"status":      status,
		"error":       runErr,
		"entry_count": entries,
		"total_size":  totalSize,
		"updated_at":  time.Now(),
	}).Error
}

// MemoryRepo 是 Repo 的内存实现（测试用）。
type MemoryRepo struct {
	mu      sync.RWMutex
	items   map[uuid.UUID]Package
	byFile  map[uuid.UUID]uuid.UUID
	byPID   map[string]uuid.UUID
	counter int
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{items: make(map[uuid.UUID]Package), byFile: make(map[uuid.UUID]uuid.UUID), byPID: make(map[string]uuid.UUID)}
}

func (m *MemoryRepo) GetByFileID(fileID uuid.UUID) (Package, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byFile[fileID]
	if !ok {
		return Package{}, ErrNotFound
	}
	return m.items[id], nil
}

func (m *MemoryRepo) GetByPublicID(publicID string) (Package, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byPID[publicID]
	if !ok {
		return Package{}, ErrNotFound
	}
	return m.items[id], nil
}

func (m *MemoryRepo) Create(p Package) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byFile[p.FileID]; exists {
		return errors.New("duplicate web package file_id")
	}
	if _, exists := m.byPID[p.PublicID]; exists {
		return errors.New("duplicate web package public_id")
	}
	if p.CreatedAt.IsZero() {
		m.counter++
		p.CreatedAt = time.Now().Add(time.Duration(m.counter))
	}
	p.UpdatedAt = p.CreatedAt
	m.items[p.ID] = p
	m.byFile[p.FileID] = p.ID
	m.byPID[p.PublicID] = p.ID
	return nil
}

func (m *MemoryRepo) SetResult(id uuid.UUID, status, runErr string, entries int, totalSize int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.items[id]
	if !ok {
		return ErrNotFound
	}
	p.Status, p.Error, p.EntryCount, p.TotalSize = status, runErr, entries, totalSize
	p.UpdatedAt = time.Now()
	m.items[id] = p
	return nil
}
