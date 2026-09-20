package share

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ Repo = (*GormStore)(nil)

// GormStore 是 Repo 的 PostgreSQL 实现（shares 表见 migrations/006_shares.sql）。
type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

// Create 写入分享记录。私有分享 TokenHash 为空串时省略该列，
// 使 token_hash 以 NULL 落库——PostgreSQL UNIQUE 约束允许多个 NULL，
// 而多个空串 ” 会冲突（008 已将列改为可空）。
func (s *GormStore) Create(v Share) error {
	if v.TokenHash == "" {
		return s.db.Omit("token_hash").Create(&v).Error
	}
	return s.db.Create(&v).Error
}

func (s *GormStore) Get(id uuid.UUID) (Share, error) {
	var v Share
	err := s.db.First(&v, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Share{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) GetByOwner(owner, id uuid.UUID) (Share, error) {
	var v Share
	err := s.db.Where("id = ? AND owner_id = ?", id, owner).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Share{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) GetByTokenHash(hash string) (Share, error) {
	var v Share
	err := s.db.Where("token_hash = ?", hash).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Share{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) ListByOwner(owner uuid.UUID, limit int) ([]Share, error) {
	var out []Share
	err := s.db.Where("owner_id = ?", owner).Order("created_at DESC, id").Limit(limit).Find(&out).Error
	return out, err
}

// ownerFilteredScope 组装「我的分享」过滤条件（OwnerListFilter → SQL），
// 返回叠加了过滤的查询（未加排序/分页）。文件名过滤经 LEFT JOIN files
// （软删文件名仍可检索，展示名由服务层置空，口径与列表页一致）。
func (s *GormStore) ownerFilteredScope(owner uuid.UUID, f OwnerListFilter) *gorm.DB {
	q := s.db.Model(&Share{}).Where("shares.owner_id = ?", owner)
	if f.Q != "" {
		needle := "%" + strings.ToLower(f.Q) + "%"
		q = q.Joins("LEFT JOIN files ON files.id = shares.file_id").
			Where("LOWER(files.name) LIKE ?", needle)
	}
	if f.Visibility == VisibilityPublic || f.Visibility == VisibilityPrivate {
		q = q.Where("shares.visibility = ?", f.Visibility)
	}
	switch f.Status {
	case "active":
		q = q.Where("shares.revoked_at IS NULL AND (shares.expires_at IS NULL OR shares.expires_at > ?)", f.Now)
	case "revoked":
		q = q.Where("shares.revoked_at IS NOT NULL")
	case "expired":
		q = q.Where("shares.revoked_at IS NULL AND shares.expires_at IS NOT NULL AND shares.expires_at <= ?", f.Now)
	}
	return q
}

// ListByOwnerFiltered 分页返回 owner 的分享（created_at 倒序）+ 过滤后总数。
func (s *GormStore) ListByOwnerFiltered(owner uuid.UUID, f OwnerListFilter, limit, offset int) ([]Share, int64, error) {
	var total int64
	if err := s.ownerFilteredScope(owner, f).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []Share
	err := s.ownerFilteredScope(owner, f).
		Order("shares.created_at DESC, shares.id").
		Limit(limit).Offset(offset).Find(&out).Error
	return out, total, err
}

// ListSharedWithUser 单条 SQL 实时判定授权（与 CanAccess 判定一致）：
// 有效私有分享（未撤销、未过期、未达下载上限，不含自己创建的），
// 且 share_users 显式授权 user，或 user 属于 share_spaces 任一授权空间
// （EXISTS 子查询判定空间成员：直接成员或经用户组，成员变动立即生效）。
func (s *GormStore) ListSharedWithUser(user uuid.UUID, now time.Time, limit int) ([]Share, error) {
	var out []Share
	err := s.db.
		Where("visibility = ? AND owner_id <> ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) AND (max_downloads IS NULL OR download_count < max_downloads)",
			VisibilityPrivate, user, now).
		Where("(EXISTS (SELECT 1 FROM share_users su WHERE su.share_id = shares.id AND su.user_id = ?) OR "+
			"EXISTS (SELECT 1 FROM share_spaces ss WHERE ss.share_id = shares.id AND ("+
			"EXISTS (SELECT 1 FROM space_members sm WHERE sm.space_id = ss.space_id AND sm.user_id = ?) OR "+
			"EXISTS (SELECT 1 FROM space_group_members sgm JOIN group_members gm ON gm.group_id = sgm.group_id WHERE sgm.space_id = ss.space_id AND gm.user_id = ?))))",
			user, user, user).
		Order("created_at DESC, id").Limit(limit).Find(&out).Error
	return out, err
}

func (s *GormStore) Revoke(id uuid.UUID, now time.Time) error {
	return s.db.Model(&Share{}).Where("id = ? AND revoked_at IS NULL", id).Update("revoked_at", now).Error
}

// Delete 物理删除分享行（手动「清除记录」，服务层已校验撤销满 30 天）；
// share_users/share_spaces/share_files 等关联经 FK ON DELETE CASCADE 清理。
func (s *GormStore) Delete(id uuid.UUID) error {
	return s.db.Delete(&Share{}, "id = ?", id).Error
}

// ConsumeDownload 用条件 UPDATE 原子递增 download_count，
// 条件与 shareActive 一致：未撤销、未过期、未达下载上限。
func (s *GormStore) ConsumeDownload(id uuid.UUID, now time.Time) (bool, error) {
	result := s.db.Model(&Share{}).
		Where("id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) AND (max_downloads IS NULL OR download_count < max_downloads)", id, now).
		UpdateColumn("download_count", gorm.Expr("download_count + 1"))
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// DecrementDownload 补偿回退一次下载计数（ConsumeDownload 已消耗但内容
// 读取失败）：事务内先锁读分享行取 file_id，再同步递减 shares 与 files 的
// download_count（条件 > 0 保证不为负）；分享不存在或计数为 0 时静默成功。
func (s *GormStore) DecrementDownload(id uuid.UUID) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var sh Share
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND download_count > 0", id).First(&sh).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if err := tx.Model(&Share{}).Where("id = ? AND download_count > 0", id).
			UpdateColumn("download_count", gorm.Expr("download_count - 1")).Error; err != nil {
			return err
		}
		return tx.Table("files").Where("id = ? AND download_count > 0", sh.FileID).
			UpdateColumn("download_count", gorm.Expr("download_count - 1")).Error
	})
}

func (s *GormStore) CountActiveByFile(fileID uuid.UUID, now time.Time) (int64, error) {
	var count int64
	// is_public 只反映「公开」分享；私有分享不参与该辅助字段的维护。
	err := s.db.Model(&Share{}).
		Where("file_id = ? AND visibility = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) AND (max_downloads IS NULL OR download_count < max_downloads)", fileID, VisibilityPublic, now).
		Count(&count).Error
	return count, err
}

func (s *GormStore) SetFilePublic(fileID uuid.UUID, value bool) error {
	return s.db.Table("files").Where("id = ?", fileID).Update("is_public", value).Error
}

// ShareUser / ShareSpace 对应 share_users / share_spaces 关联表（migrations/040_unified_spaces.sql）。
type ShareUser struct {
	ShareID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	CreatedAt time.Time
}

type ShareSpace struct {
	ShareID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	SpaceID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	CreatedAt time.Time
}

// TableName 显式映射 share_spaces（gorm 默认复数化为 share_spaces，此处
// 显式声明保持清晰）。
func (ShareSpace) TableName() string { return "share_spaces" }

// ShareFile 对应 share_files 连接表（migration 036）：多文件打包分享的
// 可见条目（分享锚点目录其余子项不暴露）。
type ShareFile struct {
	ShareID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	FileID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	CreatedAt time.Time
}

func (s *GormStore) AddShareUsers(shareID uuid.UUID, userIDs []uuid.UUID, now time.Time) error {
	if len(userIDs) == 0 {
		return nil
	}
	rows := make([]ShareUser, 0, len(userIDs))
	for _, id := range userIDs {
		rows = append(rows, ShareUser{ShareID: shareID, UserID: id, CreatedAt: now})
	}
	return s.db.Create(&rows).Error
}

func (s *GormStore) AddShareSpaces(shareID uuid.UUID, spaceIDs []uuid.UUID, now time.Time) error {
	if len(spaceIDs) == 0 {
		return nil
	}
	rows := make([]ShareSpace, 0, len(spaceIDs))
	for _, id := range spaceIDs {
		rows = append(rows, ShareSpace{ShareID: shareID, SpaceID: id, CreatedAt: now})
	}
	return s.db.Create(&rows).Error
}

func (s *GormStore) ListShareUserIDs(shareID uuid.UUID) ([]uuid.UUID, error) {
	var rows []ShareUser
	err := s.db.Where("share_id = ?", shareID).Order("user_id").Find(&rows).Error
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.UserID)
	}
	return out, err
}

func (s *GormStore) ListShareSpaceIDs(shareID uuid.UUID) ([]uuid.UUID, error) {
	var rows []ShareSpace
	err := s.db.Where("share_id = ?", shareID).Order("space_id").Find(&rows).Error
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SpaceID)
	}
	return out, err
}

// AddShareFiles 写入打包分享的可见条目（share_files，migration 036）。
func (s *GormStore) AddShareFiles(shareID uuid.UUID, fileIDs []uuid.UUID, now time.Time) error {
	if len(fileIDs) == 0 {
		return nil
	}
	rows := make([]ShareFile, 0, len(fileIDs))
	for _, id := range fileIDs {
		rows = append(rows, ShareFile{ShareID: shareID, FileID: id, CreatedAt: now})
	}
	return s.db.Create(&rows).Error
}

// ListShareFileIDs 返回打包分享的可见条目 ID（file_id 排序，结果稳定）。
func (s *GormStore) ListShareFileIDs(shareID uuid.UUID) ([]uuid.UUID, error) {
	var rows []ShareFile
	err := s.db.Where("share_id = ?", shareID).Order("file_id").Find(&rows).Error
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.FileID)
	}
	return out, err
}

// CreateSession 写入公开访问会话（share_access_sessions，migration 022）。
func (s *GormStore) CreateSession(v AccessSession) error {
	return s.db.Create(&v).Error
}

// GetSessionByHash 按 session_hash 查会话；无命中返回 ErrNotFound。
func (s *GormStore) GetSessionByHash(hash string) (AccessSession, error) {
	var v AccessSession
	err := s.db.Where("session_hash = ?", hash).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AccessSession{}, ErrNotFound
	}
	return v, err
}

// UpdateFields 按列名更新分享行（服务层已解析为最终值，nil → NULL）；
// 行不存在返回 ErrNotFound。
func (s *GormStore) UpdateFields(id uuid.UUID, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	result := s.db.Model(&Share{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordAccessEvent 写入公开访问事件（file_access_events，migration 023）。
func (s *GormStore) RecordAccessEvent(e AccessEvent) error {
	return s.db.Create(&e).Error
}

// recentAccessEventsLimit 为 ShareAccessStats 最近事件条数上限（设计 6.6.3）。
const recentAccessEventsLimit = 20

// ShareAccessStats 聚合分享访问统计：事件总数、独立访客（distinct ip_hash）
// 与最近 20 条事件（created_at 倒序）。
func (s *GormStore) ShareAccessStats(shareID uuid.UUID) (AccessStats, error) {
	var stats AccessStats
	if err := s.db.Model(&AccessEvent{}).Where("share_id = ?", shareID).Count(&stats.TotalAccess).Error; err != nil {
		return AccessStats{}, err
	}
	if err := s.db.Model(&AccessEvent{}).Select("COUNT(DISTINCT ip_hash)").
		Where("share_id = ?", shareID).Scan(&stats.UniqueVisitors).Error; err != nil {
		return AccessStats{}, err
	}
	if err := s.db.Where("share_id = ?", shareID).
		Order("created_at DESC, id DESC").Limit(recentAccessEventsLimit).Find(&stats.Recent).Error; err != nil {
		return AccessStats{}, err
	}
	return stats, nil
}
