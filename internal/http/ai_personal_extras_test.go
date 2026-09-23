// ai_personal_extras_test.go：个人技能 / 个人 MCP 服务端点测试（整表读写、
// auth_headers 掩码与继承、校验 400、子集隔离不互相清空、测试连接）。
package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/auth"
)

// newAIPersonalExtrasRouter 组装个人技能/个人 MCP 端点测试路由（含
// personal-settings 主端点，用于子集隔离断言）。
func newAIPersonalExtrasRouter(svc *ai.Service, store *fakeAIPrefsStore, actor uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiSvc = svc
	h.aiLimiter = newDynamicRateLimiter()
	h.settings = &fakeSettingsService{}
	h.aiPrefs = store
	withUser := func(handle gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor)
			handle(c)
		}
	}
	r := gin.New()
	r.GET("/api/v1/ai/personal-settings", withUser(h.getAIPersonalSettings))
	r.PUT("/api/v1/ai/personal-settings", withUser(h.putAIPersonalSettings))
	r.GET("/api/v1/ai/personal/skills", withUser(h.getAIPersonalSkills))
	r.PUT("/api/v1/ai/personal/skills", withUser(h.putAIPersonalSkills))
	r.GET("/api/v1/ai/personal/mcp-servers", withUser(h.getAIPersonalMCPServers))
	r.PUT("/api/v1/ai/personal/mcp-servers", withUser(h.putAIPersonalMCPServers))
	r.POST("/api/v1/ai/personal/mcp-servers/test", withUser(h.testAIPersonalMCP))
	return r
}

// TestAIPersonalSkillsRoundtrip 个人技能端点：GET 空态空数组 → PUT 保存 →
// GET 回读一致；校验失败 400 INVALID_AI_PREFS（error 含字段定位）。
func TestAIPersonalSkillsRoundtrip(t *testing.T) {
	store := newFakeAIPrefsStore()
	actor := uuid.New()
	r := newAIPersonalExtrasRouter(newAIv1Service(100), store, actor)

	// 空态：空数组（非 404/503）。
	w := personalJSON(r, http.MethodGet, "/api/v1/ai/personal/skills", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"skills":[]`) {
		t.Fatalf("空态 GET: %d %s", w.Code, w.Body.String())
	}

	// PUT 保存。
	body := `{"skills":[{"id":"polish","name":"润色","description":"润色选中文本","prompt":"请润色：{selection}"}]}`
	w2 := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/skills", body)
	if w2.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w2.Code, w2.Body.String())
	}
	// GET 回读一致。
	w3 := personalJSON(r, http.MethodGet, "/api/v1/ai/personal/skills", "")
	if w3.Code != http.StatusOK || !strings.Contains(w3.Body.String(), `"prompt":"请润色：{selection}"`) {
		t.Fatalf("GET 回读: %d %s", w3.Code, w3.Body.String())
	}

	// 校验失败：prompt 超长 → 400。
	long := strings.Repeat("字", 4001)
	w4 := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/skills", `{"skills":[{"id":"x","name":"n","prompt":"`+long+`"}]}`)
	if w4.Code != http.StatusBadRequest || !strings.Contains(w4.Body.String(), "INVALID_AI_PREFS") || !strings.Contains(w4.Body.String(), "prompt") {
		t.Fatalf("校验失败应 400 定位 prompt: %d %s", w4.Code, w4.Body.String())
	}
	// id 重复 → 400。
	w5 := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/skills", `{"skills":[{"id":"a","name":"n","prompt":"p"},{"id":"a","name":"m","prompt":"q"}]}`)
	if w5.Code != http.StatusBadRequest || !strings.Contains(w5.Body.String(), "重复") {
		t.Fatalf("id 重复应 400: %d %s", w5.Code, w5.Body.String())
	}
}

// TestAIPersonalMCPServersRoundtripAndMask 个人 MCP 端点：PUT 保存 → 掩码
// 回显（认证头值绝不回显，仅键名+configured）；GET 回读同构；留空继承
// （底层存储保留原值）；校验失败 400。
func TestAIPersonalMCPServersRoundtripAndMask(t *testing.T) {
	store := newFakeAIPrefsStore()
	actor := uuid.New()
	r := newAIPersonalExtrasRouter(newAIv1Service(100), store, actor)

	// 空态。
	w := personalJSON(r, http.MethodGet, "/api/v1/ai/personal/mcp-servers", "")
	if w.Code != http.StatusOK {
		t.Fatalf("空态 GET: %d %s", w.Code, w.Body.String())
	}

	// PUT 保存（含两个认证头）。
	body := `{"servers":[{"id":"my-mcp","name":"我的工具站","url":"https://mcp.example.com/mcp","auth_headers":{"Authorization":"Bearer tok-1","X-Api-Key":"k1"}}]}`
	w2 := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/mcp-servers", body)
	if w2.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w2.Code, w2.Body.String())
	}
	for _, leak := range []string{"tok-1", `"k1"`} {
		if strings.Contains(w2.Body.String(), leak) {
			t.Fatalf("PUT 响应泄漏认证头值（%q）: %s", leak, w2.Body.String())
		}
	}
	for _, want := range []string{`"id":"my-mcp"`, `"auth_headers":["Authorization","X-Api-Key"]`, `"auth_headers_configured":true`} {
		if !strings.Contains(w2.Body.String(), want) {
			t.Fatalf("PUT 响应缺 %q: %s", want, w2.Body.String())
		}
	}

	// GET 回读：掩码视图。
	w3 := personalJSON(r, http.MethodGet, "/api/v1/ai/personal/mcp-servers", "")
	if w3.Code != http.StatusOK || strings.Contains(w3.Body.String(), "tok-1") {
		t.Fatalf("GET 回读泄漏: %d %s", w3.Code, w3.Body.String())
	}

	// 二次 PUT：改名 + 认证头值留空（掩码回读视图的天然形态）→ 继承现值。
	noSecret := `{"servers":[{"id":"my-mcp","name":"改名站","url":"https://mcp2.example.com/mcp","auth_headers":{"Authorization":"","X-Api-Key":""}}]}`
	w4 := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/mcp-servers", noSecret)
	if w4.Code != http.StatusOK {
		t.Fatalf("继承 PUT: %d %s", w4.Code, w4.Body.String())
	}
	if !strings.Contains(w4.Body.String(), `"auth_headers_configured":true`) {
		t.Fatalf("留空应继承现值（configured=true）: %s", w4.Body.String())
	}
	// 底层存储仍含原值。
	raw, _ := store.GetAIPrefs(actor)
	if !strings.Contains(string(raw), "tok-1") || !strings.Contains(string(raw), "k1") {
		t.Fatalf("底层存储丢失继承值: %s", string(raw))
	}

	// 校验失败：url 非法 → 400 定位。
	w5 := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/mcp-servers", `{"servers":[{"id":"s","name":"n","url":"notaurl"}]}`)
	if w5.Code != http.StatusBadRequest || !strings.Contains(w5.Body.String(), "url") {
		t.Fatalf("url 非法应 400: %d %s", w5.Code, w5.Body.String())
	}
	// 新键值留空且库中无现值 → 400（提示补值）。
	w6 := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/mcp-servers", `{"servers":[{"id":"fresh","name":"n","url":"https://x.example.com/mcp","auth_headers":{"X-Token":""}}]}`)
	if w6.Code != http.StatusBadRequest || !strings.Contains(w6.Body.String(), "X-Token") {
		t.Fatalf("新键空值应 400 定位: %d %s", w6.Code, w6.Body.String())
	}
}

// TestAIPersonalSubsetIsolation 子集隔离：专用端点写入 skills/mcp_servers
// 不影响 providers/personas；主 personal-settings PUT 不携带 skills/
// mcp_servers（nil 继承）也不清空两子集。
func TestAIPersonalSubsetIsolation(t *testing.T) {
	store := newFakeAIPrefsStore()
	actor := uuid.New()
	r := newAIPersonalExtrasRouter(newAIv1Service(100), store, actor)

	// 先落主配置（provider/persona）+ 技能 + MCP 服务。
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", personalPrefsPayload("https://gw.example.com/v1")); w.Code != http.StatusOK {
		t.Fatalf("主 PUT: %d %s", w.Code, w.Body.String())
	}
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/skills", `{"skills":[{"id":"s1","name":"技能","prompt":"p"}]}`); w.Code != http.StatusOK {
		t.Fatalf("skills PUT: %d %s", w.Code, w.Body.String())
	}
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/mcp-servers", `{"servers":[{"id":"m1","name":"n","url":"https://m.example.com/mcp","auth_headers":{"Authorization":"tok-a"}}]}`); w.Code != http.StatusOK {
		t.Fatalf("mcp PUT: %d %s", w.Code, w.Body.String())
	}

	// 技能 PUT 不动 providers：主 GET 仍含 provider（api_key 继承可见）。
	if w := personalJSON(r, http.MethodGet, "/api/v1/ai/personal-settings", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"mine"`) {
		t.Fatalf("主 GET 丢失 provider: %d %s", w.Code, w.Body.String())
	}

	// 主 PUT（掩码回读形态：不含 skills/mcp_servers 字段）不清空两子集。
	maskedView := `{"providers":[{"id":"mine","name":"我的网关","kind":"openai_compatible","base_url":"https://gw.example.com/v1","models":[{"id":"my-chat","capabilities":{"kind":"chat"}}]}],"personas":[],"prefer_personal":true}`
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", maskedView); w.Code != http.StatusOK {
		t.Fatalf("主 PUT 二次: %d %s", w.Code, w.Body.String())
	}
	if w := personalJSON(r, http.MethodGet, "/api/v1/ai/personal/skills", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"s1"`) {
		t.Fatalf("skills 被主 PUT 清空: %d %s", w.Code, w.Body.String())
	}
	if w := personalJSON(r, http.MethodGet, "/api/v1/ai/personal/mcp-servers", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m1"`) {
		t.Fatalf("mcp_servers 被主 PUT 清空: %d %s", w.Code, w.Body.String())
	}

	// 技能清空（显式空数组）不影响 MCP。
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal/skills", `{"skills":[]}`); w.Code != http.StatusOK {
		t.Fatalf("skills 清空: %d %s", w.Code, w.Body.String())
	}
	if w := personalJSON(r, http.MethodGet, "/api/v1/ai/personal/mcp-servers", ""); !strings.Contains(w.Body.String(), `"id":"m1"`) {
		t.Fatalf("mcp_servers 被技能清空误伤: %s", w.Body.String())
	}
}

// fakeMCPTestUpstream 构造最小 MCP 端点（initialize + tools/list 单工具），
// 记录收到的认证头（测试连接端点断言）。
func fakeMCPTestUpstream(t *testing.T, gotAuth *string, gotKey *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		*gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		respond := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		}
		switch req.Method {
		case "initialize":
			respond(map[string]any{"protocolVersion": "2025-03-26"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			respond(map[string]any{"tools": []map[string]any{{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestAIPersonalMCPTest 个人测试连接端点：连通 ok=true + 工具名；auth_headers
// 多头透传到上游；url 缺失 400。
func TestAIPersonalMCPTest(t *testing.T) {
	var gotAuth, gotKey string
	up := fakeMCPTestUpstream(t, &gotAuth, &gotKey)
	defer up.Close()
	r := newAIPersonalExtrasRouter(newAIv1Service(100), newFakeAIPrefsStore(), uuid.New())

	w := personalJSON(r, http.MethodPost, "/api/v1/ai/personal/mcp-servers/test",
		`{"url":"`+up.URL+`","auth_headers":{"Authorization":"Bearer ptok","X-Api-Key":"pkey"}}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"names":["echo"]`) {
		t.Fatalf("测试连接: %d %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer ptok" || gotKey != "pkey" {
		t.Fatalf("认证头未透传: Authorization=%q X-Api-Key=%q", gotAuth, gotKey)
	}

	// url 缺失 400；不可达 url 恒 200 + ok=false。
	if w := personalJSON(r, http.MethodPost, "/api/v1/ai/personal/mcp-servers/test", `{"url":""}`); w.Code != http.StatusBadRequest {
		t.Fatalf("url 缺失应 400: %d", w.Code)
	}
	w2 := personalJSON(r, http.MethodPost, "/api/v1/ai/personal/mcp-servers/test", `{"url":"http://127.0.0.1:1/mcp"}`)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `"ok":false`) {
		t.Fatalf("不可达应 ok=false: %d %s", w2.Code, w2.Body.String())
	}
}
