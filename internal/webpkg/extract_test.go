package webpkg

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/upload"
)

// zipSpec 描述一个待构造的 zip 条目。
type zipSpec struct {
	name string
	data string
	mode os.FileMode // 0 表示常规文件
	dir  bool
}

// buildZip 手工构造 zip 字节（含符号链接等特殊模式条目）。
func buildZip(t *testing.T, specs []zipSpec) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, s := range specs {
		hdr := &zip.FileHeader{Name: s.name, Method: zip.Deflate}
		mode := s.mode
		if mode == 0 {
			mode = 0o644
			if s.dir {
				mode = 0o755 | os.ModeDir
			}
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("create header %q: %v", s.name, err)
		}
		if !s.dir && s.data != "" {
			if _, err := w.Write([]byte(s.data)); err != nil {
				t.Fatalf("write %q: %v", s.name, err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func mustLocal(t *testing.T) *upload.LocalStorage {
	t.Helper()
	s, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readKey(t *testing.T, s *upload.LocalStorage, key string) string {
	t.Helper()
	r, err := s.Read(key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExtractRootIndex(t *testing.T) {
	storage := mustLocal(t)
	data := buildZip(t, []zipSpec{
		{name: "index.html", data: "<h1>root</h1>"},
		{name: "assets/", dir: true},
		{name: "assets/app.js", data: "console.log(1)"},
		{name: "assets/logo.svg", data: "<svg/>"},
	})
	stats, err := Extract(storage, bytes.NewReader(data), "webpkg/pid1", DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if stats.EntryCount != 3 || stats.TotalSize != int64(len("<h1>root</h1>")+len("console.log(1)")+len("<svg/>")) {
		t.Fatalf("stats = %+v", stats)
	}
	if got := readKey(t, storage, "webpkg/pid1/index.html"); got != "<h1>root</h1>" {
		t.Fatalf("index.html = %q", got)
	}
	if got := readKey(t, storage, "webpkg/pid1/assets/app.js"); got != "console.log(1)" {
		t.Fatalf("app.js = %q", got)
	}
	// 清单包含全部写入条目。
	manifest := readKey(t, storage, "webpkg/pid1/"+manifestName)
	for _, want := range []string{"index.html", "assets/app.js", "assets/logo.svg"} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("manifest missing %q: %q", want, manifest)
		}
	}
}

// TestExtractSoleTopDirStrip：唯一顶层目录（site/）内的 index.html 自动剥除。
func TestExtractSoleTopDirStrip(t *testing.T) {
	storage := mustLocal(t)
	data := buildZip(t, []zipSpec{
		{name: "site/", dir: true},
		{name: "site/index.html", data: "<h1>wrapped</h1>"},
		{name: "site/css/main.css", data: "body{}"},
	})
	stats, err := Extract(storage, bytes.NewReader(data), "webpkg/pid2", DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if stats.EntryCount != 2 {
		t.Fatalf("entry count = %d, want 2", stats.EntryCount)
	}
	if got := readKey(t, storage, "webpkg/pid2/index.html"); got != "<h1>wrapped</h1>" {
		t.Fatalf("stripped index.html = %q", got)
	}
	if got := readKey(t, storage, "webpkg/pid2/css/main.css"); got != "body{}" {
		t.Fatalf("stripped css = %q", got)
	}
}

func TestExtractRejections(t *testing.T) {
	tests := []struct {
		name  string
		specs []zipSpec
		// raw 非空时直接作为 zip 字节（构造非法 zip 内容）。
		raw      string
		limits   Limits
		contains string
	}{
		{
			name:     "not a zip",
			raw:      "this is not a zip archive at all",
			contains: "not a valid zip",
		},
		{
			name: "missing index.html",
			specs: []zipSpec{
				{name: "about.html", data: "x"},
			},
			contains: "missing index.html",
		},
		{
			name: "two top dirs without root index",
			specs: []zipSpec{
				{name: "a/index.html", data: "x"},
				{name: "b/page.html", data: "x"},
			},
			contains: "missing index.html",
		},
		{
			name: "zip slip traversal",
			specs: []zipSpec{
				{name: "../evil.txt", data: "x"},
				{name: "index.html", data: "x"},
			},
			contains: "traversal",
		},
		{
			name: "zip slip absolute path",
			specs: []zipSpec{
				{name: "/etc/passwd", data: "x"},
				{name: "index.html", data: "x"},
			},
			contains: "absolute",
		},
		{
			name: "zip slip drive letter",
			specs: []zipSpec{
				{name: "C:/evil.txt", data: "x"},
				{name: "index.html", data: "x"},
			},
			contains: "drive-letter",
		},
		{
			name: "control character in name",
			specs: []zipSpec{
				{name: "bad\nname.txt", data: "x"},
				{name: "index.html", data: "x"},
			},
			contains: "control character",
		},
		{
			name: "symlink entry",
			specs: []zipSpec{
				{name: "link.txt", mode: os.ModeSymlink | 0o777},
				{name: "index.html", data: "x"},
			},
			contains: "non-regular",
		},
		{
			name: "device entry",
			specs: []zipSpec{
				{name: "dev", mode: os.ModeDevice | 0o644},
				{name: "index.html", data: "x"},
			},
			contains: "non-regular",
		},
		{
			name: "too many entries",
			specs: []zipSpec{
				{name: "index.html", data: "x"},
				{name: "a.txt", data: "x"},
				{name: "b.txt", data: "x"},
			},
			limits:   Limits{MaxEntries: 2},
			contains: "entries exceed",
		},
		{
			name: "single file too large",
			specs: []zipSpec{
				{name: "index.html", data: strings.Repeat("a", 4096)},
			},
			limits:   Limits{MaxFileSize: 1024},
			contains: "exceeds limit",
		},
		{
			name: "total size exceeded",
			specs: []zipSpec{
				{name: "index.html", data: strings.Repeat("a", 700)},
				{name: "b.txt", data: strings.Repeat("b", 700)},
			},
			limits:   Limits{MaxFileSize: 1024, MaxTotalSize: 1024},
			contains: "exceeds limit",
		},
		{
			name: "depth exceeded",
			specs: []zipSpec{
				{name: "a/b/c/d/e/f/g/h/i/j/k/l/deep.html", data: "x"},
			},
			contains: "depth",
		},
		{
			name: "archive only dirs",
			specs: []zipSpec{
				{name: "site/", dir: true},
			},
			contains: "no files",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := mustLocal(t)
			limits := DefaultLimits()
			if test.limits != (Limits{}) {
				limits = test.limits
			}
			var r *bytes.Reader
			if test.raw != "" {
				r = bytes.NewReader([]byte(test.raw))
			} else {
				r = bytes.NewReader(buildZip(t, test.specs))
			}
			_, err := Extract(storage, r, "webpkg/x", limits)
			if err == nil {
				t.Fatalf("unexpectedly accepted")
			}
			if !errors.Is(err, ErrWebpkgInvalid) {
				t.Fatalf("error = %v, want ErrWebpkgInvalid wrap", err)
			}
			if !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v, want contains %q", err, test.contains)
			}
			// 失败时不得留下 index/manifest 等半写状态。
			if _, rerr := storage.Read("webpkg/x/" + manifestName); rerr == nil {
				t.Fatal("manifest written despite failure")
			}
		})
	}
}

// TestExtractActualCopyEnforcesLimits：声明大小与实际一致的诚实 zip 在
// 实际拷贝路径上同样被单文件上限拦截（Read 截断窗口 + 二次读报错）。
func TestExtractActualCopyEnforcesLimits(t *testing.T) {
	storage := mustLocal(t)
	// 4KB 内容、声明与实际一致；单文件上限 1024 → 预检即拒绝（同一限制器）。
	data := buildZip(t, []zipSpec{{name: "index.html", data: strings.Repeat("a", 4096)}})
	_, err := Extract(storage, bytes.NewReader(data), "webpkg/y", Limits{MaxFileSize: 1024, MaxTotalSize: 1 << 20})
	if err == nil || !errors.Is(err, ErrWebpkgInvalid) {
		t.Fatalf("err = %v, want ErrWebpkgInvalid", err)
	}
}

// TestRemoveManifestDriven：Extract → Remove 幂等重建路径的旧 key 清理。
func TestRemoveManifestDriven(t *testing.T) {
	storage := mustLocal(t)
	data := buildZip(t, []zipSpec{
		{name: "index.html", data: "v1"},
		{name: "old.txt", data: "stale"},
	})
	if _, err := Extract(storage, bytes.NewReader(data), "webpkg/p", DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	Remove(storage, "webpkg/p")
	for _, key := range []string{"webpkg/p/index.html", "webpkg/p/old.txt", "webpkg/p/" + manifestName} {
		if _, err := storage.Read(key); err == nil {
			t.Fatalf("key %q still exists after Remove", key)
		}
	}
	// 无清单前缀：空操作不报错。
	Remove(storage, "webpkg/never-extracted")
}

// TestExtractRebuildReplacesOldEntries：重建（同前缀）后旧条目被清理、内容更新。
func TestExtractRebuildReplacesOldEntries(t *testing.T) {
	storage := mustLocal(t)
	if _, err := Extract(storage, bytes.NewReader(buildZip(t, []zipSpec{
		{name: "index.html", data: "v1"},
		{name: "old.txt", data: "stale"},
	})), "webpkg/p", DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	Remove(storage, "webpkg/p")
	if _, err := Extract(storage, bytes.NewReader(buildZip(t, []zipSpec{
		{name: "index.html", data: "v2"},
	})), "webpkg/p", DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if got := readKey(t, storage, "webpkg/p/index.html"); got != "v2" {
		t.Fatalf("index.html = %q, want v2", got)
	}
	if _, err := storage.Read("webpkg/p/old.txt"); err == nil {
		t.Fatal("stale old.txt still exists after rebuild")
	}
}

// failPutStorage 对命中 key 先真实写入（模拟部分写入落盘）再返回错误。
type failPutStorage struct {
	*upload.LocalStorage
	failKeys map[string]bool
}

func (f *failPutStorage) Put(key string, r io.Reader) error {
	if f.failKeys[key] {
		_ = f.LocalStorage.Put(key, r)
		return errors.New("simulated partial write failure")
	}
	return f.LocalStorage.Put(key, r)
}

// TestExtractFailureDeletesPartialKey：copyEntry 失败时当前条目（可能已部分
// 写入）与此前已写出的条目一并清理，不留半写状态。
func TestExtractFailureDeletesPartialKey(t *testing.T) {
	storage := &failPutStorage{LocalStorage: mustLocal(t), failKeys: map[string]bool{"webpkg/z/assets/app.js": true}}
	data := buildZip(t, []zipSpec{
		{name: "index.html", data: "ok"},
		{name: "assets/app.js", data: "partial"},
	})
	if _, err := Extract(storage, bytes.NewReader(data), "webpkg/z", DefaultLimits()); err == nil {
		t.Fatal("unexpectedly succeeded with failing Put")
	}
	for _, key := range []string{"webpkg/z/index.html", "webpkg/z/assets/app.js", "webpkg/z/" + manifestName} {
		if _, rerr := storage.Read(key); rerr == nil {
			t.Fatalf("key %q still exists after failed extract", key)
		}
	}
}
