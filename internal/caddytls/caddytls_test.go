package caddytls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	s := &Service{repo: repo, adminAddr: adminAddr, certDir: "/data/tls", client: &http.Client{}, now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	return s, repo, fc
}

// newTestServiceWithDir 同 newTestService，但证书目录指向临时目录
// （cert 上传 / ReadCert / Apply(custom) 测试用）。
func newTestServiceWithDir(t *testing.T, adminAddr string) (*Service, *memRepo, *fakeCaddy, string) {
	t.Helper()
	dir := t.TempDir()
	s, repo, fc := newTestService(adminAddr)
	s.certDir = dir
	return s, repo, fc, dir
}

// selfSignedPEM 生成自签证书与私钥的 PEM（测试夹具；不用于生产签发）。
func selfSignedPEM(t *testing.T, cn string, extraDNS []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     append([]string{cn}, extraDNS...),
		NotBefore:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// TestRenderModes 四种模式的站点地址 / tls 指令差异 + 模板锚定（关键路由
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
	// internal 模式带 default_sni（IP 客户端不发 SNI 的握手兜底）。
	if !strings.Contains(internalConf, "default_sni 192.168.1.10\n") {
		t.Fatalf("internal mode missing default_sni:\n%s", firstLines(internalConf, 4))
	}

	customConf := Render(ModeCustom, "docs.example.com")
	// custom 模式 tls 指令指向共享卷证书路径（env 展开含默认值）。
	if !strings.Contains(customConf, "docs.example.com {\n\ttls {$CADDY_TLS_CERT:/data/tls/cert.pem} {$CADDY_TLS_KEY:/data/tls/key.pem}\n") {
		t.Fatalf("custom mode site/tls invalid:\n%s", firstLines(customConf, 3))
	}
	if !strings.Contains(customConf, "default_sni docs.example.com\n") {
		t.Fatalf("custom mode missing default_sni:\n%s", firstLines(customConf, 4))
	}

	for name, conf := range map[string]string{"http": httpConf, "auto": autoConf, "internal": internalConf, "custom": customConf} {
		for _, anchor := range []string{
			"admin {$CADDY_ADMIN:localhost:2019}",
			"handle /api/*", "reverse_proxy backend:8080",
			"handle /content/*", "handle /raw/*", "handle_path /onlyoffice/*", "handle_path /drawio/*",
			"X-Forwarded-Path /onlyoffice",
			"root * /srv/frontend",
			"{$CONTENT_DOMAIN:content.localhost}",
		} {
			if !strings.Contains(conf, anchor) {
				t.Fatalf("%s mode template missing anchor %q", name, anchor)
			}
		}
		// /raw/* 段的安全头锚定（剥 Cookie + sandbox CSP + nosniff + no-referrer）。
		rawIdx := strings.Index(conf, "handle /raw/*")
		contentIdx := strings.Index(conf, "handle /content/*")
		spaIdx := strings.Index(conf, "handle {\n")
		if rawIdx < 0 || rawIdx < contentIdx || rawIdx > spaIdx {
			t.Fatalf("%s mode: /raw/* handle must sit with /content/* and before SPA fallback:\n%s", name, conf)
		}
		rawBlock := conf[rawIdx:spaIdx]
		for _, anchor := range []string{"-Cookie", `Content-Security-Policy "sandbox allow-scripts"`, "X-Content-Type-Options nosniff", "Referrer-Policy no-referrer", "{$CONTENT_UPSTREAM:backend:8080}"} {
			if !strings.Contains(rawBlock, anchor) {
				t.Fatalf("%s mode /raw/* block missing anchor %q", name, anchor)
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
		{string(ModeCustom), "", "domain"},
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

// TestUploadCertAndApplyCustom 证书上传链路：非法 PEM / 私钥不匹配拒绝；
// 合法证书原子落盘（0600）并返回摘要；custom 模式未上传证书时 Apply 拒绝，
// 上传后下发成功且渲染包含证书路径指令。
func TestUploadCertAndApplyCustom(t *testing.T) {
	srv := httptest.NewServer(&fakeCaddy{})
	t.Cleanup(srv.Close)
	fc := srv.Config.Handler.(*fakeCaddy)
	s, repo, _, dir := newTestServiceWithDir(t, strings.TrimPrefix(srv.URL, "http://"))
	rec := &memAudit{}
	s.SetAuditRecorder(rec)

	// 未上传证书：custom 模式 Apply 前置拦截（不触达 caddy）。
	if _, err := s.Apply(context.Background(), ModeCustom, "docs.example.com", ""); err == nil || !strings.Contains(err.Error(), "uploaded certificate") {
		t.Fatalf("apply custom without cert: err=%v", err)
	}

	// 非法 PEM 拒绝。
	if _, err := s.UploadCert([]byte("not a pem"), []byte("not a pem"), ""); err == nil {
		t.Fatal("want error for non-PEM upload")
	}

	// 私钥与证书不匹配拒绝（两张不同证书的密钥）。
	certA, keyA := selfSignedPEM(t, "a.example.com", nil)
	_, keyB := selfSignedPEM(t, "b.example.com", nil)
	if _, err := s.UploadCert(certA, keyB, ""); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched pair: err=%v", err)
	}

	// 合法上传：落盘 0600、返回摘要、写审计。
	info, err := s.UploadCert(certA, keyA, uuid.New().String())
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if info.CN != "a.example.com" || len(info.DNSNames) != 1 || !info.NotAfter.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("info = %+v", info)
	}
	for _, name := range []string{certFileName, keyFileName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		// Windows 文件模式不映射 POSIX 位（0600 落盘仅体现只读位），权限
		// 断言限 Linux（生产与 CI 环境；0600 语义在 writeFileAtomic 生效）。
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s perm = %v, want 0600", name, fi.Mode().Perm())
		}
	}
	if got, err := s.ReadCert(); err != nil || got.CN != "a.example.com" {
		t.Fatalf("read cert: %+v err=%v", got, err)
	}
	found := false
	for _, e := range rec.entries {
		if e.Action == "tls.cert_upload" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit entries = %+v, want tls.cert_upload", rec.entries)
	}

	// 上传后 Apply(custom) 成功：下发渲染包含 tls 指令与状态落库。
	st, err := s.Apply(context.Background(), ModeCustom, "docs.example.com", "")
	if err != nil {
		t.Fatalf("apply custom: %v", err)
	}
	if st.Mode != ModeCustom {
		t.Fatalf("state = %+v", st)
	}
	if len(fc.loads) != 1 || !strings.Contains(fc.loads[0], "\ttls {$CADDY_TLS_CERT:/data/tls/cert.pem} {$CADDY_TLS_KEY:/data/tls/key.pem}\n") {
		t.Fatalf("caddy loads = %v", fc.loads)
	}
	if got, err := repo.GetState(); err != nil || got.Mode != ModeCustom {
		t.Fatalf("persisted = %+v err=%v", got, err)
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
	fc.loads = nil  // 模拟 caddy 重启丢配置
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
