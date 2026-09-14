package upload

import (
	"bytes"
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

// OffsetAppender 为 Storage 的可选能力：在绝对字节偏移 offset 处写入。
// 上传服务检测到实现时 Append 走本接口，分片定位由 offset 直接推导
// （S3 分片号 = offset；LocalStorage Seek 到 offset 覆写），不再依赖
// ListObjects 推断下一分片，消除「List-then-put」竞态；同 offset 重试
// 覆盖同一位置，失败残留数据天然自愈。
type OffsetAppender interface {
	AppendAt(key string, offset int64, r io.Reader) (int64, error)
}

// RangeReader 为 Storage 的可选能力：读取 [start, start+length) 字节区间，
// 不要求返回 io.Seeker（S3 用 GetObject Range 原生实现）。LocalStorage 与
// S3Storage 均实现；HTTP 下载/预览消费点统一经 ReadSection 读取。
//
// 说明：未直接并入 Storage 接口——onlyoffice 等包的测试替身按四方法实现
// upload.Storage，接口扩展会破坏不可修改文件的编译；ReadSection 对未实现
// 者提供 Seek/内存退化路径。
type RangeReader interface {
	ReadRange(key string, start, length int64) (io.ReadCloser, error)
}

// sectionReadCloser 包装区间读取流并透传底层 Close。
type sectionReadCloser struct {
	r io.Reader
	c io.Closer
}

func (s *sectionReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *sectionReadCloser) Close() error               { return s.c.Close() }

// ReadSection 读取 [start, start+length) 字节区间：优先走原生 ReadRange
// （LocalStorage Open+Seek / S3 GetObject Range，不缓冲全量、不依赖 Seeker）；
// 旧存储（未实现 RangeReader，如测试替身）退化为 Read+Seek，再不行整读后
// SectionReader（仅内存假存储会走到该分支）。HTTP 下载/预览/分享与
// ONLYOFFICE 回源下载消费点统一经本函数读取。
func ReadSection(s Storage, key string, start, length int64) (io.ReadCloser, error) {
	if start < 0 || length < 0 {
		return nil, ErrInvalidKey
	}
	if rr, ok := s.(RangeReader); ok {
		return rr.ReadRange(key, start, length)
	}
	r, err := s.Read(key)
	if err != nil {
		return nil, err
	}
	if seeker, ok := r.(io.Seeker); ok {
		if _, err := seeker.Seek(start, io.SeekStart); err != nil {
			r.Close()
			return nil, err
		}
		return &sectionReadCloser{r: io.LimitReader(r, length), c: r}, nil
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return nil, err
	}
	if start > int64(len(data)) {
		start = int64(len(data))
	}
	end := start + length
	if end > int64(len(data)) || end < start {
		end = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data[start:end])), nil
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

// AppendAt 在绝对 offset 处覆写（OffsetAppender）：O_RDWR 打开后 Seek(offset)。
// offset 超出当前文件尾时由内核补洞填充（正常流程 offset 恒等于已写字节数，
// 不会出现空洞）。写失败/超限残留的字节由同 offset 重试覆盖，无需 truncate。
func (s *LocalStorage) AppendAt(key string, offset int64, r io.Reader) (int64, error) {
	if offset < 0 {
		return 0, ErrInvalidKey
	}
	p, e := s.path(key)
	if e != nil {
		return 0, e
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return 0, e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	if _, e = f.Seek(offset, io.SeekStart); e != nil {
		return 0, e
	}
	return io.Copy(f, r)
}

func (s *LocalStorage) Read(key string) (io.ReadCloser, error) {
	p, e := s.path(key)
	if e != nil {
		return nil, e
	}
	return os.Open(p)
}

// ReadRange 读取 [start, start+length) 区间（RangeReader）：Open+Seek，
// 不缓冲全量、不要求调用方处理 Seeker。
func (s *LocalStorage) ReadRange(key string, start, length int64) (io.ReadCloser, error) {
	if start < 0 || length < 0 {
		return nil, ErrInvalidKey
	}
	p, e := s.path(key)
	if e != nil {
		return nil, e
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	if _, e = f.Seek(start, io.SeekStart); e != nil {
		f.Close()
		return nil, e
	}
	return &sectionReadCloser{r: io.LimitReader(f, length), c: f}, nil
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
