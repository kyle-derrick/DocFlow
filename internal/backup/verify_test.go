package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sha256Of 计算内容哈希（构造真实 manifest 用）。
func sha256Of(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// writeManifest 写入 v2 格式 manifest.json（entries 顺序保持传入顺序）。
func writeManifest(t *testing.T, backupDir string, m Manifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeBackup 构造一个 v2 备份目录：文件内容真实写入，manifest 哈希/大小
// 真实计算；postgres=true 时包含 postgres_dump 组件。
func writeBackup(t *testing.T, dir, name string, files map[string]string, postgres bool) string {
	t.Helper()
	backupDir := filepath.Join(dir, name)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 2, Timestamp: "2026-09-15T00:00:00Z", ObjectStore: "local", Encryption: "ops-layer", Components: []string{"postgres"}}
	// 固定顺序：postgres.sql → objects.tar → env.sanitized。
	ordered := []string{"postgres.sql", "objects.tar", "env.sanitized"}
	for _, path := range ordered {
		if path == "postgres.sql" && !postgres {
			continue
		}
		content, ok := files[path]
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(backupDir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		typ := "object"
		if path == "postgres.sql" {
			typ = "postgres_dump"
		} else if path == "env.sanitized" {
			typ = "env"
		}
		m.Entries = append(m.Entries, Entry{Path: path, Type: typ, SHA256: sha256Of(content), Size: int64(len(content))})
	}
	if !postgres {
		m.Components = []string{"object-store"}
	}
	writeManifest(t, backupDir, m)
	return backupDir
}

// TestManifestParse 校验 v2 清单解析：timestamp/components/object_store 与
// 每文件 sha256/size 均可结构化读取。
func TestManifestParse(t *testing.T) {
	raw := `{"version":2,"timestamp":"2026-09-15T01:02:03Z","components":["postgres","object-store","env"],` +
		`"object_store":"external","encryption":"ops-layer",` +
		`"entries":[{"path":"postgres.sql","type":"postgres_dump","sha256":"abc","size":10},` +
		`{"path":"env.sanitized","type":"env","sha256":"def","size":3}]}`
	var m Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.Version != 2 || m.Timestamp != "2026-09-15T01:02:03Z" || m.ObjectStore != "external" || m.Encryption != "ops-layer" {
		t.Fatalf("manifest header mismatch: %+v", m)
	}
	if len(m.Components) != 3 || m.Components[1] != "object-store" {
		t.Fatalf("components = %v", m.Components)
	}
	if len(m.Entries) != 2 || m.Entries[0].Path != "postgres.sql" || m.Entries[0].SHA256 != "abc" || m.Entries[0].Size != 10 || m.Entries[1].Type != "env" {
		t.Fatalf("entries mismatch: %+v", m.Entries)
	}
}

func TestVerifyOK(t *testing.T) {
	dir := t.TempDir()
	writeBackup(t, dir, "docflow-backup-20260915T000000Z", map[string]string{
		"postgres.sql":  "-- dump\n",
		"objects.tar":   "tar-bytes",
		"env.sanitized": "PORT=8080\n",
	}, true)
	r, err := Verify(dir)
	if err != nil {
		t.Fatalf("Verify: %v (errors: %v)", err, r.Errors)
	}
	if !r.Verified || r.Files != 3 {
		t.Fatalf("result = %+v, want verified 3 files", r)
	}
	if r.Timestamp != "2026-09-15T00:00:00Z" || !strings.HasSuffix(filepath.ToSlash(r.Backup), "docflow-backup-20260915T000000Z") {
		t.Fatalf("result header mismatch: %+v", r)
	}
}

func TestVerifySha256Mismatch(t *testing.T) {
	dir := t.TempDir()
	backupDir := writeBackup(t, dir, "docflow-backup-20260915T000000Z", map[string]string{
		"postgres.sql":  "-- dump\n",
		"env.sanitized": "PORT=8080\n",
	}, true)
	// 备份完成后篡改文件内容。
	if err := os.WriteFile(filepath.Join(backupDir, "postgres.sql"), []byte("-- tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Verify(dir)
	if err == nil {
		t.Fatal("Verify must fail on sha256 mismatch")
	}
	if r.Verified || r.Files != 2 {
		t.Fatalf("result = %+v", r)
	}
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "postgres.sql: sha256 mismatch") {
		t.Fatalf("errors = %v", r.Errors)
	}
}

func TestVerifySizeMismatch(t *testing.T) {
	dir := t.TempDir()
	backupDir := writeBackup(t, dir, "docflow-backup-20260915T000000Z", map[string]string{
		"postgres.sql": "-- dump\n",
	}, true)
	// 与真实大小不符（sha256 正确、size 错误）。
	raw, err := os.ReadFile(filepath.Join(backupDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Entries[0].Size = 999999
	writeManifest(t, backupDir, m)
	r, err := Verify(dir)
	if err == nil {
		t.Fatal("Verify must fail on size mismatch")
	}
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "size mismatch") {
		t.Fatalf("errors = %v", r.Errors)
	}
}

func TestVerifyMissingPostgres(t *testing.T) {
	dir := t.TempDir()
	writeBackup(t, dir, "docflow-backup-20260915T000000Z", map[string]string{
		"postgres.sql":  "-- dump\n",
		"objects.tar":   "tar-bytes",
		"env.sanitized": "PORT=8080\n",
	}, false) // 移除 postgres.sql 并去掉组件。
	r, err := Verify(dir)
	if err == nil {
		t.Fatal("Verify must fail when postgres dump is missing")
	}
	found := false
	for _, e := range r.Errors {
		if strings.Contains(e, "postgres dump is missing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors = %v, want postgres dump missing", r.Errors)
	}
}

func TestVerifyNoBackup(t *testing.T) {
	dir := t.TempDir()
	if _, err := Verify(dir); err != ErrNoBackup {
		t.Fatalf("err = %v, want ErrNoBackup", err)
	}
	// 目录不存在同样视为无备份。
	if _, err := Verify(filepath.Join(dir, "does-not-exist")); err != ErrNoBackup {
		t.Fatalf("err = %v, want ErrNoBackup", err)
	}
}

// TestLatestPicksMostRecent 校验候选发现按 manifest 修改时间取最近，
// 并兼容 v1 顶层散落清单（docflow-*.manifest.json）。
func TestLatestPicksMostRecent(t *testing.T) {
	dir := t.TempDir()
	old := writeBackup(t, dir, "docflow-backup-20260101T000000Z", map[string]string{"postgres.sql": "-- old\n"}, true)
	newer := writeBackup(t, dir, "docflow-backup-20260915T000000Z", map[string]string{"postgres.sql": "-- new\n"}, true)
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(old, "manifest.json"), past, past); err != nil {
		t.Fatal(err)
	}
	manifestPath, backupDir, err := Latest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if backupDir != newer || filepath.Base(manifestPath) != "manifest.json" {
		t.Fatalf("latest = %s / %s, want %s", manifestPath, backupDir, newer)
	}
	// v1 兼容：顶层散落清单按 mtime 参与竞选。
	v1 := filepath.Join(dir, "docflow-20261001T000000Z.manifest.json")
	if err := os.WriteFile(v1, []byte(`{"version":1,"entries":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath, backupDir, err = Latest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if manifestPath != v1 || backupDir != dir {
		t.Fatalf("latest v1 = %s / %s, want %s", manifestPath, backupDir, v1)
	}
}

func TestVerifyMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadVerifyMarker(dir); ok {
		t.Fatal("no marker yet, ok must be false")
	}
	marker := VerifyMarker{Verified: true, VerifiedAt: "2026-09-15T00:00:00Z", Manifest: "m.json", Files: 3}
	if err := WriteVerifyMarker(dir, marker); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadVerifyMarker(dir)
	if !ok || !got.Verified || got.VerifiedAt != marker.VerifiedAt || got.Files != 3 {
		t.Fatalf("marker = %+v, ok=%v", got, ok)
	}
	// 不可解析内容按无标记处理。
	if err := os.WriteFile(filepath.Join(dir, markerName), []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadVerifyMarker(dir); ok {
		t.Fatal("corrupt marker must read as absent")
	}
}
