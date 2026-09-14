package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Storage 基于 S3 兼容对象存储（AWS S3 / MinIO / SeaweedFS）实现 Storage。
//
// Append 语义：S3 对象不可追加写，采用「分片对象」策略——key 作为前缀，
// 每次 Append 写入一个 `<key>/part-NNNNNNNN` 对象（8 位零填充；Read 侧按
// 数值序拼接，超出 8 位仍正确）；Read 先尝试读取 key 本体（Put 的产物），
// 不存在时按分片序号顺序流式拼接；Delete 同时删除 key 本体与全部分片。
//
// 生产路径：upload.Service 检测 S3Storage 实现 OffsetAppender 后走 AppendAt
// （分片号由 offset 直接推导），Append 仅作为兼容回退保留（进程内原子计数器）。
type S3Storage struct {
	api s3ObjectAPI
	// nextPart 每 key 的下一分片号计数器（兼容 Append 路径专用；AppendAt
	// 由 offset 直接推导分片号，不经过计数器）。
	nextPart sync.Map // key → *atomic.Int64
}

// s3ObjectAPI 是 S3Storage 依赖的最小对象操作集合，便于脱离网络测试。
type s3ObjectAPI interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	// GetObjectRange 读取 [start, start+length) 区间（GetObject Range）。
	GetObjectRange(ctx context.Context, key string, start, length int64) (io.ReadCloser, error)
	PutObject(ctx context.Context, key string, r io.Reader) error
	DeleteObject(ctx context.Context, key string) error
	// ListObjects 返回以 prefix 开头的全部对象 key（跨页聚合，去重排序由调用方处理）。
	ListObjects(ctx context.Context, prefix string) ([]string, error)
}

var errS3NoObject = errors.New("s3 object not found")

const (
	s3PartSep = "/part-"
)

// NewS3Storage 创建 S3 兼容存储。endpoint 为空表示使用 AWS 默认端点；
// accessKey 为空时使用匿名凭证（部分 SeaweedFS 部署不启用认证）。
func NewS3Storage(endpoint, bucket, region, accessKey, secretKey string, pathStyle bool) (*S3Storage, error) {
	if bucket == "" {
		return nil, errors.New("s3 bucket is required")
	}
	if region == "" {
		region = "us-east-1"
	}
	cfg := aws.Config{Region: region}
	if accessKey != "" {
		cfg.Credentials = credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")
	} else {
		cfg.Credentials = aws.AnonymousCredentials{}
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = pathStyle
	})
	return &S3Storage{api: &awsS3{client: client, uploader: manager.NewUploader(client), bucket: bucket}}, nil
}

// s3PartKey 返回第 n 个分片的对象 key（纯函数，8 位零填充保证字典序）。
func s3PartKey(key string, n int) string {
	return fmt.Sprintf("%s%s%08d", key, s3PartSep, n)
}

// s3ValidateKey 校验对象 key：非空、非绝对路径、不含 ".." 路径段。
func s3ValidateKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") {
		return ErrInvalidKey
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrInvalidKey
		}
	}
	return nil
}

// s3SortedPartKeys 从 prefix 下的对象名中筛选出属于 key 的分片，
// 按分片序号升序返回（纯函数）。非分片对象或畸形分片名被忽略。
func s3SortedPartKeys(key string, names []string) []string {
	prefix := key + s3PartSep
	byIndex := make(map[int]string, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		n, err := strconv.Atoi(name[len(prefix):])
		if err != nil || n < 0 {
			continue
		}
		byIndex[n] = name
	}
	indices := make([]int, 0, len(byIndex))
	for n := range byIndex {
		indices = append(indices, n)
	}
	sort.Ints(indices)
	parts := make([]string, 0, len(indices))
	for _, n := range indices {
		parts = append(parts, byIndex[n])
	}
	return parts
}

func (s *S3Storage) Put(key string, r io.Reader) error {
	if err := s3ValidateKey(key); err != nil {
		return err
	}
	return s.api.PutObject(context.Background(), key, r)
}

// Append 写入下一个分片对象（兼容回退路径）：分片号由进程内原子计数器
// （sync.Map）分配——计数器缺失时一次性 ListObjects 初始化（现存最大序号+1），
// 之后不再依赖 List，消除「List-then-put」在列表可见性延迟下复用已占序号的
// 竞态；PutObject 失败时归还序号供重试复用。
// 局限：计数器为进程内状态，多实例部署下各实例独立初始化（以 List 结果为
// 基线仍可能重叠）；生产上传路径走 AppendAt（分片号=offset，无跨实例状态）。
func (s *S3Storage) Append(key string, r io.Reader) (int64, error) {
	if err := s3ValidateKey(key); err != nil {
		return 0, err
	}
	ctx := context.Background()
	idx, release := s.claimPartIndex(ctx, key)
	cr := &countingReader{r: r}
	if err := s.api.PutObject(ctx, s3PartKey(key, int(idx)), cr); err != nil {
		release()
		return cr.n, err
	}
	return cr.n, nil
}

// claimPartIndex 认领下一个分片号并返回归还函数（写失败时回退计数器）。
func (s *S3Storage) claimPartIndex(ctx context.Context, key string) (int64, func()) {
	if v, ok := s.nextPart.Load(key); ok {
		counter := v.(*atomic.Int64)
		idx := counter.Add(1) - 1
		return idx, func() { counter.Add(-1) }
	}
	next := int64(0)
	if names, err := s.api.ListObjects(ctx, key+"/"); err == nil {
		if parts := s3SortedPartKeys(key, names); len(parts) > 0 {
			if n, perr := strconv.Atoi(parts[len(parts)-1][len(key)+len(s3PartSep):]); perr == nil {
				next = int64(n) + 1
			}
		}
	}
	fresh := new(atomic.Int64)
	fresh.Store(next + 1)
	if actual, loaded := s.nextPart.LoadOrStore(key, fresh); loaded {
		counter := actual.(*atomic.Int64)
		idx := counter.Add(1) - 1
		return idx, func() { counter.Add(-1) }
	}
	return next, func() { fresh.Add(-1) }
}

// AppendAt 在绝对 offset 处写入分片（OffsetAppender，生产上传路径）：
// 分片号 = offset（即 offset/chunkSize 在 chunkSize=1 下的整除映射——
// PATCH 长度任意可变时，offset/N 整除映射会让两次不同 offset 落到同一分片
// 造成覆写损坏，按字节 offset 一一映射则必然单射）。读取侧 s3SortedPartKeys
// 按数值序拼接，分片号递增即字节序；同一 offset 重试覆盖同一分片，失败残留
// 天然自愈。
func (s *S3Storage) AppendAt(key string, offset int64, r io.Reader) (int64, error) {
	if err := s3ValidateKey(key); err != nil {
		return 0, err
	}
	if offset < 0 || offset > int64(maxInt) {
		return 0, ErrInvalidKey
	}
	cr := &countingReader{r: r}
	if err := s.api.PutObject(context.Background(), s3PartKey(key, int(offset)), cr); err != nil {
		return cr.n, err
	}
	return cr.n, nil
}

const maxInt = int64(^uint(0) >> 1)

func (s *S3Storage) Read(key string) (io.ReadCloser, error) {
	if err := s3ValidateKey(key); err != nil {
		return nil, err
	}
	ctx := context.Background()
	// 先读 key 本体（Put 的产物）；对象不存在时回退到分片拼接（Append 的产物）。
	r, err := s.api.GetObject(ctx, key)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, errS3NoObject) {
		return nil, err
	}
	names, err := s.api.ListObjects(ctx, key+"/")
	if err != nil {
		return nil, err
	}
	parts := s3SortedPartKeys(key, names)
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: %s", errS3NoObject, key)
	}
	return &s3PartReader{api: s.api, keys: parts}, nil
}

// ReadRange 读取 [start, start+length) 区间（RangeReader）：单对象直接用
// GetObject Range（不缓冲、不依赖 Seeker）；对象不存在时回退到分片拼接流，
// 跳过 start 字节后截取 length（分片仅存在于 tmp/ 上传中对象，下载路径的
// objects/* 终态对象恒走原生 Range 分支）。
func (s *S3Storage) ReadRange(key string, start, length int64) (io.ReadCloser, error) {
	if err := s3ValidateKey(key); err != nil {
		return nil, err
	}
	if start < 0 || length < 0 {
		return nil, ErrInvalidKey
	}
	if length == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	ctx := context.Background()
	r, err := s.api.GetObjectRange(ctx, key, start, length)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, errS3NoObject) {
		return nil, err
	}
	full, err := s.Read(key)
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(io.Discard, full, start); err != nil {
		full.Close()
		return nil, err
	}
	return &sectionReadCloser{r: io.LimitReader(full, length), c: full}, nil
}

func (s *S3Storage) Delete(key string) error {
	if err := s3ValidateKey(key); err != nil {
		return err
	}
	ctx := context.Background()
	if err := s.api.DeleteObject(ctx, key); err != nil && !errors.Is(err, errS3NoObject) {
		return err
	}
	names, err := s.api.ListObjects(ctx, key+"/")
	if err != nil {
		return err
	}
	for _, part := range s3SortedPartKeys(key, names) {
		if err := s.api.DeleteObject(ctx, part); err != nil && !errors.Is(err, errS3NoObject) {
			return err
		}
	}
	return nil
}

// s3PartReader 按序流式拼接分片：当前分片读到 EOF 才打开下一个（懒加载）。
type s3PartReader struct {
	api  s3ObjectAPI
	keys []string
	pos  int
	cur  io.ReadCloser
}

func (m *s3PartReader) Read(p []byte) (int, error) {
	for {
		if m.cur == nil {
			if m.pos >= len(m.keys) {
				return 0, io.EOF
			}
			r, err := m.api.GetObject(context.Background(), m.keys[m.pos])
			if err != nil {
				return 0, err
			}
			m.cur = r
		}
		n, err := m.cur.Read(p)
		if n > 0 {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			m.cur.Close()
			m.cur = nil
			m.pos++
			continue
		}
		if err != nil {
			return 0, err
		}
	}
}

func (m *s3PartReader) Close() error {
	if m.cur != nil {
		err := m.cur.Close()
		m.cur = nil
		return err
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// awsS3 用 aws-sdk-go-v2 适配 s3ObjectAPI。
type awsS3 struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

func (a *awsS3) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := a.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &a.bucket, Key: &key})
	if err != nil {
		var noKey *s3types.NoSuchKey
		if errors.As(err, &noKey) {
			return nil, errS3NoObject
		}
		return nil, err
	}
	return out.Body, nil
}

func (a *awsS3) GetObjectRange(ctx context.Context, key string, start, length int64) (io.ReadCloser, error) {
	// Range: bytes=start-(start+length-1)，含端点（RFC 7233）。
	rng := fmt.Sprintf("bytes=%d-%d", start, start+length-1)
	out, err := a.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &a.bucket, Key: &key, Range: &rng})
	if err != nil {
		var noKey *s3types.NoSuchKey
		if errors.As(err, &noKey) {
			return nil, errS3NoObject
		}
		return nil, err
	}
	return out.Body, nil
}

func (a *awsS3) PutObject(ctx context.Context, key string, r io.Reader) error {
	// Uploader 对可 Seek 的 Body 走单次 PUT；不可 Seek（流式）时自动分片上传。
	_, err := a.uploader.Upload(ctx, &s3.PutObjectInput{Bucket: &a.bucket, Key: &key, Body: r})
	return err
}

func (a *awsS3) DeleteObject(ctx context.Context, key string) error {
	// S3 DELETE 对不存在的 key 也返回成功，无需区分。
	_, err := a.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &a.bucket, Key: &key})
	return err
}

func (a *awsS3) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	p := s3.NewListObjectsV2Paginator(a.client, &s3.ListObjectsV2Input{Bucket: &a.bucket, Prefix: &prefix})
	var keys []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}
	return keys, nil
}
