package share

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
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
