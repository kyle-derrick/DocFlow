package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Storage 基于 S3 兼容对象存储（AWS S3 / MinIO / SeaweedFS）实现 Storage。
//
// Append 语义：S3 对象不可追加写，采用「分片对象」策略——key 作为前缀，
// 每次 Append 写入一个 `<key>/part-NNNNNNNN` 对象（8 位零填充保证字典序）；
// Read 先尝试读取 key 本体（Put 的产物），不存在时按分片序号顺序流式拼接；
// Delete 同时删除 key 本体与全部分片。
type S3Storage struct {
	api s3ObjectAPI
}

// s3ObjectAPI 是 S3Storage 依赖的最小对象操作集合，便于脱离网络测试。
type s3ObjectAPI interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
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

// Append 写入下一个分片对象；分片序号由当前已有分片决定（最大序号 + 1）。
func (s *S3Storage) Append(key string, r io.Reader) (int64, error) {
	if err := s3ValidateKey(key); err != nil {
		return 0, err
	}
	ctx := context.Background()
	names, err := s.api.ListObjects(ctx, key+"/")
	if err != nil {
		return 0, err
	}
	parts := s3SortedPartKeys(key, names)
	next := 0
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if n, perr := strconv.Atoi(last[len(key)+len(s3PartSep):]); perr == nil {
			next = n + 1
		}
	}
	cr := &countingReader{r: r}
	if err := s.api.PutObject(ctx, s3PartKey(key, next), cr); err != nil {
		return cr.n, err
	}
	return cr.n, nil
}

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
