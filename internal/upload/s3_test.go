package upload

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"testing"
)

// fakeS3API 用内存 map 模拟对象存储，脱离网络验证 S3Storage 的分片逻辑。
type fakeS3API struct {
	objects map[string][]byte
}

func newFakeS3API() *fakeS3API { return &fakeS3API{objects: make(map[string][]byte)} }

func (f *fakeS3API) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := f.objects[key]
	if !ok {
		return nil, errS3NoObject
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeS3API) PutObject(_ context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = data
	return nil
}

func (f *fakeS3API) DeleteObject(_ context.Context, key string) error {
	if _, ok := f.objects[key]; !ok {
		return errS3NoObject
	}
	delete(f.objects, key)
	return nil
}

func (f *fakeS3API) ListObjects(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func newTestS3Storage() (*S3Storage, *fakeS3API) {
	api := newFakeS3API()
	return &S3Storage{api: api}, api
}

func TestS3PartKeyPadded(t *testing.T) {
	if got, want := s3PartKey("tmp/x", 1), "tmp/x/part-00000001"; got != want {
		t.Fatalf("s3PartKey = %q, want %q", got, want)
	}
	if got, want := s3PartKey("tmp/x", 123456789), "tmp/x/part-123456789"; got != want {
		t.Fatalf("s3PartKey = %q, want %q", got, want)
	}
}

func TestS3SortedPartKeys(t *testing.T) {
	key := "tmp/abc"
	names := []string{
		key + "/part-00000003",
		"tmp/abc-other", // 前缀近似但不匹配
		key + "/part-00000001",
		key + "/part-00000002",
		key + "/part-notanumber", // 畸形分片名被忽略
		key + "/part-00000000",
		"tmp/abcd/part-00000001", // 同前缀的其他 key
	}
	got := s3SortedPartKeys(key, names)
	want := []string{key + "/part-00000000", key + "/part-00000001", key + "/part-00000002", key + "/part-00000003"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestS3PutReadSingleObject(t *testing.T) {
	s, _ := newTestS3Storage()
	if err := s.Put("objects/u/1", strings.NewReader("hello world")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Read("objects/u/1")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, _ := io.ReadAll(r)
	if string(data) != "hello world" {
		t.Fatalf("read = %q", data)
	}
}

func TestS3AppendReadConcatenation(t *testing.T) {
	s, api := newTestS3Storage()
	key := "tmp/sess"
	n1, err := s.Append(key, strings.NewReader("hello,"))
	if err != nil || n1 != 6 {
		t.Fatalf("append1: n=%d err=%v", n1, err)
	}
	n2, err := s.Append(key, strings.NewReader(" "))
	if err != nil || n2 != 1 {
		t.Fatalf("append2: n=%d err=%v", n2, err)
	}
	n3, err := s.Append(key, strings.NewReader("世界!"))
	if err != nil || n3 != 7 {
		t.Fatalf("append3: n=%d err=%v", n3, err)
	}
	// 分片对象按序落盘（编号从 0 开始）
	for _, k := range []string{key + "/part-00000000", key + "/part-00000001", key + "/part-00000002"} {
		if _, ok := api.objects[k]; !ok {
			t.Fatalf("missing part object %s; objects=%v", k, api.objects)
		}
	}
	r, err := s.Read(key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello, 世界!" {
		t.Fatalf("concatenated = %q", data)
	}
}

func TestS3ReadStreamingOpensPartsLazily(t *testing.T) {
	s, _ := newTestS3Storage()
	key := "tmp/sess"
	if _, err := s.Append(key, strings.NewReader("aaa")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(key, strings.NewReader("bbb")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Read(key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	buf := make([]byte, 3)
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "aaa" {
		t.Fatalf("first chunk = %q err=%v", buf, err)
	}
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "bbb" {
		t.Fatalf("second chunk = %q err=%v", buf, err)
	}
	if _, err := r.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestS3AppendContinuesNumbering(t *testing.T) {
	s, api := newTestS3Storage()
	key := "tmp/sess"
	for i := 0; i < 3; i++ {
		if _, err := s.Append(key, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	// 删除中间分片后，下一次 Append 仍取最大序号 + 1
	delete(api.objects, key+"/part-00000001")
	if _, err := s.Append(key, strings.NewReader("y")); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.objects[key+"/part-00000003"]; !ok {
		t.Fatalf("expected part-00000003, objects=%v", api.objects)
	}
}

func TestS3DeleteRemovesAllParts(t *testing.T) {
	s, api := newTestS3Storage()
	key := "tmp/sess"
	for _, part := range []string{"11", "22"} {
		if _, err := s.Append(key, strings.NewReader(part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put("objects/final", strings.NewReader("zz")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(key); err != nil {
		t.Fatal(err)
	}
	if len(api.objects) != 1 {
		t.Fatalf("expected only objects/final left, got %v", api.objects)
	}
	// 删除不存在的 key 幂等成功
	if err := s.Delete("tmp/none"); err != nil {
		t.Fatalf("delete missing key: %v", err)
	}
}

func TestS3ReadMissing(t *testing.T) {
	s, _ := newTestS3Storage()
	if _, err := s.Read("tmp/none"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestS3InvalidKeys(t *testing.T) {
	s, _ := newTestS3Storage()
	for _, key := range []string{"", "/abs", "a/../b", "..", "tmp//x"} {
		if _, err := s.Append(key, strings.NewReader("x")); err == nil {
			t.Fatalf("Append(%q) expected error", key)
		}
		if err := s.Put(key, strings.NewReader("x")); err == nil {
			t.Fatalf("Put(%q) expected error", key)
		}
		if _, err := s.Read(key); err == nil {
			t.Fatalf("Read(%q) expected error", key)
		}
		if err := s.Delete(key); err == nil {
			t.Fatalf("Delete(%q) expected error", key)
		}
	}
}

func TestS3NewS3StorageRequiresBucket(t *testing.T) {
	if _, err := NewS3Storage("", "", "", "", "", true); err == nil {
		t.Fatal("expected error for empty bucket")
	}
}
