package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const drawioTestAPISecret = "0123456789abcdef0123456789abcdef"

func drawioTestToken(t *testing.T) string {
	t.Helper()
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": uuid.NewString(), "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(drawioTestAPISecret))
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}
	return access
}

func drawioConfigResponse(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h.Register(router, drawioTestAPISecret, 120, 10, 60)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/drawio/config", nil)
	req.Header.Set("Authorization", "Bearer "+drawioTestToken(t))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("drawio config: status = %d, body = %s", w.Code, w.Body.String())
	}
	var cfg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("drawio config body: %v", err)
	}
	return cfg
}

// config 探测端点恒注册（认证组）：无 Bearer 401；未启用（未 SetDrawio）时
// enabled=false 且 url=null，不暴露内部地址。
func TestDrawioConfigDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	router := gin.New()
	h.Register(router, drawioTestAPISecret, 120, 10, 60)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/drawio/config", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("drawio config without bearer: status = %d, want 401", w.Code)
	}

	cfg := drawioConfigResponse(t, h)
	if cfg["enabled"] != false || cfg["url"] != nil {
		t.Fatalf("drawio config disabled = %v, url = %v；want {false, nil}", cfg["enabled"], cfg["url"])
	}
}

// 启用态：url 优先返回 DRAWIO_PUBLIC_URL；未配置 PUBLIC_URL 时回退 SERVER_URL。
func TestDrawioConfigURLResolution(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	h.SetDrawio(true, "http://drawio:8080", "https://example.com/drawio")
	cfg := drawioConfigResponse(t, h)
	if cfg["enabled"] != true || cfg["url"] != "https://example.com/drawio" {
		t.Fatalf("drawio config public = %v, url = %v", cfg["enabled"], cfg["url"])
	}

	h2 := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	h2.SetDrawio(true, "http://drawio:8080", "")
	cfg2 := drawioConfigResponse(t, h2)
	if cfg2["enabled"] != true || cfg2["url"] != "http://drawio:8080" {
		t.Fatalf("drawio config fallback = %v, url = %v", cfg2["enabled"], cfg2["url"])
	}
}
