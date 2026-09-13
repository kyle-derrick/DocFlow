package webpkg

import (
	"errors"
	"strings"
	"testing"
)

// ---------- 路径清理矩阵 ----------

func TestSanitizePathValidMatrix(t *testing.T) {
	tests := []struct {
		input  string
		want   string
		isDir  bool
		maxDep int
	}{
		{"index.html", "index.html", false, 10},
		{"assets/app.js", "assets/app.js", false, 10},
		{"a/b/c/d/e/f/g/h/i/j/k/file.txt", "a/b/c/d/e/f/g/h/i/j/k/file.txt", false, 11},
		{"dir/", "dir", true, 10},
		{"a/b/", "a/b", true, 10},
		{"/", "", true, 10}, // 根目录条目
		{"a/b/c/d.txt", "a/b/c/d.txt", false, 3},
		{"中文/页面.html", "中文/页面.html", false, 10},
	}
	for _, test := range tests {
		got, isDir, err := SanitizePath(test.input, test.maxDep)
		if err != nil {
			t.Errorf("SanitizePath(%q) unexpected error: %v", test.input, err)
			continue
		}
		if got != test.want || isDir != test.isDir {
			t.Errorf("SanitizePath(%q) = %q, isDir=%v; want %q, isDir=%v", test.input, got, isDir, test.want, test.isDir)
		}
	}
}

func TestSanitizePathInvalidMatrix(t *testing.T) {
	invalid := []string{
		"",                                    // 空路径
		"../evil.txt",                         // Zip Slip：前导 ..
		"a/../../evil.txt",                    // Zip Slip：中段 ..
		"../",                                 // 目录形式 Zip Slip
		"/etc/passwd",                         // 绝对路径
		"/abs/index.html",                     // 前导 /
		"C:\\Windows\\evil.txt",               // 盘符 + 反斜杠
		"C:/evil.txt",                         // 盘符
		"c:/evil.txt",                         // 小写盘符
		"a\\b.txt",                            // 反斜杠
		"dir//file.txt",                       // 空路径段
		"a/./b.txt",                           // . 段
		"./index.html",                        // 前导 .
		"na\x00me.txt",                        // NUL 控制字符
		"bad\nname.txt",                       // 换行控制字符
		"tab\tname.txt",                       // TAB 控制字符
		"del\x7f.txt",                         // DEL
		"CON.txt",                             // Windows 保留设备名
		"a/COM1.txt",                          // 子段保留设备名
		"trailing. ",                          // 段以空格结尾
		"dot./x.txt",                          // 段以点结尾
		"a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p.txt", // 深度 15 超限（maxDepth=10）
	}
	for _, name := range invalid {
		_, _, err := SanitizePath(name, 10)
		if err == nil {
			t.Errorf("SanitizePath(%q) unexpectedly accepted", name)
			continue
		}
		if !errors.Is(err, ErrWebpkgInvalid) {
			t.Errorf("SanitizePath(%q) error = %v, want ErrWebpkgInvalid wrap", name, err)
		}
	}
	// 深度边界：恰好等于 maxDepth 合法，超出一级拒绝。
	if _, _, err := SanitizePath("a/b/c/d/e/f/g/h/i/j/x.txt", 10); err != nil {
		t.Errorf("depth == maxDepth rejected: %v", err)
	}
	if _, _, err := SanitizePath("a/b/c/d/e/f/g/h/i/j/k/x.txt", 10); err == nil {
		t.Error("depth == maxDepth+1 unexpectedly accepted")
	}
}

// ---------- Resolve 白名单与穿越 ----------

func TestResolvePathWhitelist(t *testing.T) {
	tests := []struct {
		rel  string
		want string
	}{
		{"index.html", "text/html; charset=utf-8"},
		{"page.htm", "text/html; charset=utf-8"},
		{"style.CSS", "text/css; charset=utf-8"}, // 扩展名大小写不敏感
		{"assets/app.js", "text/javascript; charset=utf-8"},
		{"data.json", "application/json; charset=utf-8"},
		{"img/pic.jpeg", "image/jpeg"},
		{"img/pic.JPG", "image/jpeg"},
		{"logo.svg", "image/svg+xml"},
		{"favicon.ico", "image/x-icon"},
		{"f.woff2", "font/woff2"},
		{"f.ttf", "font/ttf"},
		{"app.js.map", "application/json; charset=utf-8"},
		{"notes.txt", "text/plain; charset=utf-8"},
	}
	for _, test := range tests {
		path, ct, ok := ResolvePath(test.rel, 10)
		if !ok {
			t.Errorf("ResolvePath(%q) unexpectedly rejected", test.rel)
			continue
		}
		if ct != test.want {
			t.Errorf("ResolvePath(%q) content-type = %q, want %q", test.rel, ct, test.want)
		}
		if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
			t.Errorf("ResolvePath(%q) path = %q not normalized", test.rel, path)
		}
	}
}

func TestResolvePathRejected(t *testing.T) {
	rejected := []string{
		"../secret.txt",               // 穿越
		"a/../../secret.txt",          // 多级穿越
		"/etc/passwd",                 // 绝对路径
		"C:/x.txt",                    // 盘符
		"app.exe",                     // 非白名单扩展名
		"script.php",                  // 非白名单扩展名
		"archive.zip",                 // 非白名单扩展名（不允许嵌套 zip）
		"noext",                       // 无扩展名
		"dir/",                        // 目录路径
		".webpkg-manifest",            // 内部清单不可提供
		"a/b/c/d/e/f/g/h/i/j/k/x.txt", // 深度超限
		"bad\x00name.html",            // 控制字符
	}
	for _, rel := range rejected {
		if _, _, ok := ResolvePath(rel, 10); ok {
			t.Errorf("ResolvePath(%q) unexpectedly allowed", rel)
		}
	}
	// 前导斜杠（gin 通配参数形如 /index.html）应归一处理。
	if path, _, ok := ResolvePath("/index.html", 10); !ok || path != "index.html" {
		t.Errorf("ResolvePath(/index.html) = %q, %v; want index.html, true", path, ok)
	}
}

func TestURLSafePublicID(t *testing.T) {
	pid, err := NewPublicID()
	if err != nil {
		t.Fatal(err)
	}
	if len(pid) != 43 || !URLSafePublicID(pid) {
		t.Fatalf("NewPublicID() = %q not URL-safe 43 chars", pid)
	}
	for _, bad := range []string{"", "short", pid + "x", "../../etc/passwd-path-aaaaaaaaaaaaaaaaaaaaaaaa", "abc+/abcabcabcabcabcabcabcabcabcabcabcabcabcabc"} {
		if URLSafePublicID(bad) {
			t.Errorf("URLSafePublicID(%q) unexpectedly true", bad)
		}
	}
}

func TestZipCandidate(t *testing.T) {
	cases := []struct {
		mime, name string
		want       bool
	}{
		{"application/zip", "x", true},
		{"application/zip; charset=binary", "x", true},
		{"APPLICATION/ZIP", "x", true},
		{"application/octet-stream", "site.ZIP", true},
		{"", "site.zip", true},
		{"application/octet-stream", "site.zip", true},
		{"application/x-tar", "site.tar", false},
		{"image/png", "a.png", false},
		{"application/zip.txt", "a", false}, // 前缀不整段匹配
	}
	for _, c := range cases {
		if got := ZipCandidate(c.mime, c.name); got != c.want {
			t.Errorf("ZipCandidate(%q, %q) = %v, want %v", c.mime, c.name, got, c.want)
		}
	}
}
