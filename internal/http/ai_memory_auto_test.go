// ai_memory_auto_test.go：AI 记忆自动提取测试（maybeAutoExtractMemory
// 门控与后台链路、autoExtractAIMemory 落库/裁剪/去重/非 JSON 静默、
// aiChat 完成点接入 e2e）。
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/settings"
)

// ---------- fake（模式同 fakeAIMemoryStore / fakeAIPrefsStore） ----------

// fakeAutoMemoryStore 嵌入 fakeAIMemoryStore 补齐自动提取两方法（语义
// 镜像 auth.UserStore：CreateAutoAIMemory 保留校验、kind=auto、最新在
// 前；TrimAutoAIMemory 仅裁 kind=auto 的最旧条目，manual 不动，未超限
// 不动——被裁条目记入 autoTrimmed 供断言）。
type fakeAutoMemoryStore struct {
	fakeAIMemoryStore
	autoTrimmed []auth.AIMemory
}

func (f *fakeAutoMemoryStore) CreateAutoAIMemory(userID uuid.UUID, content string) (auth.AIMemory, error) {
	if err := auth.ValidateAIMemory(auth.AIMemoryKindAuto, content); err != nil {
		return auth.AIMemory{}, err
	}
	item := auth.AIMemory{ID: uuid.New(), UserID: userID, Kind: auth.AIMemoryKindAuto, Content: strings.TrimSpace(content), CreatedAt: time.Now().UTC()}
	f.items = append([]auth.AIMemory{item}, f.items...) // 最新在前（List 倒序）
	return item, nil
}

func (f *fakeAutoMemoryStore) TrimAutoAIMemory(_ uuid.UUID, keep int) error {
	out := make([]auth.AIMemory, 0, len(f.items))
	autos := 0
	for _, it := range f.items { // items 最新在前：越过 keep 的 auto 即最旧
		if it.Kind == auth.AIMemoryKindAuto {
			if autos >= keep {
				f.autoTrimmed = append(f.autoTrimmed, it)
				continue
			}
			autos++
		}
		out = append(out, it)
	}
	f.items = out
	return nil
}

func (f *fakeAutoMemoryStore) autoCount() int {
	n := 0
	for _, it := range f.items {
		if it.Kind == auth.AIMemoryKindAuto {
			n++
		}
	}
	return n
}

func (f *fakeAutoMemoryStore) find(kind, content string) *auth.AIMemory {
	for i := range f.items {
		if f.items[i].Kind == kind && f.items[i].Content == content {
			return &f.items[i]
		}
	}
	return nil
}

// memoryUpstream 构造 OpenAI 兼容假上游：stream 请求（aiChat 流式对话轮）
// 回 SSE，非流式请求回固定 JSON 补全——第 1 次非流式调用为对话轮
// （chatReply，e2e 用），其后为提取轮（extractReply）。计数调用次数。
type memoryUpstream struct {
	srv          *httptest.Server
	calls        int32
	chatReply    string
	extractReply string
}

func newMemoryUpstream(t *testing.T, chatReply, extractReply string) *memoryUpstream {
	t.Helper()
	u := &memoryUpstream{chatReply: chatReply, extractReply: extractReply}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&u.calls, 1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if stream, _ := body["stream"].(bool); stream {
			// 对话轮流式：SSE delta（chatReply 缺省回落 extractReply）。
			text := u.chatReply
			if text == "" {
				text = u.extractReply
			}
			enc, _ := json.Marshal(text)
			sse := func(payload string) { _, _ = w.Write([]byte("data: " + payload + "\n\n")) }
			w.Header().Set("Content-Type", "text/event-stream")
			sse(`{"choices":[{"delta":{"content":` + string(enc) + `}}]}`)
			sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			sse(`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":4}}`)
			sse(`[DONE]`)
			return
		}
		reply := u.extractReply
		if u.chatReply != "" && n == 1 {
			reply = u.chatReply // e2e：第 1 次非流式调用为对话轮
		}
		content, _ := json.Marshal(reply)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(content) + `}}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *memoryUpstream) callCount() int32 { return atomic.LoadInt32(&u.calls) }

func (u *memoryUpstream) service() *ai.Service {
	return ai.NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{
			Providers:       []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: u.srv.URL, Model: "gpt-test", Enabled: true}},
			DefaultProvider: "o1", Temperature: 0.2, MaxTokens: 64, PerUserPerMin: 100,
		}, nil
	})
}

// newAutoMemoryHandler 组装提取链路测试 handler（prefs/memory 注入）。
func newAutoMemoryHandler(svc *ai.Service, prefs *fakeAIPrefsStore, mem *fakeAutoMemoryStore) *Handler {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiSvc = svc
	h.aiLimiter = newDynamicRateLimiter()
	h.settings = &fakeSettingsService{}
	h.aiPrefs = prefs
	h.aiMemory = mem
	return h
}

func newMemoryTestContext(user uuid.UUID) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(auth.UserIDContextKey, user)
	return c
}

func setMemoryAuto(t *testing.T, store *fakeAIPrefsStore, user uuid.UUID, on bool) {
	t.Helper()
	if err := store.SetAIPrefs(user, json.RawMessage(`{"memory_auto":`+boolStr(on)+`}`)); err != nil {
		t.Fatalf("set prefs: %v", err)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// waitUntil 轮询等待异步条件成立（后台 goroutine 完成提取）。
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------- 门控（偏好关闭/未配置 → 不调 AI） ----------

// TestMaybeAutoExtractMemoryGateOff 偏好关闭 / 无 prefs 记录 / 本轮无实质
// 对话（assistantMsg 空）→ 均不发起 AI 调用（计数 0）、不落库。
func TestMaybeAutoExtractMemoryGateOff(t *testing.T) {
	user := uuid.New()
	for name, tc := range map[string]struct {
		prefs       string // 空 = 不写 prefs 记录
		userMsg     string
		assistantMS string
	}{
		"偏好关闭":    {prefs: `{"memory_auto":false}`, userMsg: "你好", assistantMS: "你好！"},
		"无 prefs": {prefs: "", userMsg: "你好", assistantMS: "你好！"},
		"助手回复为空":  {prefs: `{"memory_auto":true}`, userMsg: "你好", assistantMS: "  "},
	} {
		up := newMemoryUpstream(t, "", `["新偏好"]`)
		prefs := newFakeAIPrefsStore()
		if tc.prefs != "" {
			if err := prefs.SetAIPrefs(user, json.RawMessage(tc.prefs)); err != nil {
				t.Fatalf("set prefs: %v", err)
			}
		}
		mem := &fakeAutoMemoryStore{}
		h := newAutoMemoryHandler(up.service(), prefs, mem)
		h.maybeAutoExtractMemory(newMemoryTestContext(user), tc.userMsg, tc.assistantMS)
		time.Sleep(150 * time.Millisecond) // 门控同步判定：通过则本地假上游毫秒级完成
		if got := up.callCount(); got != 0 {
			t.Fatalf("%s: 不应调用 AI, calls=%d", name, got)
		}
		if mem.autoCount() != 0 {
			t.Fatalf("%s: 不应落库, items=%v", name, mem.items)
		}
	}
}

// TestAutoExtractMemoryServiceDisabled 服务未装配 / AI 关闭（无可用
// Provider）→ 提取链路静默返回（不 panic、不落库）。
func TestAutoExtractMemoryServiceDisabled(t *testing.T) {
	mem := &fakeAutoMemoryStore{}
	// 未装配（aiSvc=nil）。
	h := newAutoMemoryHandler(nil, newFakeAIPrefsStore(), mem)
	h.autoExtractAIMemory(context.Background(), mem, uuid.New(), "你好", "你好！")
	// AI 关闭：EnabledNow=false。
	off := ai.NewService(func() (settings.AIConfig, error) { return settings.DefaultAIConfig(), nil })
	h2 := newAutoMemoryHandler(off, newFakeAIPrefsStore(), mem)
	h2.autoExtractAIMemory(context.Background(), mem, uuid.New(), "你好", "你好！")
	if mem.autoCount() != 0 {
		t.Fatalf("不应落库: %v", mem.items)
	}
}

// ---------- 落库 / 裁剪 / 去重 / 非 JSON ----------

// TestAutoExtractMemoryWritesItems AI 返回 JSON 数组（可带前后杂讯文本）→
// 逐条 TrimSpace 落 kind=auto；>3 条硬截断；>200 rune 硬截断。
func TestAutoExtractMemoryWritesItems(t *testing.T) {
	up := newMemoryUpstream(t, "", `提取结果如下：\n["  用户偏好中文回答  ", "`+strings.Repeat("长", 250)+`", "第三条", "第四条"]\n请查收`)
	mem := &fakeAutoMemoryStore{}
	h := newAutoMemoryHandler(up.service(), newFakeAIPrefsStore(), mem)
	h.autoExtractAIMemory(context.Background(), mem, uuid.New(), "以后请用中文回答", "好的")
	if got := mem.autoCount(); got != aiMemoryExtractMaxItems {
		t.Fatalf("应至多落 %d 条, got %d", aiMemoryExtractMaxItems, got)
	}
	if it := mem.find(auth.AIMemoryKindAuto, "用户偏好中文回答"); it == nil {
		t.Fatalf("应 TrimSpace 后落库: %v", mem.items)
	}
	// 超长条目截 200 rune。
	var clipped string
	for _, it := range mem.items {
		if strings.HasPrefix(it.Content, "长") {
			clipped = it.Content
		}
	}
	if clipped == "" || len([]rune(clipped)) != aiMemoryExtractMaxRunes {
		t.Fatalf("超长条目应截 %d rune: %q", aiMemoryExtractMaxRunes, clipped)
	}
}

// TestAutoExtractMemoryTrim 超限裁剪：既有 AIMemoryAutoMax 条 auto + 1 条
// manual，本轮新增 1 条 → auto 恰保留最新 100 条（最旧 1 条被裁）、manual
// 不动、新条目在库；未超限（2 条 auto + 1 manual，新增 1）→ 不裁剪。
func TestAutoExtractMemoryTrim(t *testing.T) {
	// 场景 1：超限 → 裁最旧 auto。
	up := newMemoryUpstream(t, "", `["新偏好"]`)
	mem := &fakeAutoMemoryStore{}
	mem.items = append(mem.items, auth.AIMemory{ID: uuid.New(), Kind: auth.AIMemoryKindManual, Content: "手动记忆"})
	oldest := auth.AIMemory{ID: uuid.New(), Kind: auth.AIMemoryKindAuto, Content: "最旧自动记忆"}
	mem.items = append(mem.items, oldest) // items 末尾 = 最旧
	for i := 0; i < auth.AIMemoryAutoMax-1; i++ {
		mem.items = append([]auth.AIMemory{{ID: uuid.New(), Kind: auth.AIMemoryKindAuto, Content: "自动记忆"}}, mem.items...)
	}
	h := newAutoMemoryHandler(up.service(), newFakeAIPrefsStore(), mem)
	h.autoExtractAIMemory(context.Background(), mem, uuid.New(), "你好", "你好！")
	if got := mem.autoCount(); got != auth.AIMemoryAutoMax {
		t.Fatalf("auto 应裁剪至 %d 条, got %d", auth.AIMemoryAutoMax, got)
	}
	if len(mem.autoTrimmed) != 1 || mem.autoTrimmed[0].ID != oldest.ID {
		t.Fatalf("应只裁最旧 auto: trimmed=%v", mem.autoTrimmed)
	}
	if mem.find(auth.AIMemoryKindManual, "手动记忆") == nil {
		t.Fatal("manual 不应被裁剪")
	}
	if mem.find(auth.AIMemoryKindAuto, "新偏好") == nil {
		t.Fatal("新增条目应在库")
	}

	// 场景 2：未超限 → 不裁剪。
	up2 := newMemoryUpstream(t, "", `["新偏好"]`)
	mem2 := &fakeAutoMemoryStore{}
	mem2.items = []auth.AIMemory{
		{ID: uuid.New(), Kind: auth.AIMemoryKindAuto, Content: "自动1"},
		{ID: uuid.New(), Kind: auth.AIMemoryKindManual, Content: "手动"},
		{ID: uuid.New(), Kind: auth.AIMemoryKindAuto, Content: "自动2"},
	}
	h2 := newAutoMemoryHandler(up2.service(), newFakeAIPrefsStore(), mem2)
	h2.autoExtractAIMemory(context.Background(), mem2, uuid.New(), "你好", "你好！")
	if got := mem2.autoCount(); got != 3 {
		t.Fatalf("未超限不应裁剪, auto=%d", got)
	}
	if len(mem2.autoTrimmed) != 0 {
		t.Fatalf("未超限不应有裁剪: %v", mem2.autoTrimmed)
	}
	if mem2.find(auth.AIMemoryKindManual, "手动") == nil {
		t.Fatal("manual 应保持不动")
	}
}

// TestAutoExtractMemoryDedup 重复内容去重：与既有记忆（manual/auto）内容
// 完全相同的条目跳过；同批内重复同样只落一条。
func TestAutoExtractMemoryDedup(t *testing.T) {
	up := newMemoryUpstream(t, "", `["偏好简洁中文回答","偏好简洁中文回答","新偏好"]`)
	mem := &fakeAutoMemoryStore{}
	mem.items = []auth.AIMemory{
		{ID: uuid.New(), Kind: auth.AIMemoryKindManual, Content: "偏好简洁中文回答"},
	}
	h := newAutoMemoryHandler(up.service(), newFakeAIPrefsStore(), mem)
	h.autoExtractAIMemory(context.Background(), mem, uuid.New(), "你好", "你好！")
	if got := mem.autoCount(); got != 1 {
		t.Fatalf("重复内容应去重, auto=%d items=%v", got, mem.items)
	}
	if mem.find(auth.AIMemoryKindAuto, "新偏好") == nil {
		t.Fatalf("非重复条目应落库: %v", mem.items)
	}
	if mem.find(auth.AIMemoryKindAuto, "偏好简洁中文回答") != nil {
		t.Fatalf("与 manual 重复的内容不应再落 auto: %v", mem.items)
	}
}

// TestAutoExtractMemoryNonJSON AI 返回非 JSON / 括号内非法 JSON / 空数组 →
// 静默不落库、不 panic。
func TestAutoExtractMemoryNonJSON(t *testing.T) {
	for name, reply := range map[string]string{
		"纯文本无括号": "抱歉，我无法从这段对话中提取偏好",
		"括号内非法":  "结果如下 [a, b] 请查收",
		"混合类型数组": `["x", 3]`,
		"空数组":    `[]`,
	} {
		up := newMemoryUpstream(t, "", reply)
		mem := &fakeAutoMemoryStore{}
		h := newAutoMemoryHandler(up.service(), newFakeAIPrefsStore(), mem)
		h.autoExtractAIMemory(context.Background(), mem, uuid.New(), "你好", "你好！") // 不 panic 即通过一半
		if mem.autoCount() != 0 {
			t.Fatalf("%s: 不应落库, items=%v", name, mem.items)
		}
		if up.callCount() != 1 {
			t.Fatalf("%s: 恰一次模型调用, calls=%d", name, up.callCount())
		}
	}
}

// ---------- maybeAutoExtractMemory 全链路（后台 goroutine） ----------

// TestMaybeAutoExtractMemoryAsync 偏好开启 + 实质对话 → 后台 goroutine 完成
// 提取落库（AI 恰调用一次）。
func TestMaybeAutoExtractMemoryAsync(t *testing.T) {
	up := newMemoryUpstream(t, "", `["用户偏好中文回答"]`)
	user := uuid.New()
	prefs := newFakeAIPrefsStore()
	setMemoryAuto(t, prefs, user, true)
	mem := &fakeAutoMemoryStore{}
	h := newAutoMemoryHandler(up.service(), prefs, mem)
	h.maybeAutoExtractMemory(newMemoryTestContext(user), "以后请用中文回答", "好的，已记下")
	waitUntil(t, 3*time.Second, func() bool { return mem.autoCount() == 1 })
	if it := mem.find(auth.AIMemoryKindAuto, "用户偏好中文回答"); it == nil {
		t.Fatalf("后台应落 auto 条目: %v", mem.items)
	}
	if got := up.callCount(); got != 1 {
		t.Fatalf("应恰一次模型调用, calls=%d", got)
	}
}

// ---------- aiChat 完成点接入 e2e ----------

// newAutoMemoryChatRouter 组装 aiChat 完成点接入测试路由（第 1 次上游调用
// = 对话轮回复 chatReply，其后 = 提取轮回复 extractReply）。
func newAutoMemoryChatRouter(up *memoryUpstream, prefs *fakeAIPrefsStore, mem *fakeAutoMemoryStore, actor uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiSvc = up.service()
	h.aiLimiter = newDynamicRateLimiter()
	h.settings = &fakeSettingsService{}
	h.aiPrefs = prefs
	h.aiMemory = mem
	r := gin.New()
	r.POST("/api/v1/ai/chat", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, actor)
		h.aiChat(c)
	})
	return r
}

// TestAIChatAutoMemoryE2ENonStream 非流式对话完成点：响应 200 返回后，
// 后台提取落 auto 条目（memory_auto 开启）。
func TestAIChatAutoMemoryE2ENonStream(t *testing.T) {
	up := newMemoryUpstream(t, "好的，没问题", `["用户偏好中文回答"]`)
	user := uuid.New()
	prefs := newFakeAIPrefsStore()
	setMemoryAuto(t, prefs, user, true)
	mem := &fakeAutoMemoryStore{}
	r := newAutoMemoryChatRouter(up, prefs, mem, user)
	w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"messages":[{"role":"user","content":"以后请用中文回答"}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "好的，没问题") {
		t.Fatalf("对话主流程不受影响: %d %s", w.Code, w.Body.String())
	}
	waitUntil(t, 3*time.Second, func() bool { return mem.autoCount() == 1 })
	if it := mem.find(auth.AIMemoryKindAuto, "用户偏好中文回答"); it == nil {
		t.Fatalf("完成点应触发后台提取: %v", mem.items)
	}
}

// TestAIChatAutoMemoryE2EStream 流式对话（SSE done 事件）完成点：同样触发
// 后台提取；偏好关闭时不触发（第 2 次上游调用不发生）。
func TestAIChatAutoMemoryE2EStream(t *testing.T) {
	up := newMemoryUpstream(t, "好的", `["用户偏好中文回答"]`)
	user := uuid.New()
	prefs := newFakeAIPrefsStore()
	setMemoryAuto(t, prefs, user, true)
	mem := &fakeAutoMemoryStore{}
	r := newAutoMemoryChatRouter(up, prefs, mem, user)
	w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"以后请用中文回答"}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "event: done") {
		t.Fatalf("流式主流程不受影响: %d %s", w.Code, w.Body.String())
	}
	waitUntil(t, 3*time.Second, func() bool { return mem.autoCount() == 1 })
	if it := mem.find(auth.AIMemoryKindAuto, "用户偏好中文回答"); it == nil {
		t.Fatalf("done 完成点应触发后台提取: %v", mem.items)
	}

	// 偏好关闭：对话轮之外无第 2 次上游调用、无落库。
	up2 := newMemoryUpstream(t, "好的", `["用户偏好中文回答"]`)
	prefs2 := newFakeAIPrefsStore()
	setMemoryAuto(t, prefs2, user, false)
	mem2 := &fakeAutoMemoryStore{}
	r2 := newAutoMemoryChatRouter(up2, prefs2, mem2, user)
	if w2 := postAI(r2, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"以后请用中文回答"}]}`); w2.Code != http.StatusOK {
		t.Fatalf("对话应正常: %d %s", w2.Code, w2.Body.String())
	}
	time.Sleep(150 * time.Millisecond)
	if got := up2.callCount(); got != 1 {
		t.Fatalf("偏好关闭应只有对话轮一次调用, calls=%d", got)
	}
	if mem2.autoCount() != 0 {
		t.Fatalf("偏好关闭不应落库: %v", mem2.items)
	}
}
