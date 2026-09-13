package onlyoffice

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CallbackStore 抽象回调幂等键 (file_id, document.key, url) 的持久化：
// 验签通过后、处理前先 TryRecord 抢占，冲突（inserted=false）即视为已处理；
// 处理失败时 Release 回滚记录，允许 DocumentServer 重试。
type CallbackStore interface {
	// TryRecord 尝试记录幂等键：首次插入返回 inserted=true；
	// 已存在（重复回调）返回 inserted=false，error 非 nil 表示存储故障。
	TryRecord(fileID uuid.UUID, key, url, status, result string) (inserted bool, err error)
	// Release 删除幂等记录（保存处理失败时回滚，重试可重新抢占）。
	Release(fileID uuid.UUID, key, url string) error
}

// CallbackRecord 映射 onlyoffice_callbacks 表（migration 012）：
// UNIQUE (file_id, document_key, callback_url) 即设计幂等键。
type CallbackRecord struct {
	ID          int64     `gorm:"primaryKey"`
	FileID      uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:uq_onlyoffice_callbacks_idem,priority:1"`
	DocumentKey string    `gorm:"column:document_key;size:255;not null;uniqueIndex:uq_onlyoffice_callbacks_idem,priority:2"`
	CallbackURL string    `gorm:"column:callback_url;type:text;not null;uniqueIndex:uq_onlyoffice_callbacks_idem,priority:3"`
	Status      string    `gorm:"size:16;not null"`
	Result      string    `gorm:"size:8;not null"`
	CreatedAt   time.Time
}

// TableName 固定表名（Gorm 默认复数化为 callback_records）。
func (CallbackRecord) TableName() string { return "onlyoffice_callbacks" }

// GormCallbackStore 是 CallbackStore 的 PostgreSQL 实现：
// INSERT ... ON CONFLICT DO NOTHING + RowsAffected 判定是否首次插入。
type GormCallbackStore struct{ db *gorm.DB }

// NewGormCallbackStore 构造 Gorm 版回调幂等存储（main 接线用）。
func NewGormCallbackStore(db *gorm.DB) *GormCallbackStore { return &GormCallbackStore{db: db} }

// TryRecord 以幂等唯一约束抢占：冲突时 INSERT 不生效（RowsAffected=0）。
func (s *GormCallbackStore) TryRecord(fileID uuid.UUID, key, url, status, result string) (bool, error) {
	rec := CallbackRecord{FileID: fileID, DocumentKey: key, CallbackURL: url, Status: status, Result: result}
	res := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rec)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// Release 按幂等键删除记录（处理失败回滚）。
func (s *GormCallbackStore) Release(fileID uuid.UUID, key, url string) error {
	return s.db.Where("file_id = ? AND document_key = ? AND callback_url = ?", fileID, key, url).
		Delete(&CallbackRecord{}).Error
}

// memoryCallbackStore 是 CallbackStore 的进程内实现（Service 默认；单测使用）。
type memoryCallbackStore struct {
	mu   sync.Mutex
	rows map[string]struct{}
}

func newMemoryCallbackStore() *memoryCallbackStore {
	return &memoryCallbackStore{rows: make(map[string]struct{})}
}

// callbackRecordKey 拼接幂等键（与表 UNIQUE (file_id, document_key, callback_url) 对应）。
func callbackRecordKey(fileID uuid.UUID, key, url string) string {
	return fileID.String() + "\x00" + key + "\x00" + url
}

func (m *memoryCallbackStore) TryRecord(fileID uuid.UUID, key, url, status, result string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idem := callbackRecordKey(fileID, key, url)
	if _, ok := m.rows[idem]; ok {
		return false, nil
	}
	m.rows[idem] = struct{}{}
	return true, nil
}

func (m *memoryCallbackStore) Release(fileID uuid.UUID, key, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, callbackRecordKey(fileID, key, url))
	return nil
}
