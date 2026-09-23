// ai_lookup_test.go：模型能力自动识别端点测试（httptest 假 models.dev
// 上游）——命中（含宽松匹配与 provider 目录优先）、未命中、上游超时
// （found:false 静默降级）与缓存复用。
package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeModelsDevJSON 假 api.json 载荷：覆盖 chat/embed/rerank/image 四类、
// attachment 与 modalities 两种视觉标记、带目录前缀的 id。
const fakeModelsDevJSON = `{
  "openai": {"name": "OpenAI", "models": {
    "gpt-4o": {"id": "gpt-4o", "name": "GPT-4o", "attachment": true, "reasoning": false,
      "modalities": {"input": ["text", "image"]}},
    "text-embedding-3-small": {"id": "text-embedding-3-small", "name": "Text Embedding 3 Small", "type": "embed"},
    "gpt-image-1": {"id": "gpt-image-1", "name": "GPT Image 1", "type": "image"}
  }},
  "deepseek": {"name": "DeepSeek", "models": {
    "deepseek/deepseek-chat": {"id": "deepseek/deepseek-chat", "name": "DeepSeek Chat", "reasoning": true,
      "modalities": {"input": ["text"]}}
  }},
  "jina": {"name": "Jina", "models": {
    "jina-reranker-v2": {"id": "jina-reranker-v2", "name": "Jina Reranker v2", "type": "rerank"}
  }},
  "gemini-pro": {"name": "Google", "models": {
    "gemini-2.5-pro": {"id": "gemini-2.5-pro", "name": "Gemini 2.5 Pro", "attachment": true, "reasoning": true,
      "modalities": {"input": ["text", "image", "audio", "video"]}}
  }}
}`

// setupModelsDevFake 挂假上游并重置包级缓存；返回清理函数。
func setupModelsDevFake(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	origURL, origTimeout, origCache := modelsDevAPIURL, modelsDevTimeout, modelsDevCache
	srv := httptest.NewServer(handler)
	modelsDevAPIURL = srv.URL
	modelsDevCache = nil
	t.Cleanup(func() {
		modelsDevAPIURL, modelsDevTimeout, modelsDevCache = origURL, origTimeout, origCache
		srv.Close()
	})
}

// newAILookupRouter 组装 lookup 端点测试路由。
func newAILookupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	r := gin.New()
	r.GET("/api/v1/admin/settings/ai/models/lookup", h.aiModelLookup)
	r.GET("/api/v1/ai/models/lookup", h.aiModelLookup)
	return r
}

func getLookup(t *testing.T, r *gin.Engine, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// lookupResp 为 lookup 端点响应（键序无关断言用）。
type lookupResp struct {
	Found       bool   `json:"found"`
	Kind        string `json:"kind"`
	Reasoning   bool   `json:"reasoning"`
	Vision      bool   `json:"vision"`
	Audio       bool   `json:"audio"`
	Video       bool   `json:"video"`
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider"`
}

func getLookupJSON(t *testing.T, r *gin.Engine, path string) lookupResp {
	t.Helper()
	code, body := getLookup(t, r, path)
	if code != http.StatusOK {
		t.Fatalf("status = %d body %s", code, body)
	}
	var resp lookupResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return resp
}

// TestAIModelLookupHit 命中：类型映射（chat/embed/rerank/image→平台 kind）、
// 能力并集（attachment 与 modalities.input 两种视觉标记）、宽松 id 匹配
// （大小写/分隔符）与 id 尾段（带目录前缀）匹配。
func TestAIModelLookupHit(t *testing.T) {
	setupModelsDevFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeModelsDevJSON))
	})
	r := newAILookupRouter()

	t.Run("chat with vision via attachment", func(t *testing.T) {
		resp := getLookupJSON(t, r, "/api/v1/admin/settings/ai/models/lookup?model=gpt-4o")
		if !resp.Found || resp.Kind != "chat" || !resp.Vision || resp.Reasoning || resp.Audio || resp.Video {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		if resp.DisplayName != "GPT-4o" || resp.Provider != "openai" {
			t.Fatalf("display/provider mismatch: %+v", resp)
		}
	})
	t.Run("embedding type mapped", func(t *testing.T) {
		if resp := getLookupJSON(t, r, "/api/v1/ai/models/lookup?model=text-embedding-3-small"); !resp.Found || resp.Kind != "embedding" {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})
	t.Run("rerank type mapped", func(t *testing.T) {
		if resp := getLookupJSON(t, r, "/api/v1/ai/models/lookup?model=jina-reranker-v2"); !resp.Found || resp.Kind != "rerank" {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})
	t.Run("image type mapped", func(t *testing.T) {
		if resp := getLookupJSON(t, r, "/api/v1/ai/models/lookup?model=gpt-image-1"); !resp.Found || resp.Kind != "image" {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})
	t.Run("prefixed id tail segment matched", func(t *testing.T) {
		resp := getLookupJSON(t, r, "/api/v1/ai/models/lookup?model=deepseek-chat")
		if !resp.Found || resp.Kind != "chat" || !resp.Reasoning || resp.Vision {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})
	t.Run("loose separator/case matching", func(t *testing.T) {
		if resp := getLookupJSON(t, r, "/api/v1/ai/models/lookup?model=GPT_4o"); !resp.Found || resp.Kind != "chat" {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})
	t.Run("modalities union (audio/video)", func(t *testing.T) {
		resp := getLookupJSON(t, r, "/api/v1/ai/models/lookup?model=gemini-2.5-pro")
		if !resp.Found || resp.Kind != "chat" || !resp.Reasoning || !resp.Vision || !resp.Audio || !resp.Video {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})
}

// TestAIModelLookupProviderScope provider 参数（目录名或显示名）优先在该
// 目录内匹配；不匹配目录（如平台 kind openai_compatible）回落全局索引。
func TestAIModelLookupProviderScope(t *testing.T) {
	setupModelsDevFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeModelsDevJSON))
	})
	r := newAILookupRouter()
	// 目录 slug 匹配 deepseek（显示名 DeepSeek 同效）。
	for _, provider := range []string{"deepseek", "DeepSeek"} {
		code, body := getLookup(t, r, "/api/v1/ai/models/lookup?model=deepseek-chat&provider="+provider)
		if code != http.StatusOK || !strings.Contains(body, `"found":true`) {
			t.Fatalf("provider=%s: status %d body %s", provider, code, body)
		}
	}
	// 平台 kind（openai_compatible）不匹配任何目录 → 全局索引照常命中。
	code, body := getLookup(t, r, "/api/v1/ai/models/lookup?model=gpt-4o&provider=openai_compatible")
	if code != http.StatusOK || !strings.Contains(body, `"found":true`) {
		t.Fatalf("kind provider fallback failed: %d %s", code, body)
	}
}

// TestAIModelLookupMiss 未命中与 model 缺失：未命中 200 found:false；
// model 缺失 400。
func TestAIModelLookupMiss(t *testing.T) {
	setupModelsDevFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeModelsDevJSON))
	})
	r := newAILookupRouter()
	code, body := getLookup(t, r, "/api/v1/ai/models/lookup?model=totally-unknown-model")
	if code != http.StatusOK || !strings.Contains(body, `"found":false`) {
		t.Fatalf("miss should be 200 found:false, got %d %s", code, body)
	}
	code, _ = getLookup(t, r, "/api/v1/ai/models/lookup?model=")
	if code != http.StatusBadRequest {
		t.Fatalf("empty model should be 400, got %d", code)
	}
}

// TestAIModelLookupTimeout 上游超时：found:false（200，前端静默手动），
// 且失败不缓存（后续上游恢复可重新命中）。
func TestAIModelLookupTimeout(t *testing.T) {
	slow := false
	setupModelsDevFake(t, func(w http.ResponseWriter, r *http.Request) {
		if slow {
			time.Sleep(600 * time.Millisecond)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeModelsDevJSON))
	})
	modelsDevTimeout = 150 * time.Millisecond
	r := newAILookupRouter()

	slow = true
	code, body := getLookup(t, r, "/api/v1/ai/models/lookup?model=gpt-4o")
	if code != http.StatusOK || !strings.Contains(body, `"found":false`) {
		t.Fatalf("timeout should be 200 found:false, got %d %s", code, body)
	}

	slow = false
	code, body = getLookup(t, r, "/api/v1/ai/models/lookup?model=gpt-4o")
	if code != http.StatusOK || !strings.Contains(body, `"found":true`) {
		t.Fatalf("recovered upstream should hit, got %d %s", code, body)
	}
}

// TestAIModelLookupCache 缓存复用：首次抓取后上游关闭，TTL 内命中缓存。
func TestAIModelLookupCache(t *testing.T) {
	hits := 0
	setupModelsDevFake(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeModelsDevJSON))
	})
	r := newAILookupRouter()
	for i := 0; i < 3; i++ {
		code, body := getLookup(t, r, "/api/v1/ai/models/lookup?model=gpt-4o")
		if code != http.StatusOK || !strings.Contains(body, `"found":true`) {
			t.Fatalf("request %d failed: %d %s", i, code, body)
		}
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (cached)", hits)
	}
}
