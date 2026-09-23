package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"strings"
	"time"
)

type WebDAVToken struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID     uuid.UUID `gorm:"type:uuid;not null;index"`
	Name       string    `gorm:"size:100;not null"`
	TokenHash  string    `gorm:"size:64;uniqueIndex;not null"`
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

func (WebDAVToken) TableName() string { return "webdav_tokens" }

type WebDAVStore struct{ db *gorm.DB }

func NewWebDAVStore(db *gorm.DB) *WebDAVStore { return &WebDAVStore{db: db} }
func webDAVHash(s string) string              { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
func (s *WebDAVStore) Create(user uuid.UUID, name string, days int) (WebDAVToken, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 100 {
		return WebDAVToken{}, "", errors.New("invalid name")
	}
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return WebDAVToken{}, "", e
	}
	raw := "dfdav_" + base64.RawURLEncoding.EncodeToString(b)
	now := time.Now().UTC()
	t := WebDAVToken{ID: uuid.New(), UserID: user, Name: name, TokenHash: webDAVHash(raw), CreatedAt: now}
	if days > 0 {
		x := now.AddDate(0, 0, days)
		t.ExpiresAt = &x
	}
	return t, raw, s.db.Create(&t).Error
}
func (s *WebDAVStore) List(user uuid.UUID) ([]WebDAVToken, error) {
	var out []WebDAVToken
	e := s.db.Where("user_id=? AND revoked_at IS NULL", user).Order("created_at desc").Find(&out).Error
	return out, e
}
func (s *WebDAVStore) Revoke(user, id uuid.UUID) error {
	now := time.Now().UTC()
	r := s.db.Model(&WebDAVToken{}).Where("id=? AND user_id=? AND revoked_at IS NULL", id, user).Update("revoked_at", now)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
func (s *WebDAVStore) Verify(username, token string) (uuid.UUID, error) {
	var t WebDAVToken
	if e := s.db.Where("token_hash=? AND revoked_at IS NULL", webDAVHash(token)).First(&t).Error; e != nil {
		return uuid.Nil, e
	}
	if t.ExpiresAt != nil && time.Now().UTC().After(*t.ExpiresAt) {
		return uuid.Nil, errors.New("expired")
	}
	var u User
	if e := s.db.First(&u, "id=? AND (email=? OR username=?)", t.UserID, username, username).Error; e != nil || u.Status != StatusActive {
		return uuid.Nil, errors.New("invalid credentials")
	}
	now := time.Now().UTC()
	s.db.Model(&t).Update("last_used_at", now)
	return t.UserID, nil
}
