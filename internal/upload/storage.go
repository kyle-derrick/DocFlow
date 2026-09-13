package upload

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrInvalidKey = errors.New("invalid storage key")

type Storage interface {
	Put(key string, r io.Reader) error
	Append(key string, r io.Reader) (int64, error)
	Read(key string) (io.ReadCloser, error)
	Delete(key string) error
}

type LocalStorage struct{ root string }

func NewLocalStorage(root string) (*LocalStorage, error) {
	if root == "" {
		return nil, errors.New("storage root is required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &LocalStorage{root: root}, nil
}
func (s *LocalStorage) path(key string) (string, error) {
	clean := filepath.Clean(key)
	if clean == "." || filepath.IsAbs(key) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrInvalidKey
	}
	p := filepath.Join(s.root, clean)
	rel, err := filepath.Rel(s.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrInvalidKey
	}
	return p, nil
}
func (s *LocalStorage) Put(key string, r io.Reader) error {
	p, e := s.path(key)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = io.Copy(f, r)
	return e
}
func (s *LocalStorage) Append(key string, r io.Reader) (int64, error) {
	p, e := s.path(key)
	if e != nil {
		return 0, e
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return 0, e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	n, e := io.Copy(f, r)
	return n, e
}
func (s *LocalStorage) Read(key string) (io.ReadCloser, error) {
	p, e := s.path(key)
	if e != nil {
		return nil, e
	}
	return os.Open(p)
}
func (s *LocalStorage) Delete(key string) error {
	p, e := s.path(key)
	if e != nil {
		return e
	}
	e = os.Remove(p)
	if os.IsNotExist(e) {
		return nil
	}
	return e
}
