// Package http —— ai_memory_auto.go：AI 记忆自动提取（个人偏好
// memory_auto 开关，user_ai_prefs.prefs）。
//
// 用户开启后，aiChat 每轮对话完成（响应已完整发出）即调
// maybeAutoExtractMemory：后台 goroutine 用平台默认 chat 模型从本轮
// 「用户-助手」对话中提取值得长期记住的偏好/事实，与既有记忆去重后以
// kind=auto 写入 ai_memory 并按 AIMemoryAutoMax 裁剪上限。全程静默失败
// （log 留痕，不重试），绝不影响对话主流程与响应延迟。
package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/auth"
)

// 提取链路边界。
const (
	// aiMemoryExtractTimeout 整个后台提取（AI 调用 + 落库 + 裁剪）的时长
	// 上限：超时即放弃（不重试），防慢上游拖住 goroutine。
	aiMemoryExtractTimeout = 30 * time.Second
	// aiMemoryExtractRecent 提取 prompt 中列出的既有记忆条数（去重依据，
	// 兼顾 prompt 长度上限）。
	aiMemoryExtractRecent = 50
	// aiMemoryExtractMaxItems 单轮对话最多新增的记忆条数。
	aiMemoryExtractMaxItems = 3
	// aiMemoryExtractMaxRunes 单条提取记忆长度上限（rune 计，模型输出
	// 超长硬截断）。
	aiMemoryExtractMaxRunes = 200
	// aiMemoryExtractInputMaxRunes 对话两侧文本（用户/助手）各自截断。
	aiMemoryExtractInputMaxRunes = 4000
)

// aiAutoMemoryStore 抽象自动提取链路所需的记忆存取（生产实现
// *auth.UserStore，NewHandler 装配到 h.aiMemory）。刻意不并入
// aiMemoryStore：以类型断言解耦，旧测试 fake 不实现 auto 两个方法时
// 链路按「未配置」静默跳过（模式同 mcpSettingsStore）。
type aiAutoMemoryStore interface {
	ListAIMemory(userID uuid.UUID, limit int) ([]auth.AIMemory, error)
	CreateAutoAIMemory(userID uuid.UUID, content string) (auth.AIMemory, error)
	TrimAutoAIMemory(userID uuid.UUID, keep int) error
}

var _ aiAutoMemoryStore = (*auth.UserStore)(nil)

// maybeAutoExtractMemory 对话完成后按需自动提取记忆：个人偏好
// memory_auto 开启且本轮有实质对话时，后台 goroutine 用平台默认对话
// 模型提取新增偏好（与既有记忆去重），kind=auto 写入并裁剪上限。
// 全程静默失败（log 留痕），绝不影响对话主流程/延迟（须在响应完整
// 发送之后调用）。
func (h *Handler) maybeAutoExtractMemory(c *gin.Context, userMsg, assistantMsg string) {
	// 前置门控均在请求 goroutine 内同步完成（一次轻量 prefs 读取），
	// 通过后 goroutine 不再触碰 gin.Context（防 handler 返回后池化回收）。
	store, ok := h.aiMemory.(aiAutoMemoryStore)
	if !ok || h.aiPrefs == nil {
		return
	}
	user := userID(c)
	if !h.aiMemoryAutoEnabled(user) {
		return
	}
	if strings.TrimSpace(userMsg) == "" || strings.TrimSpace(assistantMsg) == "" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ai memory auto: 提取链路 panic 已恢复: %v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), aiMemoryExtractTimeout)
		defer cancel()
		h.autoExtractAIMemory(ctx, store, user, userMsg, assistantMsg)
	}()
}

// aiMemoryAutoEnabled 读取个人偏好的 memory_auto 开关（经 aiPrefs 存储
// 现有模式直接解析原始 JSON——不走 loadAIPersonalPrefs 的 providers>0
// 约束：开关独立于个人 Provider 池，未配 Provider 也可开启）。未装配/
// 无记录/解析失败一律 false（best-effort，不阻塞对话）。
func (h *Handler) aiMemoryAutoEnabled(user uuid.UUID) bool {
	if h.aiPrefs == nil {
		return false
	}
	raw, err := h.aiPrefs.GetAIPrefs(user)
	if err != nil || len(raw) == 0 {
		return false
	}
	var prefs auth.AIPersonalPrefs
	if err := json.Unmarshal(raw, &prefs); err != nil {
		return false
	}
	return prefs.MemoryAuto
}

// autoExtractAIMemory 提取主链路（goroutine 内执行；全程静默失败）：
// 组装提取 prompt → 平台默认 chat 模型非流式调用（ForUser 记账到本人）
// → 解析 JSON 数组 → 去重落库 kind=auto → 裁剪上限。
func (h *Handler) autoExtractAIMemory(ctx context.Context, store aiAutoMemoryStore, user uuid.UUID, userMsg, assistantMsg string) {
	if h.aiSvc == nil || !h.aiSvc.EnabledNow() {
		return // 服务未装配 / AI 关闭：静默跳过
	}
	existing, err := store.ListAIMemory(user, aiMemoryExtractRecent)
	if err != nil {
		log.Printf("ai memory auto: 读取既有记忆失败: %v", err)
		return
	}
	known := make(map[string]bool, len(existing))
	var list strings.Builder
	if len(existing) == 0 {
		list.WriteString("（无）")
	}
	for _, it := range existing {
		known[it.Content] = true
		list.WriteString("\n- " + it.Content)
	}
	system := fmt.Sprintf(`你是用户长期偏好的提取助手。任务：从下面这段对话中提取值得长期记住的用户偏好或事实（例如常用语言、回答格式与详细程度偏好、技术栈与工具习惯、职业背景、称呼方式等），而不是闲聊内容或一次性问题。
用户现有记忆列表（新提取项不得与之重复）：%s
要求：只输出新增项的 JSON 字符串数组，不要输出任何其他文字；最多 %d 条，每条不超过 %d 字，以第三人称陈述用户；若无值得记录的新增项，输出 []。`, list.String(), aiMemoryExtractMaxItems, aiMemoryExtractMaxRunes)
	content := "用户：" + clipMemoryRunes(userMsg, aiMemoryExtractInputMaxRunes) +
		"\n助手：" + clipMemoryRunes(assistantMsg, aiMemoryExtractInputMaxRunes)
	res, err := h.aiSvc.ForUser(user).Chat(ctx, ai.ChatRequest{
		System:   system,
		Messages: []ai.Message{{Role: "user", Content: content}},
		Stream:   false, // ProviderID/Model 留空 = 平台默认 chat 模型
	}, nil)
	if err != nil {
		log.Printf("ai memory auto: 提取模型调用失败（不重试）: %v", err)
		return
	}
	for _, item := range parseAIMemoryItems(res.Content) {
		if known[item] {
			continue // 与既有记忆内容完全相同：跳过
		}
		if _, err := store.CreateAutoAIMemory(user, item); err != nil {
			log.Printf("ai memory auto: 写入记忆失败: %v", err)
			continue
		}
		known[item] = true // 同批内多条相同内容同样去重
	}
	if err := store.TrimAutoAIMemory(user, auth.AIMemoryAutoMax); err != nil {
		log.Printf("ai memory auto: 裁剪自动记忆失败: %v", err)
	}
}

// parseAIMemoryItems 从模型回复中提取 JSON 字符串数组：取首个 '[' 到
// 最后一个 ']' 的子串 → json.Unmarshal；逐条 TrimSpace、去空、≤200
// rune 截断、至多 3 条（模型超报硬截断）。无 JSON 数组 / 解析失败返回
// nil（调用方静默不落库）。
func parseAIMemoryItems(reply string) []string {
	start := strings.Index(reply, "[")
	end := strings.LastIndex(reply, "]")
	if start < 0 || end <= start {
		return nil
	}
	var raw []string
	if err := json.Unmarshal([]byte(reply[start:end+1]), &raw); err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if r := []rune(s); len(r) > aiMemoryExtractMaxRunes {
			s = string(r[:aiMemoryExtractMaxRunes])
		}
		out = append(out, s)
		if len(out) == aiMemoryExtractMaxItems {
			break
		}
	}
	return out
}

// clipMemoryRunes 无尾注 rune 截断（提取输入裁剪，不注入「已截断」提示
// 干扰模型）。
func clipMemoryRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

// lastUserMessage 返回消息列表中最后一条 user 消息原文（本轮用户提问；
// 无则空串——maybeAutoExtractMemory 门控据此静默跳过）。
func lastUserMessage(messages []ai.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}
