package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSyncFile 在 root 下写文件（rel 用斜杠分隔）。
func writeSyncFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeManifest 写 .docflow-changes.json（runner 收尾产物的平台侧等价物）。
func writeManifest(t *testing.T, root string, entries [][2]string) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"changes":[`)
	for i, e := range entries {
		if i > 0 {
			b.WriteByte(',')
		}
		payload, _ := json.Marshal(map[string]string{"path": e[0], "status": e[1]})
		b.Write(payload)
	}
	b.WriteString(`]}`)
	if err := os.WriteFile(filepath.Join(root, ChangesManifestFile), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// byPath 把 diff 切成按路径索引。
func byPath(diff []WorkspaceDiff) map[string]WorkspaceDiff {
	out := make(map[string]WorkspaceDiff, len(diff))
	for _, d := range diff {
		out[d.Path] = d
	}
	return out
}

// TestGitSyncManifestDiff git 模式：合法清单（A/M/D）按其构造 diff，
// 存在文件计算 sha256+size、排序确定。
func TestGitSyncManifestDiff(t *testing.T) {
	root := t.TempDir()
	writeSyncFile(t, root, "src/app.js", "console.log(1)")
	writeSyncFile(t, root, "README.md", "updated")
	writeSyncFile(t, root, "unchanged.txt", "same")
	writeManifest(t, root, [][2]string{
		{"README.md", "M"},
		{"gone.txt", "D"},
		{"src/app.js", "A"},
	})
	diff, err := (Executor{SyncMode: "git"}).workspaceDiff(root, map[string]workspaceEntry{})
	if err != nil {
		t.Fatal(err)
	}
	m := byPath(diff)
	if len(m) != 3 {
		t.Fatalf("diff = %+v", diff)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("console.log(1)")))
	if d := m["src/app.js"]; d.Action != "added" || d.Size != 14 || d.SHA256 != sum {
		t.Fatalf("added entry: %+v", d)
	}
	if d := m["README.md"]; d.Action != "modified" || d.Size != 7 || d.SHA256 == "" {
		t.Fatalf("modified entry: %+v", d)
	}
	if d := m["gone.txt"]; d.Action != "deleted" || d.Size != 0 || d.SHA256 != "" {
		t.Fatalf("deleted entry: %+v", d)
	}
	// 排序确定（apply 的 diff hash 依赖）。
	if diff[0].Path != "README.md" || diff[1].Path != "gone.txt" || diff[2].Path != "src/app.js" {
		t.Fatalf("diff order = %+v", diff)
	}
}

// TestGitSyncManifestInvalidFallsBackToScan 清单非法（路径走私/声称删除但
// 文件仍在/JSON 损坏/状态码未知/条目超限/A 文件缺失）一律整体拒绝，
// 回退 scanWorkspace 全量对比。
func TestGitSyncManifestInvalidFallsBackToScan(t *testing.T) {
	root := t.TempDir()
	writeSyncFile(t, root, "index.html", "<h1>ok</h1>")
	writeSyncFile(t, root, "still-here.txt", "x")
	cases := []struct {
		name     string
		manifest string
	}{
		{"path-traversal", `{"changes":[{"path":"../escape","status":"A"}]}`},
		{"deleted-but-exists", `{"changes":[{"path":"still-here.txt","status":"D"}]}`},
		{"corrupt-json", `{"changes":`},
		{"unknown-status", `{"changes":[{"path":"index.html","status":"X"}]}`},
		{"added-missing", `{"changes":[{"path":"no-such.txt","status":"A"}]}`},
		{"empty", `{"changes":[]}`},
	}
	for _, tc := range cases {
		if err := os.WriteFile(filepath.Join(root, ChangesManifestFile), []byte(tc.manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		diff, err := (Executor{SyncMode: "git"}).workspaceDiff(root, map[string]workspaceEntry{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		m := byPath(diff)
		if _, ok := m["index.html"]; !ok || m["index.html"].Action != "added" {
			t.Fatalf("%s: should fall back to scan, diff = %+v", tc.name, diff)
		}
		// 清单文件自身不入 scan diff。
		if _, ok := m[ChangesManifestFile]; ok {
			t.Fatalf("%s: manifest file leaked into scan diff", tc.name)
		}
	}
	// 条目超限（>500）同样拒绝。
	var entries strings.Builder
	entries.WriteString(`{"changes":[`)
	for i := 0; i < manifestMaxEntries+1; i++ {
		if i > 0 {
			entries.WriteByte(',')
		}
		fmt.Fprintf(&entries, `{"path":"f%d.txt","status":"D"}`, i)
	}
	entries.WriteString(`]}`)
	if err := os.WriteFile(filepath.Join(root, ChangesManifestFile), []byte(entries.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	diff, err := (Executor{SyncMode: "git"}).workspaceDiff(root, map[string]workspaceEntry{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byPath(diff)["index.html"]; !ok {
		t.Fatal("oversize manifest should fall back to scan")
	}
}

// TestGitSyncMissingManifestFallsBackToScan 无清单文件（runner 未产出）
// 回退 scanWorkspace。
func TestGitSyncMissingManifestFallsBackToScan(t *testing.T) {
	root := t.TempDir()
	writeSyncFile(t, root, "a.txt", "A")
	baseline := map[string]workspaceEntry{"a.txt": {Size: 1, SHA256: "old"}}
	diff, err := (Executor{SyncMode: "git"}).workspaceDiff(root, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if d := byPath(diff)["a.txt"]; d.Action != "modified" {
		t.Fatalf("scan fallback should compute modified against baseline: %+v", diff)
	}
}

// TestGitSyncScanModeAlwaysScans sync_mode=scan 恒走 scanWorkspace：
// 即使存在合法清单也不消费。
func TestGitSyncScanModeAlwaysScans(t *testing.T) {
	root := t.TempDir()
	writeSyncFile(t, root, "a.txt", "A")
	writeSyncFile(t, root, "b.txt", "B")
	// 清单只声明 a.txt 的修改；scan 模式按基线对比应同时看到 a 修改与
	// b 新增。
	writeManifest(t, root, [][2]string{{"a.txt", "M"}})
	diff, err := (Executor{SyncMode: "scan"}).workspaceDiff(root, map[string]workspaceEntry{"a.txt": {Size: 1, SHA256: "old"}})
	if err != nil {
		t.Fatal(err)
	}
	m := byPath(diff)
	if m["a.txt"].Action != "modified" || m["b.txt"].Action != "added" {
		t.Fatalf("scan mode should ignore manifest, diff = %+v", diff)
	}
}
