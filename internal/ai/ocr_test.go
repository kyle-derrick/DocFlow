package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/settings"
)

// fakeOCRStorage 最小 StorageReader：按 key 返回固定字节。
type fakeOCRStorage struct{ data map[string][]byte }

func (f fakeOCRStorage) Read(key string) (io.ReadCloser, error) {
	b, ok := f.data[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// newOCRService 按固定配置构造服务（测试用）。
func newOCRService(cfg settings.AIConfig) *Service {
	return NewService(func() (settings.AIConfig, error) { return cfg, nil })
}

// fakeVisionOpenAI 校验 /chat/completions 多模态请求（model、text +
// image_url dataURL），dataURL 后缀写入 gotImage 供外部比对，按 reply 回文。
func fakeVisionOpenAI(t *testing.T, reply string, gotImage *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s, want /chat/completions", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k1" {
			t.Errorf("Authorization = %q, want Bearer k1", got)
		}
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Role    string `json:"role"`
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if req.Model != "vision-1" || req.MaxTokens != 2000 || len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Errorf("request meta = %+v", req)
		}
		parts := req.Messages[0].Content
		if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text == "" || parts[1].Type != "image_url" {
			t.Errorf("content parts = %+v, want [text, image_url]", parts)
		}
		if !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
			t.Errorf("image_url = %q, want data:image/png;base64 dataURL", parts[1].ImageURL.URL)
		}
		if gotImage != nil {
			*gotImage = strings.TrimPrefix(parts[1].ImageURL.URL, "data:image/png;base64,")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, reply)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExtractImageTextOpenAI(t *testing.T) {
	img := []byte("fake-png-bytes")
	var gotImage string
	srv := fakeVisionOpenAI(t, "  发票号 2026-001\n合计 456 元  ", &gotImage)
	svc := newOCRService(settings.AIConfig{
		Providers: []settings.AIProvider{{ID: "p1", Kind: settings.AIKindOpenAICompatible, BaseURL: srv.URL, APIKey: "k1", Model: "vision-1", Enabled: true}},
		OCR:       settings.AIOCRConfig{Enabled: true, ProviderID: "p1", ModelID: "vision-1", MaxImageBytes: 1 << 20},
	})
	svc.SetStorageReader(fakeOCRStorage{data: map[string][]byte{"img/1": img}})
	got, err := svc.ExtractImageText(context.Background(), "img/1", "image/png", "scan.png", int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "发票号 2026-001\n合计 456 元" {
		t.Fatalf("text = %q（应 trim 空白）", got)
	}
	if gotImage != base64.StdEncoding.EncodeToString(img) {
		t.Fatal("image_url dataURL 未携带图片 base64 内容")
	}
}

func TestExtractImageTextDisabledAndUnavailable(t *testing.T) {
	svc := newOCRService(settings.AIConfig{})
	svc.SetStorageReader(fakeOCRStorage{data: map[string][]byte{"k": []byte("x")}})
	// OCR 关闭 → 安静跳过（("", nil)）。
	if got, err := svc.ExtractImageText(context.Background(), "k", "image/png", "a.png", 1); got != "" || err != nil {
		t.Fatalf("disabled = (%q, %v), want (\"\", nil)", got, err)
	}
	// 开启但 Provider 不存在 → 报错。
	svc = newOCRService(settings.AIConfig{OCR: settings.AIOCRConfig{Enabled: true, ProviderID: "ghost", ModelID: "m", MaxImageBytes: 1 << 20}})
	svc.SetStorageReader(fakeOCRStorage{data: map[string][]byte{"k": []byte("x")}})
	if _, err := svc.ExtractImageText(context.Background(), "k", "image/png", "a.png", 1); err == nil || !strings.Contains(err.Error(), "ocr provider not available") {
		t.Fatalf("ghost provider err = %v", err)
	}
	// mock Provider 无视觉能力 → 报错。
	svc = newOCRService(settings.AIConfig{
		Providers: []settings.AIProvider{{ID: "mk", Kind: settings.AIKindMock, Model: "m", Enabled: true}},
		OCR:       settings.AIOCRConfig{Enabled: true, ProviderID: "mk", ModelID: "m", MaxImageBytes: 1 << 20},
	})
	svc.SetStorageReader(fakeOCRStorage{data: map[string][]byte{"k": []byte("x")}})
	if _, err := svc.ExtractImageText(context.Background(), "k", "image/png", "a.png", 1); err == nil || !strings.Contains(err.Error(), "mock provider") {
		t.Fatalf("mock err = %v", err)
	}
	// 存储读取器未注入 → 报错。
	svc = newOCRService(settings.AIConfig{
		Providers: []settings.AIProvider{{ID: "p1", Kind: settings.AIKindOpenAICompatible, BaseURL: "http://unused", Model: "m", Enabled: true}},
		OCR:       settings.AIOCRConfig{Enabled: true, ProviderID: "p1", ModelID: "m", MaxImageBytes: 1 << 20},
	})
	if _, err := svc.ExtractImageText(context.Background(), "k", "image/png", "a.png", 1); err == nil {
		t.Fatal("未注入存储读取器应报错")
	}
}

func TestExtractImageTextTooLarge(t *testing.T) {
	svc := newOCRService(settings.AIConfig{
		Providers: []settings.AIProvider{{ID: "p1", Kind: settings.AIKindOpenAICompatible, BaseURL: "http://unused", Model: "m", Enabled: true}},
		OCR:       settings.AIOCRConfig{Enabled: true, ProviderID: "p1", ModelID: "m", MaxImageBytes: 8},
	})
	svc.SetStorageReader(fakeOCRStorage{data: map[string][]byte{"k": []byte("123456789")}})
	// 声明大小超限。
	if _, err := svc.ExtractImageText(context.Background(), "k", "image/png", "a.png", 9); err == nil || !strings.Contains(err.Error(), "image too large") {
		t.Fatalf("size>max err = %v", err)
	}
	// size<=0 同样拒绝。
	if _, err := svc.ExtractImageText(context.Background(), "k", "image/png", "a.png", 0); err == nil {
		t.Fatal("size=0 应报错")
	}
	// 声明合规但实际读取超限（存储大小漂移）→ 截断防御同样报错。
	if _, err := svc.ExtractImageText(context.Background(), "k", "image/png", "a.png", 8); err == nil || !strings.Contains(err.Error(), "image too large") {
		t.Fatalf("实际超限 err = %v", err)
	}
}

func TestExtractImageTextAnthropic(t *testing.T) {
	img := []byte("fake-jpeg-bytes")
	var gotData, gotMedia string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s, want /v1/messages", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("x-api-key"); got != "k1" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Role    string `json:"role"`
				Content []struct {
					Type   string `json:"type"`
					Text   string `json:"text"`
					Source struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if req.Model != "claude-v" || req.MaxTokens != 2000 || len(req.Messages) != 1 {
			t.Errorf("request meta = %+v", req)
		}
		parts := req.Messages[0].Content
		if len(parts) != 2 || parts[0].Type != "image" || parts[0].Source.Type != "base64" || parts[1].Type != "text" || parts[1].Text == "" {
			t.Errorf("content parts = %+v, want [image(base64), text]", parts)
		}
		gotData, gotMedia = parts[0].Source.Data, parts[0].Source.MediaType
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"无文字"}]}`))
	}))
	t.Cleanup(srv.Close)
	svc := newOCRService(settings.AIConfig{
		Providers: []settings.AIProvider{{ID: "p1", Kind: settings.AIKindAnthropic, BaseURL: srv.URL, APIKey: "k1", Model: "claude-v", Enabled: true}},
		OCR:       settings.AIOCRConfig{Enabled: true, ProviderID: "p1", ModelID: "claude-v", MaxImageBytes: 1 << 20},
	})
	svc.SetStorageReader(fakeOCRStorage{data: map[string][]byte{"img/1": img}})
	got, err := svc.ExtractImageText(context.Background(), "img/1", "image/jpeg", "photo.jpg", int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	// 模型回「无文字」约定值 → 归一为空串（无内容，仅名称索引）。
	if got != "" {
		t.Fatalf("无文字应归一为空串: %q", got)
	}
	if gotData != base64.StdEncoding.EncodeToString(img) || gotMedia != "image/jpeg" {
		t.Fatalf("source = (%q, %q)，与图片内容/媒体类型不符", gotData, gotMedia)
	}
}

func TestExtractImageTextUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	t.Cleanup(srv.Close)
	svc := newOCRService(settings.AIConfig{
		Providers: []settings.AIProvider{{ID: "p1", Kind: settings.AIKindOpenAICompatible, BaseURL: srv.URL, APIKey: "k1", Model: "m", Enabled: true}},
		OCR:       settings.AIOCRConfig{Enabled: true, ProviderID: "p1", ModelID: "m", MaxImageBytes: 1 << 20},
	})
	svc.SetStorageReader(fakeOCRStorage{data: map[string][]byte{"k": []byte("x")}})
	if _, err := svc.ExtractImageText(context.Background(), "k", "image/png", "a.png", 1); !errors.Is(err, ErrUpstreamChat) {
		t.Fatalf("5xx err = %v, want ErrUpstreamChat", err)
	}
}
