// Package http —— ONLYOFFICE config 端点的 public URL 动态推导测试：
// 未配置 / 显式 127.0.0.1（本机回环）→ 按请求 Host 推导
// "{scheme}://{host}/onlyoffice"；显式外网域名 → 原样返回。
package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/docflow/docflow/internal/onlyoffice"
)

func newOnlyofficeConfigHandler(publicURL string) *Handler {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.SetOnlyOffice(onlyoffice.New(onlyoffice.Config{
		ServerURL: "http://onlyoffice:80",
		PublicURL: publicURL,
		JWTSecret: "0123456789abcdef0123456789abcdef",
	}, nil, nil, nil, nil), 0)
	return h
}

func onlyofficeConfigGet(t *testing.T, h *Handler, url, forwardedProto string) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/onlyoffice/config", h.onlyofficeConfig)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if forwardedProto != "" {
		req.Header.Set("X-Forwarded-Proto", forwardedProto)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("config: status = %d body %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("config body: %s (%v)", w.Body.String(), err)
	}
	return out
}

// TestOnlyofficeConfigPublicURLDerivation server_url 推导规则：
// 未配置 → 按请求 Host；显式外网域名 → 原样；显式 127.0.0.1 → 视为
// 未配置推导（X-Forwarded-Proto 优先决定 scheme）。
func TestOnlyofficeConfigPublicURLDerivation(t *testing.T) {
	// 未配置：按请求 Host 推导（http）。
	h := newOnlyofficeConfigHandler("")
	if got := onlyofficeConfigGet(t, h, "http://docflow.example.com/api/v1/onlyoffice/config", ""); got["server_url"] != "http://docflow.example.com/onlyoffice" {
		t.Fatalf("server_url = %v", got["server_url"])
	}
	// 未配置 + X-Forwarded-Proto: https（caddy TLS 反代）→ https。
	if got := onlyofficeConfigGet(t, h, "http://docflow.example.com/api/v1/onlyoffice/config", "https"); got["server_url"] != "https://docflow.example.com/onlyoffice" {
		t.Fatalf("server_url (forwarded https) = %v", got["server_url"])
	}
	// 显式外网域名：原样返回（不按 Host 推导）。
	hExplicit := newOnlyofficeConfigHandler("https://office.example.com/onlyoffice")
	if got := onlyofficeConfigGet(t, hExplicit, "http://docflow.example.com/api/v1/onlyoffice/config", ""); got["server_url"] != "https://office.example.com/onlyoffice" {
		t.Fatalf("explicit server_url = %v", got["server_url"])
	}
	// 显式 127.0.0.1（env 默认/本地联调值）：视为未配置，按 Host 推导。
	hLoopback := newOnlyofficeConfigHandler("http://127.0.0.1/onlyoffice")
	if got := onlyofficeConfigGet(t, hLoopback, "http://docflow.example.com/api/v1/onlyoffice/config", "https"); got["server_url"] != "https://docflow.example.com/onlyoffice" {
		t.Fatalf("loopback server_url = %v", got["server_url"])
	}
	// 显式 localhost：同 127.0.0.1 处理。
	hLocal := newOnlyofficeConfigHandler("http://localhost/onlyoffice")
	if got := onlyofficeConfigGet(t, hLocal, "http://10.0.0.8:8080/api/v1/onlyoffice/config", ""); got["server_url"] != "http://10.0.0.8:8080/onlyoffice" {
		t.Fatalf("localhost server_url = %v", got["server_url"])
	}
}

// TestOnlyofficeConfigDisabled 集成未启用：{enabled:false, server_url:null}。
func TestOnlyofficeConfigDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	r := gin.New()
	r.GET("/api/v1/onlyoffice/config", h.onlyofficeConfig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/onlyoffice/config", nil))
	if w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("disabled config: %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["enabled"] != false || out["server_url"] != nil {
		t.Fatalf("disabled config body: %v", out)
	}
}

// TestOnlyofficeAIPluginManifest 动态插件清单：绝对 URL（按请求 Host 推导
// 或显式 BrowserBase）、guid/panel 形态与静态插件页路径配套。
func TestOnlyofficeAIPluginManifest(t *testing.T) {
	h := newOnlyofficeConfigHandler("")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/onlyoffice/ai-plugin/config.json", h.onlyofficeAIPluginManifest)
	get := func(url, forwardedProto string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		if forwardedProto != "" {
			req.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("manifest: status = %d body %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("manifest body: %s (%v)", w.Body.String(), err)
		}
		return out
	}
	// 按请求 Host 推导（X-Forwarded-Proto 优先）。
	out := get("http://docflow.example.com/api/v1/onlyoffice/ai-plugin/config.json", "https")
	if out["baseUrl"] != "https://docflow.example.com/oo-plugins/docflow-ai/" {
		t.Fatalf("baseUrl = %v", out["baseUrl"])
	}
	if out["guid"] != onlyoffice.AIPluginGUID {
		t.Fatalf("guid = %v", out["guid"])
	}
	vars := out["variations"].([]any)
	v := vars[0].(map[string]any)
	if v["url"] != "https://docflow.example.com/oo-plugins/docflow-ai/index.html" {
		t.Fatalf("variation url = %v", v["url"])
	}
	if v["type"] != "panel" || v["isInsideMode"] != false {
		t.Fatalf("variation 形态 = %v", v)
	}
	// 显式 BrowserBase（PUBLIC_BASE_URL）优先。
	hExplicit := onlyoffice.New(onlyoffice.Config{
		ServerURL:   "http://onlyoffice:80",
		BrowserBase: "https://docs.example.com",
		JWTSecret:   "0123456789abcdef0123456789abcdef",
	}, nil, nil, nil, nil)
	h2 := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h2.SetOnlyOffice(hExplicit, 0)
	r2 := gin.New()
	r2.GET("/api/v1/onlyoffice/ai-plugin/config.json", h2.onlyofficeAIPluginManifest)
	req := httptest.NewRequest(http.MethodGet, "http://inner.host/api/v1/onlyoffice/ai-plugin/config.json", nil)
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)
	var out2 map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out2); err != nil {
		t.Fatalf("manifest body: %s (%v)", w.Body.String(), err)
	}
	if out2["baseUrl"] != "https://docs.example.com/oo-plugins/docflow-ai/" {
		t.Fatalf("explicit baseUrl = %v", out2["baseUrl"])
	}
}
