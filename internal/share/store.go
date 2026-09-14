package share

import (
	"errors"
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

// ListSharedWithUser 单条 SQL 实时判定授权（与 CanAccess 判定一致）：
// 有效私有分享（未撤销、未过期、未达下载上限，不含自己创建的），
// 且 share_users 显式授权 user，或 user 属于 share_teams 任一授权团队
// （EXISTS 子查询 JOIN team_members，成员变动立即生效）。
func (s *GormStore) ListSharedWithUser(user uuid.UUID, now time.Time, limit int) ([]Share, error) {
	var out []Share
	err := s.db.
		Where("visibility = ? AND owner_id <> ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) AND (max_downloads IS NULL OR download_count < max_downloads)",
			VisibilityPrivate, user, now).
		Where("(EXISTS (SELECT 1 FROM share_users su WHERE su.share_id = shares.id AND su.user_id = ?) OR "+
			"EXISTS (SELECT 1 FROM share_teams st JOIN team_members tm ON tm.team_id = st.team_id WHERE st.share_id = shares.id AND tm.user_id = ?))",
			user, user).
		Order("created_at DESC, id").Limit(limit).Find(&out).Error
	return out, err
}

func (s *GormStore) Revoke(id uuid.UUID, now time.Time) error {
	return s.db.Model(&Share{}).Where("id = ? AND revoked_at IS NULL", id).Update("revoked_at", now).Error
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

// ShareUser / ShareTeam 对应 share_users / share_teams 关联表（migrations/008_teams_shares.sql）。
type ShareUser struct {
	ShareID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	CreatedAt time.Time
}

type ShareTeam struct {
	ShareID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	TeamID    uuid.UUID `gorm:"type:uuid;primaryKey"`
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

func (s *GormStore) AddShareTeams(shareID uuid.UUID, teamIDs []uuid.UUID, now time.Time) error {
	if len(teamIDs) == 0 {
		return nil
	}
	rows := make([]ShareTeam, 0, len(teamIDs))
	for _, id := range teamIDs {
		rows = append(rows, ShareTeam{ShareID: shareID, TeamID: id, CreatedAt: now})
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

func (s *GormStore) ListShareTeamIDs(shareID uuid.UUID) ([]uuid.UUID, error) {
	var rows []ShareTeam
	err := s.db.Where("share_id = ?", shareID).Order("team_id").Find(&rows).Error
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.TeamID)
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
