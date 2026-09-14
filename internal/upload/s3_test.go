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

func (f *fakeS3API) GetObjectRange(_ context.Context, key string, start, length int64) (io.ReadCloser, error) {
	data, ok := f.objects[key]
	if !ok {
		return nil, errS3NoObject
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

// TestS3AppendAtPartIndexFromOffset AppendAt 的分片号由 offset 直接推导：
// 不同 offset 落不同分片对象，数值序拼接保持字节序（变长 PATCH 下
// offset/N 整除映射会碰撞，offset 一一映射单射）。
func TestS3AppendAtPartIndexFromOffset(t *testing.T) {
	s, api := newTestS3Storage()
	key := "tmp/sess"
	if n, err := s.AppendAt(key, 0, strings.NewReader("abc")); err != nil || n != 3 {
		t.Fatalf("append@0: n=%d err=%v", n, err)
	}
	// offset=2（与 offset=0 的区间重叠）：分片号不同，不覆盖 part-00000000。
	if n, err := s.AppendAt(key, 3, strings.NewReader("def")); err != nil || n != 3 {
		t.Fatalf("append@3: n=%d err=%v", n, err)
	}
	for _, k := range []string{key + "/part-00000000", key + "/part-00000003"} {
		if _, ok := api.objects[k]; !ok {
			t.Fatalf("missing %s; objects=%v", k, api.objects)
		}
	}
	r, err := s.Read(key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, _ := io.ReadAll(r)
	if string(data) != "abcdef" {
		t.Fatalf("concatenated = %q, want %q", data, "abcdef")
	}
	// 同 offset 重试覆盖同一分片（自愈）。
	if _, err := s.AppendAt(key, 3, strings.NewReader("DEF")); err != nil {
		t.Fatal(err)
	}
	r2, _ := s.Read(key)
	data2, _ := io.ReadAll(r2)
	r2.Close()
	if string(data2) != "abcDEF" {
		t.Fatalf("after retry = %q, want %q", data2, "abcDEF")
	}
}

// TestS3AppendAtomicCounterNoListAfterInit 兼容 Append：计数器初始化仅
// List 一次，此后分片号原子递增（删除中间分片不影响后续编号，不再复用）。
func TestS3AppendAtomicCounterNoListAfterInit(t *testing.T) {
	s, api := newTestS3Storage()
	key := "tmp/sess"
	for i := 0; i < 3; i++ {
		if _, err := s.Append(key, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	delete(api.objects, key+"/part-00000001")
	if _, err := s.Append(key, strings.NewReader("y")); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.objects[key+"/part-00000003"]; !ok {
		t.Fatalf("expected part-00000003 via counter, objects=%v", api.objects)
	}
}

// TestS3ReadRangeSingleObject 单对象 ReadRange：走 GetObject Range 原生分支。
func TestS3ReadRangeSingleObject(t *testing.T) {
	s, _ := newTestS3Storage()
	if err := s.Put("objects/u/1", strings.NewReader("hello world")); err != nil {
		t.Fatal(err)
	}
	r, err := s.ReadRange("objects/u/1", 6, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, _ := io.ReadAll(r)
	if string(data) != "world" {
		t.Fatalf("range read = %q, want %q", data, "world")
	}
}

// TestS3ReadRangePartsFallback 分片对象（无 key 本体）的 ReadRange：
// 流式拼接 + 跳过 start 后截取 length。
func TestS3ReadRangePartsFallback(t *testing.T) {
	s, _ := newTestS3Storage()
	key := "tmp/sess"
	var offset int64
	for _, part := range []string{"aaaa", "bbbb", "cccc"} {
		if _, err := s.AppendAt(key, offset, strings.NewReader(part)); err != nil {
			t.Fatal(err)
		}
		offset += int64(len(part))
	}
	// 拼接结果 "aaaabbbbcccc"：[5, 11) = "bbbccc"。
	r, err := s.ReadRange(key, 5, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, _ := io.ReadAll(r)
	if string(data) != "bbbccc" {
		t.Fatalf("range read = %q, want %q", data, "bbbccc")
	}
}
