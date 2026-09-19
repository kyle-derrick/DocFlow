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
	// SetResult 写入解包结果（状态、原因、条目数、总量与本次解包的源 blob
	// sha256；runErr/sourceSHA 空串落 NULL）并刷新 updated_at。
	SetResult(id uuid.UUID, status, runErr string, entries int, totalSize int64, sourceSHA string) error
	// TryMarkExtracting 并发互斥的条件更新：仅当行存在且 status<>'extracting'
	// 时置 extracting（并清空原因/计数），返回是否生效；false 表示他人解包
	// 进行中（0 行受影响），调用方应直接返回。
	TryMarkExtracting(fileID uuid.UUID) (bool, error)
	// ReadyFileIDs 批量返回 ids 中已成功解包（status=ready——Extract 校验
	// 保证 zip 含 index.html）的文件 ID 集合；列表侧据此给 zip 行打「网页」
	// 标记。ids 为空时返回空集合。
	ReadyFileIDs(ids []uuid.UUID) (map[uuid.UUID]bool, error)
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

// nullableStr 空串规整为 NULL 指针。
func nullableStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func (g *GormRepo) SetResult(id uuid.UUID, status, runErr string, entries int, totalSize int64, sourceSHA string) error {
	return g.db.Model(&Package{}).Where("id = ?", id).Updates(map[string]any{
		"status":             status,
		"error":              nullableStr(runErr),
		"entry_count":        entries,
		"total_size":         totalSize,
		"source_blob_sha256": nullableStr(sourceSHA),
		"updated_at":         time.Now(),
	}).Error
}

// TryMarkExtracting 条件更新（单语句原子判定）：status<>'extracting' 才置
// extracting，RowsAffected=0 表示他人解包进行中。
func (g *GormRepo) TryMarkExtracting(fileID uuid.UUID) (bool, error) {
	result := g.db.Model(&Package{}).
		Where("file_id = ? AND status <> ?", fileID, StatusExtracting).
		Updates(map[string]any{
			"status":      StatusExtracting,
			"error":       nil,
			"entry_count": 0,
			"total_size":  0,
			"updated_at":  time.Now(),
		})
	return result.RowsAffected > 0, result.Error
}

// ReadyFileIDs 的 GORM 实现：单条 IN 查询 status=ready 的 file_id。
func (g *GormRepo) ReadyFileIDs(ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []Package
	if err := g.db.Select("file_id").Where("file_id IN ? AND status = ?", ids, StatusReady).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.FileID] = true
	}
	return out, nil
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

func (m *MemoryRepo) SetResult(id uuid.UUID, status, runErr string, entries int, totalSize int64, sourceSHA string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.items[id]
	if !ok {
		return ErrNotFound
	}
	p.Status, p.Error, p.EntryCount, p.TotalSize = status, nullableStr(runErr), entries, totalSize
	p.SourceBlobSHA256 = nullableStr(sourceSHA)
	p.UpdatedAt = time.Now()
	m.items[id] = p
	return nil
}

// TryMarkExtracting 的内存实现：锁内检查 status<>'extracting' 后置位，
// 并发下恰好一个调用方生效。
func (m *MemoryRepo) TryMarkExtracting(fileID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byFile[fileID]
	if !ok {
		return false, nil
	}
	p := m.items[id]
	if p.Status == StatusExtracting {
		return false, nil
	}
	p.Status, p.Error, p.EntryCount, p.TotalSize = StatusExtracting, nil, 0, 0
	p.UpdatedAt = time.Now()
	m.items[id] = p
	return true, nil
}

// ReadyFileIDs 的内存实现。
func (m *MemoryRepo) ReadyFileIDs(ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if pkgID, ok := m.byFile[id]; ok && m.items[pkgID].Status == StatusReady {
			out[id] = true
		}
	}
	return out, nil
}
