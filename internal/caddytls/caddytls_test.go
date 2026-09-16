package caddytls

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// memRepo 为 repository 的内存实现（模式参照 settings 包测试）。
type memRepo struct {
	mu   sync.Mutex
	rows map[uint]State
}

func (m *memRepo) GetState() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rows[1]; ok {
		return s, nil
	}
	return State{}, gorm.ErrRecordNotFound
}

func (m *memRepo) UpsertState(s State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.ID = 1
	s.UpdatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.rows[1] = s
	return nil
}

// fakeCaddy 记录 POST /load 的请求，可注入失败。
type fakeCaddy struct {
	mu       sync.Mutex
	loads    []string
	decline  bool
	declines int // 前 N 次 /load 返回 400（测试重试/失败路径）
}

func (f *fakeCaddy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/load" || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "text/caddyfile" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"content-type"}`))
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decline || f.declines > 0 {
		if f.declines > 0 {
			f.declines--
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid config"}`))
		return
	}
	f.loads = append(f.loads, string(body))
	w.WriteHeader(http.StatusOK)
}

type memAudit struct{ entries []audit.Entry }

func (m *memAudit) Record(e audit.Entry) error { m.entries = append(m.entries, e); return nil }

func newTestService(adminAddr string) (*Service, *memRepo, *fakeCaddy) {
	repo := &memRepo{rows: make(map[uint]State)}
	fc := &fakeCaddy{}
	s := &Service{repo: repo, adminAddr: adminAddr, client: &http.Client{}, now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	return s, repo, fc
}

// TestRenderModes 三种模式的站点地址 / tls 指令差异 + 模板锚定（关键路由
// 块存在，防止 deploy/Caddyfile 演进时模板漂移失忆）。
func TestRenderModes(t *testing.T) {
	httpConf := Render(ModeHTTP, "")
	if !strings.Contains(httpConf, "{$APP_DOMAIN::80} {\n") {
		t.Fatalf("http mode site = %q", firstLine(httpConf))
	}
	if strings.Contains(httpConf, "tls internal") {
		t.Fatal("http mode must not contain tls directive")
	}

	autoConf := Render(ModeAuto, "docflow.example.com")
	if !strings.Contains(autoConf, "docflow.example.com {\n") || strings.Contains(autoConf, "tls internal") {
		t.Fatalf("auto mode site invalid:\n%s", firstLines(autoConf, 3))
	}

	internalConf := Render(ModeInternal, "192.168.1.10")
	if !strings.Contains(internalConf, "192.168.1.10 {\n\ttls internal\n") {
		t.Fatalf("internal mode site/tls invalid:\n%s", firstLines(internalConf, 3))
	}

	for name, conf := range map[string]string{"http": httpConf, "auto": autoConf, "internal": internalConf} {
		for _, anchor := range []string{
			"{\n\tadmin {$CADDY_ADMIN:localhost:2019}\n}",
			"handle /api/*", "reverse_proxy backend:8080",
			"handle /content/*", "handle_path /onlyoffice/*", "handle_path /drawio/*",
			"X-Forwarded-Path /onlyoffice",
			"root * /srv/frontend",
			"{$CONTENT_DOMAIN:content.localhost}",
		} {
			if !strings.Contains(conf, anchor) {
				t.Fatalf("%s mode template missing anchor %q", name, anchor)
			}
		}
	}
}

func firstLine(s string) string { return firstLines(s, 1) }

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	return strings.Join(lines[:n], "\n")
}

// TestApplySuccessAndPersist 下发成功：请求头 Caddyfile 类型、内容为渲染
// 结果、状态落库、审计记录。
func TestApplySuccessAndPersist(t *testing.T) {
	srv := httptest.NewServer(&fakeCaddy{})
	t.Cleanup(srv.Close)
	fc := srv.Config.Handler.(*fakeCaddy)
	s, repo, _ := newTestService(strings.TrimPrefix(srv.URL, "http://"))
	rec := &memAudit{}
	s.SetAuditRecorder(rec)

	actor := uuid.New()
	st, err := s.Apply(context.Background(), ModeInternal, "192.168.1.10", actor.String())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if st.Mode != ModeInternal || st.Domain != "192.168.1.10" || st.UpdatedBy == nil || st.UpdatedBy.String() != actor.String() {
		t.Fatalf("state = %+v", st)
	}
	if len(fc.loads) != 1 || !strings.Contains(fc.loads[0], "192.168.1.10 {\n\ttls internal\n") {
		t.Fatalf("caddy loads = %v", fc.loads)
	}
	got, err := repo.GetState()
	if err != nil || got.Mode != ModeInternal {
		t.Fatalf("persisted = %+v err=%v", got, err)
	}
	if len(rec.entries) != 1 || rec.entries[0].Action != "tls.update" {
		t.Fatalf("audit = %+v", rec.entries)
	}
}

// TestApplyRejectCaddyError caddy 拒绝配置时不落库（原子性：旧配置保持）。
func TestApplyRejectCaddyError(t *testing.T) {
	fc := &fakeCaddy{decline: true}
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	s, repo, _ := newTestService(strings.TrimPrefix(srv.URL, "http://"))

	if _, err := s.Apply(context.Background(), ModeAuto, "a.example.com", ""); err == nil {
		t.Fatal("want error when caddy declines")
	}
	if _, err := repo.GetState(); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("state must stay empty, got err=%v", err)
	}
}

// TestApplyValidation 模式与域名校验。
func TestApplyValidation(t *testing.T) {
	srv := httptest.NewServer(&fakeCaddy{})
	t.Cleanup(srv.Close)
	s, _, _ := newTestService(strings.TrimPrefix(srv.URL, "http://"))

	cases := []struct {
		mode, domain, want string
	}{
		{"bogus", "", "mode"},
		{string(ModeAuto), "", "domain"},
		{string(ModeAuto), "http://x.com", "domain"},
		{string(ModeAuto), "192.168.1.1", "IP"},
		{string(ModeInternal), "bad domain", "domain"},
	}
	for _, c := range cases {
		_, err := s.Apply(context.Background(), Mode(c.mode), c.domain, "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("mode=%s domain=%q: err=%v, want contains %q", c.mode, c.domain, err, c.want)
		}
	}
	// internal 允许 IP；http 模式忽略 domain。
	if _, err := s.Apply(context.Background(), ModeInternal, "10.0.0.5", ""); err != nil {
		t.Fatalf("internal with IP: %v", err)
	}
	if _, err := s.Apply(context.Background(), ModeHTTP, "", ""); err != nil {
		t.Fatalf("http reset: %v", err)
	}
}

// TestNotManaged 未配置 admin 地址时 Apply 报 ErrNotManaged。
func TestNotManaged(t *testing.T) {
	s, _, _ := newTestService("")
	if _, err := s.Apply(context.Background(), ModeInternal, "x.com", ""); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("err = %v, want ErrNotManaged", err)
	}
}

// TestReapplyStartup 非 http 偏好：caddy 前几次拒绝后成功重试。
func TestReapplyStartup(t *testing.T) {
	fc := &fakeCaddy{}
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	s, repo, _ := newTestService(strings.TrimPrefix(srv.URL, "http://"))
	s.retry = 10 * time.Millisecond
	if _, err := s.Apply(context.Background(), ModeInternal, "127.0.0.1", ""); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	fc.mu.Lock()
	fc.loads = nil // 模拟 caddy 重启丢配置
	fc.declines = 2 // 重启后前两次 /load 失败（如 caddy 尚未就绪）
	fc.mu.Unlock()

	s.ReapplyStartup(context.Background())
	fc.mu.Lock()
	defer fc.mu.Unlock()
	// declines 两次被消耗（失败不记录 loads），第三次成功下发 1 次。
	if fc.declines != 0 || len(fc.loads) != 1 || !strings.Contains(fc.loads[0], "127.0.0.1 {\n\ttls internal\n") {
		t.Fatalf("declines=%d loads=%d", fc.declines, len(fc.loads))
	}
	if _, err := repo.GetState(); err != nil {
		t.Fatalf("state: %v", err)
	}
}
