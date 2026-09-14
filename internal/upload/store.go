package upload

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) *GormStore       { return &GormStore{db: db} }
func (s *GormStore) Save(v UploadSession) error { return s.db.Create(&v).Error }
func (s *GormStore) Get(id uuid.UUID) (UploadSession, error) {
	var v UploadSession
	err := s.db.First(&v, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return UploadSession{}, ErrNotFound
	}
	return v, err
}
func (s *GormStore) Update(v UploadSession) error {
	result := s.db.Model(&UploadSession{}).Where("id = ?", v.ID).Updates(&v)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// AdvanceOffset 条件更新 offset（Append CAS）：仅当行存在、offset 仍等于
// from 且状态为 uploading 时 offset += delta（delta 可为负，用于写失败回退
// 预占），否则返回 ErrOffset（offset/状态已被并发 Append、Complete 或
// janitor 改写）。表无 updated_at 列，仅更新 offset。
func (s *GormStore) AdvanceOffset(id uuid.UUID, from, delta int64, _ time.Time) error {
	result := s.db.Model(&UploadSession{}).
		Where("id = ? AND offset = ? AND status = ?", id, from, StatusUploading).
		Update("offset", gorm.Expr("offset + ?", delta))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrOffset
	}
	return nil
}

// MarkVerifying 状态迁移 uploading → verifying 的条件更新（Complete CAS）：
// 仅当状态为 uploading 且 offset 已达 size 时生效，返回是否迁移成功。
// 失败说明会话被并发改写（另一 Complete 已推进 / 仍在写入 / janitor 置 failed）。
func (s *GormStore) MarkVerifying(id uuid.UUID, size int64) (bool, error) {
	result := s.db.Model(&UploadSession{}).
		Where("id = ? AND status = ? AND offset = ?", id, StatusUploading, size).
		Update("status", StatusVerifying)
	return result.RowsAffected > 0, result.Error
}

// MarkScanning 状态迁移 verifying → scanning 的条件更新。
func (s *GormStore) MarkScanning(id uuid.UUID) (bool, error) {
	result := s.db.Model(&UploadSession{}).
		Where("id = ? AND status = ?", id, StatusVerifying).
		Update("status", StatusScanning)
	return result.RowsAffected > 0, result.Error
}

// MarkAvailable 终态写入 scanning → available 的条件更新（completed_at 与
// 终态 storage_key 一并落库）。失败说明会话已被并发流转（如 janitor 在
// 扫描阶段置 failed）。
func (s *GormStore) MarkAvailable(id uuid.UUID, storageKey string, completedAt time.Time) (bool, error) {
	result := s.db.Model(&UploadSession{}).
		Where("id = ? AND status = ?", id, StatusScanning).
		Updates(map[string]any{"status": StatusAvailable, "storage_key": storageKey, "completed_at": completedAt})
	return result.RowsAffected > 0, result.Error
}

// CountActiveByUser 统计用户当前活跃（uploading/verifying/scanning 且未
// 过期）的上传会话数（DB COUNT）：作为 upload.max_concurrent_uploads_per_user
// 门控的计数来源。DB 为多实例共享的单一事实来源，水平扩展下语义一致；
// 计数在 Save 之前统计，瞬时并窗口下为软上限（并发建会话可短暂超出，
// 不引入行锁以避免建会话热路径串行化）。
func (s *GormStore) CountActiveByUser(user uuid.UUID, now time.Time) (int64, error) {
	var n int64
	err := s.db.Model(&UploadSession{}).
		Where("user_id = ? AND status IN ? AND expires_at > ?", user, []Status{StatusUploading, StatusVerifying, StatusScanning}, now).
		Count(&n).Error
	return n, err
}
