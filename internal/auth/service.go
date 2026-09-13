package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidRefreshToken = errors.New("invalid refresh token")

type SessionStore interface {
	Create(session Session) error
	Rotate(tokenHash, replacementHash string, now time.Time, expiresAt time.Time) (Session, error)
	Revoke(tokenHash string, now time.Time) error
}

type Service struct {
	store           SessionStore
	jwtSecret       []byte
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
	now             func() time.Time
}

func NewService(store SessionStore, jwtSecret string, accessTokenTTL, refreshTokenTTL time.Duration) *Service {
	return &Service{store: store, jwtSecret: []byte(jwtSecret), accessTokenTTL: accessTokenTTL, refreshTokenTTL: refreshTokenTTL, now: time.Now}
}

func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost+2)
	return string(hash), err
}

func (s *Service) VerifyPassword(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

func (s *Service) NewSession(userID uuid.UUID) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	return token, s.store.Create(Session{ID: uuid.New(), UserID: userID, RefreshTokenHash: HashRefreshToken(token), CreatedAt: now, LastActiveAt: now, ExpiresAt: now.Add(s.refreshTokenTTL)})
}

func (s *Service) RotateRefreshToken(token string) (Session, string, error) {
	replacement, err := randomToken()
	if err != nil {
		return Session{}, "", err
	}
	now := s.now().UTC()
	session, err := s.store.Rotate(HashRefreshToken(token), HashRefreshToken(replacement), now, now.Add(s.refreshTokenTTL))
	if err != nil {
		return Session{}, "", ErrInvalidRefreshToken
	}
	return session, replacement, nil
}

func (s *Service) RevokeRefreshToken(token string) error {
	if err := s.store.Revoke(HashRefreshToken(token), s.now().UTC()); err != nil {
		return ErrInvalidRefreshToken
	}
	return nil
}

func (s *Service) AccessToken(userID uuid.UUID) (string, error) {
	now := s.now().UTC()
	claims := jwt.RegisteredClaims{Subject: userID.String(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTokenTTL))}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtSecret)
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
