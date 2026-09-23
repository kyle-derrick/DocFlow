// Package ai —— v1 能力单测（多 Provider ChatService / 内容抽取 / RAG）。
package ai

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/settings"
)

// mockConfig 返回含单个 mock Provider 的配置读取器。
func mockConfig() ConfigProvider {
	return func() (settings.AIConfig, error) {
		return settings.AIConfig{
			Providers:       []settings.AIProvider{{ID: "m1", Name: "Mock", Kind: settings.AIKindMock, Model: "mock-echo", Enabled: true}},
			DefaultProvider: "m1",
			Temperature:     settings.AITemperatureDefault,
			MaxTokens:       settings.AIMaxTokensDefault,
			PerUserPerMin:   settings.AIPerUserPerMinDefault,
		}, nil
	}
}

// captureDeltas 收集流式增量（顺序敏感）。
func captureDeltas() (*[]string, func(string)) {
	var out []string
	return &out, func(s string) { out = append(out, s) }
}

func TestChatMockEchoStream(t *testing.T) {
	svc := NewService(mockConfig())
	deltas, onDelta := captureDeltas()
	res, err := svc.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "你好，DocFlow"}},
		Stream:   true,
	}, onDelta)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.ProviderID != "m1" || res.ProviderKind != settings.AIKindMock || res.Model != "mock-echo" {
		t.Fatalf("result meta = %+v", res)
	}
	joined := strings.Join(*deltas, "")
	if joined != res.Content {
		t.Fatalf("deltas (%d chunks) 拼装与 Content 不一致", len(*deltas))
	}
	if !strings.Contains(res.Content, "你好，DocFlow") {
		t.Fatalf("echo 内容缺失: %q", res.Content)
	}
	if len(*deltas) < 2 {
		t.Fatalf("mock 流式应多块输出，got %d", len(*deltas))
	}
}

func TestChatMockSummarizeTemplate(t *testing.T) {
	svc := NewService(mockConfig())
	res, err := svc.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "文件名：notes.md\n\n# 标题\n第一行内容"}},
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(res.Content, "【Mock 摘要】") || !strings.Contains(res.Content, "notes.md") {
		t.Fatalf("mock 摘要模板缺失: %q", res.Content)
	}
}

func TestChatMockRAGSources(t *testing.T) {
	svc := NewService(mockConfig())
	repo := search.NewMemoryRepo()
	fileID := uuid.New()
	svc.SetSearcher(search.NewStore(repo))
	_, sources, res, err := svc.AskDocs(context.Background(), uuid.New(), "部署", nil, nil, nil)
	if err != nil {
		t.Fatalf("AskDocs: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("空索引不应有来源，got %d", len(sources))
	}
	if !strings.Contains(res.Content, "未检索到") && !strings.Contains(res.Content, "Mock") {
		t.Fatalf("空 RAG 回答异常: %q", res.Content)
	}
	// 命中一篇文档：mock 应回显来源段落。
	owner := uuid.New()
	repo.PutDoc(search.Doc{FileID: fileID, OwnerID: owner, Name: "部署手册.md", Content: "docker compose 部署指南"})
	repo.PutFile(fileID, search.MemoryFile{Name: "部署手册.md", Type: "file"})
	answer, sources, _, err := svc.AskDocs(context.Background(), owner, "部署", nil, nil, nil)
	if err != nil {
		t.Fatalf("AskDocs hit: %v", err)
	}
	if len(sources) != 1 || sources[0].URL != "/view/"+fileID.String() {
		t.Fatalf("sources = %+v", sources)
	}
	if !strings.Contains(answer, "来源：") {
		t.Fatalf("mock RAG 回答缺来源段: %q", answer)
	}
}

func TestChatNoProvider(t *testing.T) {
	svc := NewService(func() (settings.AIConfig, error) { return settings.DefaultAIConfig(), nil })
	if _, err := svc.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err == nil || !strings.Contains(err.Error(), "no ai provider") {
		t.Fatalf("err = %v, want ErrNoProvider", err)
	}
	if _, _, err := svc.ResolveProvider("missing"); err == nil {
		t.Fatal("ResolveProvider missing 应报错")
	}
}

func TestChatProviderDisabledAndNotFound(t *testing.T) {
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "off", Name: "x", Kind: settings.AIKindMock, Enabled: false}}}, nil
	})
	if _, err := svc.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err == nil {
		t.Fatal("全部禁用应 ErrNoProvider")
	}
	if _, err := svc.Chat(context.Background(), ChatRequest{ProviderID: "off", Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err == nil {
		t.Fatal("指定禁用 Provider 应 ErrProviderNotFound")
	}
}

// fakeOpenAISSE 构造 OpenAI 兼容 SSE 流式假服务。
func fakeOpenAISSE(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k1" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		chunks := []string{"你好", "，", "世界"}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + quoteJSON(c) + "}}]}\n\n"))
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func quoteJSON(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}

func TestChatOpenAIStreamPassthrough(t *testing.T) {
	srv := fakeOpenAISSE(t)
	defer srv.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: srv.URL, APIKey: "k1", Model: "gpt-test", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	deltas, onDelta := captureDeltas()
	res, err := svc.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, Stream: true}, onDelta)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "你好，世界" {
		t.Fatalf("content = %q", res.Content)
	}
	if len(*deltas) != 3 {
		t.Fatalf("deltas = %v", *deltas)
	}
	if res.PromptTokens != 7 || res.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", res)
	}
}

// TestChatOpenAIStreamFallbackNonStream：流式失败（500）回退非流式成功。
func TestChatOpenAIStreamFallbackNonStream(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = jsonDecode(r, &body)
		if _, streaming := body["stream"]; streaming {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"stream unsupported"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"fallback ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: srv.URL, Model: "m", Enabled: true}}, Temperature: 0.3, MaxTokens: 64}, nil
	})
	res, err := svc.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, Stream: true}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "fallback ok" || calls != 2 {
		t.Fatalf("res=%+v calls=%d", res, calls)
	}
}

// jsonDecode 读取请求体并解码为 map（测试辅助）。
func jsonDecode(r *http.Request, dst *map[string]any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}

// TestChatAnthropicNonStream：messages API 非流式 + system 参数 + 头部。
func TestChatAnthropicNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("x-api-key"); got != "ak" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Error("anthropic-version 头缺失")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"claude says hi"}],"usage":{"input_tokens":5,"output_tokens":4}}`))
	}))
	defer srv.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "a1", Name: "Anthropic", Kind: settings.AIKindAnthropic, BaseURL: srv.URL, APIKey: "ak", Model: "claude-3", Enabled: true}}, Temperature: 0.3, MaxTokens: 64}, nil
	})
	res, err := svc.Chat(context.Background(), ChatRequest{System: "be brief", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "claude says hi" || res.PromptTokens != 5 || res.CompletionTokens != 4 {
		t.Fatalf("res = %+v", res)
	}
}

func TestSummarizeFileFlow(t *testing.T) {
	svc := NewService(mockConfig())
	src := &fakeFileSource{meta: FileMeta{ID: uuid.New(), Name: "notes.md", MimeType: "text/markdown", Size: 12}, data: []byte("# 标题\n正文一行")}
	summary, res, err := svc.SummarizeFile(context.Background(), uuid.New(), src, src.meta.ID, nil)
	if err != nil {
		t.Fatalf("SummarizeFile: %v", err)
	}
	if !strings.Contains(summary, "【Mock 摘要】") || !strings.Contains(summary, "notes.md") {
		t.Fatalf("summary = %q", summary)
	}
	if res.ProviderID != "m1" {
		t.Fatalf("provider = %s", res.ProviderID)
	}
}

// fakeFileSource 为 FileSource 的内存实现。
type fakeFileSource struct {
	meta FileMeta
	data []byte
	err  error
}

func (f *fakeFileSource) FileWithContent(_, _ uuid.UUID) (FileMeta, []byte, error) {
	if f.err != nil {
		return FileMeta{}, nil, f.err
	}
	return f.meta, f.data, nil
}

// ---------- 内容抽取 ----------

func zipBytes(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractDocx(t *testing.T) {
	data := zipBytes(t, map[string][]byte{
		"word/document.xml": []byte(`<w:document><w:body><w:p><w:r><w:t>第一段</w:t></w:r><w:r><w:t>文字</w:t></w:r></w:p><w:p><w:r><w:t>第二段</w:t></w:r></w:p></w:body></w:document>`),
	})
	text, ok := ExtractText("a.docx", "", data)
	if !ok || !strings.Contains(text, "第一段文字") || !strings.Contains(text, "第二段") {
		t.Fatalf("text = %q ok=%v", text, ok)
	}
}

func TestExtractXlsx(t *testing.T) {
	data := zipBytes(t, map[string][]byte{
		"xl/sharedStrings.xml":     []byte(`<sst><si><t>表头A</t></si><si><t>表头B</t></si></sst>`),
		"xl/worksheets/sheet1.xml": []byte(`<worksheet><sheetData><row><c><is><t>内联值</t></is></c></row></sheetData></worksheet>`),
	})
	text, ok := ExtractText("b.xlsx", "", data)
	if !ok || !strings.Contains(text, "表头A") || !strings.Contains(text, "内联值") {
		t.Fatalf("text = %q ok=%v", text, ok)
	}
}

func TestExtractPptx(t *testing.T) {
	data := zipBytes(t, map[string][]byte{
		"ppt/slides/slide1.xml": []byte(`<p:sld><a:t>标题页</a:t><a:t>副标题</a:t></p:sld>`),
		"ppt/slides/slide2.xml": []byte(`<p:sld><a:t>第二页</a:t></p:sld>`),
	})
	text, ok := ExtractText("c.pptx", "", data)
	if !ok || !strings.Contains(text, "标题页") || !strings.Contains(text, "第二页") {
		t.Fatalf("text = %q ok=%v", text, ok)
	}
}

func TestExtractTiptapAndPlain(t *testing.T) {
	doc := []byte(`{"type":"doc","content":[{"type":"heading","content":[{"type":"text","text":"标题"}]},{"type":"paragraph","content":[{"type":"text","text":"段落"}]},{"type":"codeBlock","content":[{"type":"text","text":"code()"}]}]}`)
	text, ok := ExtractText("a.dfdoc", "", doc)
	if !ok || !strings.Contains(text, "标题") || !strings.Contains(text, "段落") || !strings.Contains(text, "code()") {
		t.Fatalf("dfdoc text = %q ok=%v", text, ok)
	}
	plain, ok := ExtractText("a.md", "text/markdown", []byte("# hello\n世界"))
	if !ok || plain != "# hello\n世界" {
		t.Fatalf("plain = %q", plain)
	}
	// 二进制：不支持。
	if _, ok := ExtractText("a.png", "image/png", []byte{0x89, 0x50, 0x4e, 0x47, 0, 1, 2, 3}); ok {
		t.Fatal("png 不应支持抽取")
	}
}

func TestExtractDrawioAndExcalidraw(t *testing.T) {
	drawio := []byte(`<mxfile><diagram><mxGraphModel><root><mxCell value="开始节点"/><mxCell value="&lt;b&gt;加粗&lt;/b&gt;标签"/></root></mxGraphModel></diagram></mxfile>`)
	text, ok := ExtractText("f.drawio", "", drawio)
	if !ok || !strings.Contains(text, "开始节点") || !strings.Contains(text, "加粗标签") {
		t.Fatalf("drawio text = %q ok=%v", text, ok)
	}
	ex := []byte(`{"elements":[{"type":"text","text":"白板文字"},{"type":"rectangle"}]}`)
	text, ok = ExtractText("g.excalidraw", "", ex)
	if !ok || text != "白板文字" {
		t.Fatalf("excalidraw text = %q ok=%v", text, ok)
	}
}

func TestExtractPDF(t *testing.T) {
	// 构造最小 PDF：FlateDecode 内容流含两段文本。
	var stream bytes.Buffer
	zw := zlib.NewWriter(&stream)
	_, _ = zw.Write([]byte("BT /F1 12 Tf (Hello PDF) Tj ET\nBT /F1 12 Tf (Second line) Tj ET"))
	_ = zw.Close()
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	pdf.WriteString("1 0 obj\n<< /Length " + strconv.Itoa(stream.Len()) + " /Filter /FlateDecode >>\nstream\n")
	pdf.Write(stream.Bytes())
	pdf.WriteString("\nendstream\nendobj\ntrailer\n<< >>\n%%EOF")
	text, ok := ExtractText("h.pdf", "", pdf.Bytes())
	if !ok || !strings.Contains(text, "Hello PDF") || !strings.Contains(text, "Second line") {
		t.Fatalf("pdf text = %q ok=%v", text, ok)
	}
}

// ---------- settings 校验 ----------

func TestValidateAI(t *testing.T) {
	bad := settings.AIConfig{Providers: []settings.AIProvider{{ID: "x", Name: "X", Kind: "bogus"}}}
	if err := settings.ValidateAI(bad); err == nil {
		t.Fatal("非法 kind 应报错")
	}
	dup := settings.AIConfig{Providers: []settings.AIProvider{
		{ID: "a", Name: "A", Kind: settings.AIKindMock, Enabled: true},
		{ID: "a", Name: "B", Kind: settings.AIKindMock},
	}}
	if err := settings.ValidateAI(dup); err == nil {
		t.Fatal("重复 ID 应报错")
	}
	good := settings.AIConfig{
		Providers:       []settings.AIProvider{{ID: "a", Name: "A", Kind: settings.AIKindMock, Enabled: true}, {ID: "b", Name: "B", Kind: settings.AIKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "llama3", Enabled: true}},
		DefaultProvider: "a", Temperature: 0.3, MaxTokens: 2048, PerUserPerMin: 20,
	}
	if err := settings.ValidateAI(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	if err := settings.ValidateAI(settings.AIConfig{DefaultProvider: "missing"}); err == nil {
		t.Fatal("默认 Provider 不存在应报错")
	}
}

// ---------- 限流器（http 包外复用的纯逻辑在此不测；见 http 侧测试） ----------
