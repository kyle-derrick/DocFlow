package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient 指向假 OpenAI 服务（短超时便于超时用例）。
func newTestClient(baseURL string, timeout time.Duration) *Client {
	return newClient(true, baseURL, "test-key", "gpt-4o-mini", timeout)
}

// fakeOpenAI 构造 /chat/completions 假服务：status 为响应码，body 为 JSON，
// requests 收到的请求体（可选）。
func fakeOpenAI(t *testing.T, status int, body string, requests *[]chatRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s, want /chat/completions", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if requests != nil {
			*requests = append(*requests, req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSummarizeSuccess(t *testing.T) {
	var got []chatRequest
	srv := fakeOpenAI(t, http.StatusOK, `{"choices":[{"message":{"content":"  这是一份季度报告摘要。  "}}]}`, &got)

	summary, err := newTestClient(srv.URL, 5*time.Second).Summarize(context.Background(), "quarterly report body", "report.txt")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if summary != "这是一份季度报告摘要。" {
		t.Fatalf("summary = %q", summary)
	}
	if len(got) != 1 {
		t.Fatalf("requests = %d, want 1", len(got))
	}
	req := got[0]
	if req.Model != "gpt-4o-mini" || req.MaxTokens != 500 {
		t.Fatalf("request = %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" ||
		req.Messages[0].Content != "用与文件相同的语言总结以下文件内容，300 字内" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[1].Content, "文件名：report.txt") ||
		!strings.Contains(req.Messages[1].Content, "quarterly report body") {
		t.Fatalf("user message = %q", req.Messages[1].Content)
	}
}

func TestSummarizeDisabledAndTooLarge(t *testing.T) {
	disabled := New(false, "http://unused", "k", "m")
	if disabled.Enabled() {
		t.Fatal("Enabled() = true, want false")
	}
	if _, err := disabled.Summarize(context.Background(), "x", "f.txt"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: err = %v, want ErrDisabled", err)
	}
	enabled := newTestClient("http://unused", time.Second)
	if !enabled.Enabled() {
		t.Fatal("Enabled() = false, want true")
	}
	if _, err := enabled.Summarize(context.Background(), strings.Repeat("a", MaxInputBytes+1), "f.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: err = %v, want ErrTooLarge", err)
	}
}

func TestSummarizeUpstreamErrors(t *testing.T) {
	// 500 → ErrUpstream。
	srv := fakeOpenAI(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`, nil)
	if _, err := newTestClient(srv.URL, time.Second).Summarize(context.Background(), "x", "f.txt"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("500: err = %v, want ErrUpstream", err)
	}
	// 200 但空 choices → ErrUpstream。
	srv2 := fakeOpenAI(t, http.StatusOK, `{}`, nil)
	if _, err := newTestClient(srv2.URL, time.Second).Summarize(context.Background(), "x", "f.txt"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("empty choices: err = %v, want ErrUpstream", err)
	}
	// 超时（上游 sleep 超过客户端超时）→ ErrUpstream。
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := newTestClient(slow.URL, 50*time.Millisecond).Summarize(ctx, "x", "f.txt"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("timeout: err = %v, want ErrUpstream", err)
	}
}
