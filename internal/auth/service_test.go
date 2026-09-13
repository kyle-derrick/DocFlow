package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeStore struct {
	sessions map[string]Session
}

func newFakeStore() *fakeStore { return &fakeStore{sessions: make(map[string]Session)} }
func (f *fakeStore) Create(session Session) error {
	f.sessions[session.RefreshTokenHash] = session
	return nil
}
func (f *fakeStore) Rotate(oldHash, newHash string, now, expiresAt time.Time) (Session, error) {
	session, ok := f.sessions[oldHash]
	if !ok || session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		return Session{}, errors.New("invalid")
	}
	delete(f.sessions, oldHash)
	session.RefreshTokenHash, session.LastActiveAt, session.ExpiresAt = newHash, now, expiresAt
	f.sessions[newHash] = session
	return session, nil
}
func (f *fakeStore) Revoke(hash string, now time.Time) error {
	session, ok := f.sessions[hash]
	if !ok || session.RevokedAt != nil {
		return errors.New("invalid")
	}
	session.RevokedAt = &now
	f.sessions[hash] = session
	return nil
}

func TestHashRefreshTokenDeterministic(t *testing.T) {
	if HashRefreshToken("secret") != HashRefreshToken("secret") {
		t.Fatal("hash should be deterministic")
	}
	if HashRefreshToken("secret") == HashRefreshToken("other") {
		t.Fatal("different tokens must hash differently")
	}
}

func TestRefreshRotationRejectsReplay(t *testing.T) {
	store := newFakeStore()
	service := NewService(store, "a sufficiently long test secret", time.Minute, time.Hour)
	original, err := service.NewSession(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	_, replacement, err := service.RotateRefreshToken(original)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == original {
		t.Fatal("rotation must replace token")
	}
	if _, _, err := service.RotateRefreshToken(original); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("replay error = %v", err)
	}
	if _, _, err := service.RotateRefreshToken(replacement); err != nil {
		t.Fatalf("replacement should work: %v", err)
	}
}

func TestRefreshRevocation(t *testing.T) {
	store := newFakeStore()
	service := NewService(store, "a sufficiently long test secret", time.Minute, time.Hour)
	token, err := service.NewSession(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeRefreshToken(token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.RotateRefreshToken(token); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("revoked token error = %v", err)
	}
}
