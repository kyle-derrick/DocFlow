package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
)

// fakeAIClient 是 aiSummarizer 的可控实现。
type fakeAIClient struct {
	enabled bool
	summary string
	err     error
	gotText string
	gotName string
}

func (f *fakeAIClient) Enabled() bool { return f.enabled }

func (f *fakeAIClient) Summarize(_ context.Context, text, filename string) (string, error) {
	f.gotText, f.gotName = text, filename
	if f.err != nil {
		return "", f.err
	}
	return f.summary, nil
}

// fakeAIFiles 是 aiFileSource 的内存实现：按 (user,file) 返回预置结果，
// 非 owner 个人文件与生产 authorizeFileAccess 一致按 ErrNotFound 处理。
type fakeAIFiles struct {
	owner   uuid.UUID
	file    files.File
	blob    files.ObjectBlob
	version files.FileVersion
	getErr  error
}

func (f *fakeAIFiles) Get(user, fileID uuid.UUID) (files.File, error) {
	if f.getErr != nil {
		return files.File{}, f.getErr
	}
	if user != f.owner || fileID != f.file.ID {
		return files.File{}, files.ErrNotFound
	}
	return f.file, nil
}

func (f *fakeAIFiles) CurrentVersion(user, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	if _, err := f.Get(user, fileID); err != nil {
		return files.FileVersion{}, files.ObjectBlob{}, err
	}
	return f.version, f.blob, nil
}

// newAITestHandler 组装 AI 摘要端点测试环境（内存文件源 + 假客户端 +
// 内存存储）；actor 为注入路由的请求用户；client 为 nil 时模拟未注入。
func newAITestHandler(actor uuid.UUID, src *fakeAIFiles, client aiSummarizer, content string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiFiles = src
	h.ai = client
	if content != "" {
		h.storage = newMemStorage()
		if err := h.storage.Put(src.blob.StorageKey, strings.NewReader(content)); err != nil {
			panic(err)
		}
	}
	r := gin.New()
	r.POST("/api/v1/files/:id/ai/summary", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, actor)
		h.fileAISummary(c)
	})
	return r
}

func callAISummary(r *gin.Engine, fileID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/"+fileID+"/ai/summary", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newAISource(mime, name, status string, size int64) *fakeAIFiles {
	owner := uuid.New()
	return &fakeAIFiles{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: name, OwnerID: owner, Type: "file", ScopeType: "personal"},
		version: files.FileVersion{ID: uuid.New()},
		blob:    files.ObjectBlob{StorageKey: "objects/ai", Size: size, MimeType: mime, Status: status},
	}
}

// TestAISummaryEndpointSuccess 成功：200 {summary}，内容与文件名透传给客户端。
func TestAISummaryEndpointSuccess(t *testing.T) {
	src := newAISource("text/markdown", "notes.md", files.BlobStatusAvailable, 12)
	client := &fakeAIClient{enabled: true, summary: "这是摘要。"}
	r := newAITestHandler(src.owner, src, client, "hello content")

	w := callAISummary(r, src.file.ID.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, `"summary":"这是摘要。"`) {
		t.Fatalf("body = %s", body)
	}
	if client.gotText != "hello content" || client.gotName != "notes.md" {
		t.Fatalf("client got (%q, %q)", client.gotText, client.gotName)
	}
}

// TestAISummaryEndpointDisabled 未注入客户端 / AI 禁用：503 AI_DISABLED。
func TestAISummaryEndpointDisabled(t *testing.T) {
	src := newAISource("text/plain", "a.txt", files.BlobStatusAvailable, 1)
	r := newAITestHandler(src.owner, src, &fakeAIClient{enabled: false}, "x")
	if w := callAISummary(r, src.file.ID.String()); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "AI_DISABLED") {
		t.Fatalf("disabled client: status = %d body %s", w.Code, w.Body.String())
	}

	// 未注入客户端（h.ai nil）。
	r2 := newAITestHandler(src.owner, src, nil, "x")
	if w := callAISummary(r2, src.file.ID.String()); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: status = %d, want 503", w.Code)
	}
}

// TestAISummaryEndpointValidation 非文本 400；版本不可用 409；内容过长 413；
// 越权（非 owner）404。
func TestAISummaryEndpointValidation(t *testing.T) {
	// 非文本（二进制 mime + 不在扩展名白名单）。
	src := newAISource("application/octet-stream", "logo.png", files.BlobStatusAvailable, 4)
	r := newAITestHandler(src.owner, src, &fakeAIClient{enabled: true, summary: "s"}, "data")
	if w := callAISummary(r, src.file.ID.String()); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "NOT_TEXT_FILE") {
		t.Fatalf("binary: status = %d body %s", w.Code, w.Body.String())
	}
	// 文本判定走扩展名白名单（上传链 mime 恒 octet-stream）。
	textSrc := newAISource("application/octet-stream", "notes.md", files.BlobStatusAvailable, 2)
	r2 := newAITestHandler(textSrc.owner, textSrc, &fakeAIClient{enabled: true, summary: "s"}, "ok")
	if w := callAISummary(r2, textSrc.file.ID.String()); w.Code != http.StatusOK {
		t.Fatalf("ext whitelist: status = %d body %s", w.Code, w.Body.String())
	}
	// 版本未 available。
	scanSrc := newAISource("text/plain", "a.txt", files.BlobStatusScanning, 2)
	r3 := newAITestHandler(scanSrc.owner, scanSrc, &fakeAIClient{enabled: true, summary: "s"}, "ok")
	if w := callAISummary(r3, scanSrc.file.ID.String()); w.Code != http.StatusConflict {
		t.Fatalf("scanning: status = %d, want 409", w.Code)
	}
	// 内容过长（blob.Size > 100KB，读前快判）。
	bigSrc := newAISource("text/plain", "big.txt", files.BlobStatusAvailable, ai.MaxInputBytes+1)
	r4 := newAITestHandler(bigSrc.owner, bigSrc, &fakeAIClient{enabled: true, summary: "s"}, strings.Repeat("a", 10))
	if w := callAISummary(r4, bigSrc.file.ID.String()); w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "CONTENT_TOO_LARGE") {
		t.Fatalf("too large: status = %d body %s", w.Code, w.Body.String())
	}
	// 防御性读侧兜底（Size 与实际内容不一致）。
	lieSrc := newAISource("text/plain", "a.txt", files.BlobStatusAvailable, 10)
	r5 := newAITestHandler(lieSrc.owner, lieSrc, &fakeAIClient{enabled: true, summary: "s"}, strings.Repeat("a", ai.MaxInputBytes+1))
	if w := callAISummary(r5, lieSrc.file.ID.String()); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("read-side too large: status = %d, want 413", w.Code)
	}
	// 越权：非 owner 读取（404 不泄露存在性）。
	r6 := newAITestHandler(uuid.New(), src, &fakeAIClient{enabled: true, summary: "s"}, "data")
	if w := callAISummary(r6, src.file.ID.String()); w.Code != http.StatusNotFound {
		t.Fatalf("forbidden read: status = %d, want 404", w.Code)
	}
}

// TestAISummaryEndpointUpstreamError 上游失败 502；文件不存在 404。
func TestAISummaryEndpointUpstreamError(t *testing.T) {
	src := newAISource("text/plain", "a.txt", files.BlobStatusAvailable, 2)
	r := newAITestHandler(src.owner, src, &fakeAIClient{enabled: true, err: ai.ErrUpstream}, "ok")
	if w := callAISummary(r, src.file.ID.String()); w.Code != http.StatusBadGateway {
		t.Fatalf("upstream: status = %d, want 502 (body %s)", w.Code, w.Body.String())
	}
	missing := &fakeAIFiles{owner: uuid.New(), getErr: files.ErrNotFound}
	r2 := newAITestHandler(missing.owner, missing, &fakeAIClient{enabled: true, summary: "s"}, "")
	if w := callAISummary(r2, uuid.New().String()); w.Code != http.StatusNotFound {
		t.Fatalf("missing file: status = %d, want 404", w.Code)
	}
}
