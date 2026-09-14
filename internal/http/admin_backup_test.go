package http

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/backup"
	"github.com/docflow/docflow/internal/metrics"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// backupMetricValue 从默认 registry 读取指标当前值（按标签过滤；指标为全局
// 注册，断言一律使用调用前后差值）。标签容器类型来自 client_model，但此处
// 只调用其方法、不显式命名类型，无需直接依赖该 indirect 包。
func backupMetricValue(t *testing.T, name string, labelMatch map[string]string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	outer:
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if want, ok := labelMatch[lp.GetName()]; ok && want != lp.GetValue() {
					continue outer
				}
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
		}
	}
	return 0
}

// writeTestBackup 在 dir 下构造 v2 备份目录（manifest 哈希/大小真实计算）。
func writeTestBackup(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	backupDir := filepath.Join(dir, name)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := backup.Manifest{Version: 2, Timestamp: "2026-09-15T00:00:00Z", ObjectStore: "local", Encryption: "ops-layer", Components: []string{"postgres", "env"}}
	for _, path := range []string{"postgres.sql", "objects.tar", "env.sanitized"} {
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
		sum := sha256.Sum256([]byte(content))
		m.Entries = append(m.Entries, backup.Entry{Path: path, Type: typ, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))})
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return backupDir
}

func TestAdminBackupStatusNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("BACKUP_DIR", "")
	h := &Handler{}
	c, w := adminContext(http.MethodGet, "/api/v1/admin/backups/status", "")
	h.adminBackupStatus(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"enabled":false`, `"last_backup":null`, `"verified":null`, `"files":[]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s must contain %s", body, want)
		}
	}
}

// 目录缺失（或从未备份）：enabled=true、last_backup=null、files=[]，不报错。
func TestAdminBackupStatusDirMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	h.SetBackupDir(filepath.Join(t.TempDir(), "does-not-exist"))
	c, w := adminContext(http.MethodGet, "/api/v1/admin/backups/status", "")
	h.adminBackupStatus(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"enabled":true`, `"last_backup":null`, `"files":[]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s must contain %s", body, want)
		}
	}
}

func TestAdminBackupStatusWithBackup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	writeTestBackup(t, dir, "docflow-backup-20260915T000000Z", map[string]string{
		"postgres.sql":  "-- dump\n",
		"env.sanitized": "PORT=8080\n",
	})
	h := &Handler{}
	h.SetBackupDir(dir)
	c, w := adminContext(http.MethodGet, "/api/v1/admin/backups/status", "")
	h.adminBackupStatus(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`"enabled":true`,
		`"name":"docflow-backup-20260915T000000Z"`,
		`"timestamp":"2026-09-15T00:00:00Z"`,
		`"components":["postgres","env"]`,
		`"object_store":"local"`,
		`"encryption":"ops-layer"`,
		`"path":"postgres.sql"`,
		`"type":"postgres_dump"`,
		`"verified":null`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s must contain %s", body, want)
		}
	}
	// 写入校验标记后：verified=true 且携带校验时间。
	if err := backup.WriteVerifyMarker(filepath.Join(dir, "docflow-backup-20260915T000000Z"), backup.VerifyMarker{Verified: true, VerifiedAt: "2026-09-15T01:00:00Z", Files: 2}); err != nil {
		t.Fatal(err)
	}
	c, w = adminContext(http.MethodGet, "/api/v1/admin/backups/status", "")
	h.adminBackupStatus(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body = w.Body.String()
	if !strings.Contains(body, `"verified":true`) || !strings.Contains(body, `"verified_at":"2026-09-15T01:00:00Z"`) {
		t.Fatalf("body must reflect verify marker: %s", body)
	}
}

func newBackupVerifyHandler(t *testing.T) (*Handler, *memAuditRecorder, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("BACKUP_DIR", "")
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	rec := &memAuditRecorder{}
	h.SetAuditRecorder(rec)
	return h, rec, uuid.New()
}

func callBackupVerify(h *Handler, actor uuid.UUID) *httptest.ResponseRecorder {
	c, w := adminContext(http.MethodPost, "/api/v1/admin/backups/verify", "")
	c.Set("user_id", actor)
	h.adminBackupVerify(c)
	return w
}

func TestAdminBackupVerifyNotConfigured(t *testing.T) {
	h, rec, actor := newBackupVerifyHandler(t)
	w := callBackupVerify(h, actor)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	if len(rec.entries) != 0 {
		t.Fatalf("no audit expected, got %d entries", len(rec.entries))
	}
}

func TestAdminBackupVerifyNoBackup(t *testing.T) {
	h, rec, actor := newBackupVerifyHandler(t)
	h.SetBackupDir(t.TempDir())
	w := callBackupVerify(h, actor)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
	if len(rec.entries) != 0 {
		t.Fatalf("no audit expected, got %d entries", len(rec.entries))
	}
}

func TestAdminBackupVerifyOK(t *testing.T) {
	h, rec, actor := newBackupVerifyHandler(t)
	dir := t.TempDir()
	backupDir := writeTestBackup(t, dir, "docflow-backup-20260915T000000Z", map[string]string{
		"postgres.sql":  "-- dump\n",
		"env.sanitized": "PORT=8080\n",
	})
	h.SetBackupDir(dir)
	successBefore := backupMetricValue(t, "docflow_backup_verification_total", map[string]string{"result": metrics.BackupResultSuccess})
	w := callBackupVerify(h, actor)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"verified":true`, `"files":2`, `"timestamp":"2026-09-15T00:00:00Z"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s must contain %s", body, want)
		}
	}
	// 指标：success 计数 +1；最近成功时间戳 > 0。
	if after := backupMetricValue(t, "docflow_backup_verification_total", map[string]string{"result": metrics.BackupResultSuccess}); after != successBefore+1 {
		t.Fatalf("docflow_backup_verification_total{success} = %v, want %v", after, successBefore+1)
	}
	if ts := backupMetricValue(t, "docflow_backup_last_success_timestamp", nil); ts <= 0 {
		t.Fatalf("docflow_backup_last_success_timestamp = %v, want > 0", ts)
	}
	// 审计：backup.verify success，ResourceID 为备份目录名。
	entry := rec.find(audit.ActionBackupVerify)
	if entry == nil {
		t.Fatalf("audit backup.verify not recorded: %+v", rec.entries)
	}
	if entry.ResourceType != audit.ResourceBackup || entry.ResourceID != "docflow-backup-20260915T000000Z" || entry.Status != audit.StatusSuccess {
		t.Fatalf("audit entry = %+v", entry)
	}
	if !strings.Contains(entry.Metadata, `"files":2`) {
		t.Fatalf("audit metadata = %s", entry.Metadata)
	}
	// 校验标记写回后 status 端点展示 verified=true（管理闭环）。
	marker, ok := backup.ReadVerifyMarker(backupDir)
	if !ok || !marker.Verified || marker.Files != 2 {
		t.Fatalf("marker = %+v, ok=%v", marker, ok)
	}
	c, w2 := adminContext(http.MethodGet, "/api/v1/admin/backups/status", "")
	h.adminBackupStatus(c)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `"verified":true`) {
		t.Fatalf("status after verify = %d %s", w2.Code, w2.Body.String())
	}
}

func TestAdminBackupVerifyCorrupt(t *testing.T) {
	h, rec, actor := newBackupVerifyHandler(t)
	dir := t.TempDir()
	backupDir := writeTestBackup(t, dir, "docflow-backup-20260915T000000Z", map[string]string{
		"postgres.sql": "-- dump\n",
	})
	h.SetBackupDir(dir)
	failedBefore := backupMetricValue(t, "docflow_backup_verification_total", map[string]string{"result": metrics.BackupResultFailed})
	if err := os.WriteFile(filepath.Join(backupDir, "postgres.sql"), []byte("-- tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := callBackupVerify(h, actor)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"verified":false`) || !strings.Contains(body, "sha256 mismatch") {
		t.Fatalf("body must carry failure detail: %s", body)
	}
	if after := backupMetricValue(t, "docflow_backup_verification_total", map[string]string{"result": metrics.BackupResultFailed}); after != failedBefore+1 {
		t.Fatalf("docflow_backup_verification_total{failed} = %v, want %v", after, failedBefore+1)
	}
	entry := rec.find(audit.ActionBackupVerify)
	if entry == nil || entry.Status != audit.StatusFailure {
		t.Fatalf("audit entry = %+v, want failure", entry)
	}
	// 失败标记同样写回：status 展示 verified=false。
	marker, ok := backup.ReadVerifyMarker(backupDir)
	if !ok || marker.Verified {
		t.Fatalf("marker = %+v, ok=%v, want verified=false", marker, ok)
	}
	c, w2 := adminContext(http.MethodGet, "/api/v1/admin/backups/status", "")
	h.adminBackupStatus(c)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `"verified":false`) {
		t.Fatalf("status after failed verify = %d %s", w2.Code, w2.Body.String())
	}
}
