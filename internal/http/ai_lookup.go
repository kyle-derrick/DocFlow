// ai_lookup.go：模型能力自动识别（Cherry Studio 语义）。
//
// GET /api/v1/admin/settings/ai/models/lookup?model=<模型id>&provider=<provider名或kind>
//（登录侧别名 GET /api/v1/ai/models/lookup——个人池模型编辑复用，models.dev
// 为公开元数据、无敏感信息）：服务端查询 https://models.dev/api.json
//（开源 LLM 元数据目录，含各模型类型与能力标记）匹配模型 id（大小写与
// -_.:/ 等分隔符宽松匹配），返回类型（chat/embedding/rerank/image 互斥）
// 与能力并集（reasoning/vision/audio/video）。上游 5s 超时、内存缓存
// 10 分钟（失败保留旧快照降级）；未命中/网络失败一律 found:false
//（前端静默回退手动选择）。端点不依赖 AI 总开关——管理员正在配置
// Provider（AI 尚未启用）时恰是识别最需要的时刻。
package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/docflow/docflow/internal/settings"
)

const (
	// modelsDevCacheTTL 内存快照有效期（静态目录变化缓慢，10 分钟足够）。
	modelsDevCacheTTL = 10 * time.Minute
	// modelsDevFetchLimit 上游响应读取上限（防御异常超大响应）。
	modelsDevFetchLimit = 32 << 20
)

// 包级可覆盖状态（生产用默认值；同包测试覆盖 URL/超时并重置缓存）。
var (
	modelsDevMu      sync.Mutex
	modelsDevAPIURL  = "https://models.dev/api.json"
	modelsDevTimeout = 5 * time.Second
	modelsDevCache   *modelsDevSnapshot
)

// modelsDevModel 为解析归一后的目录条目（类型/能力已映射到平台语义）。
type modelsDevModel struct {
	Provider    string
	DisplayName string
	Kind        string
	Reasoning   bool
	Vision      bool
	Audio       bool
	Video       bool
}

// modelsDevSnapshot 为一次抓取的内存快照：byID 以规范化模型 id 为键
//（目录键、完整 id 与 id 尾段都入索引，冲突保留首个）；byProvider 以
// 规范化目录 slug/名称为键支持 provider 参数优先匹配。
type modelsDevSnapshot struct {
	fetched    time.Time
	byID       map[string]modelsDevModel
	byProvider map[string]map[string]modelsDevModel
}

// modelsDevAPIJSON 为 api.json 的宽松解析结构（仅取识别所需字段；未知
// 字段忽略）。模型 type 缺省为 chat；attachment 表示图片输入（视觉）。
type modelsDevAPIJSON map[string]struct {
	Name   string `json:"name"`
	Models map[string]struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Type       string `json:"type"`
		Attachment bool   `json:"attachment"`
		Reasoning  bool   `json:"reasoning"`
		Modalities struct {
			Input []string `json:"input"`
		} `json:"modalities"`
	}
}

// normalizeModelLookupKey 归一模型/provider 标识：小写并剔除 -_.:/ 与
// 空白等分隔符（"Qwen2.5-72B_Instruct" 与 "qwen2572binstruct" 等价）。
func normalizeModelLookupKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch r {
		case '-', '_', '.', ':', '/', ' ', '\t':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hasModality 报告 modalities.input 是否含指定模态（image/audio/video）。
func hasModality(list []string, want string) bool {
	for _, m := range list {
		if strings.EqualFold(strings.TrimSpace(m), want) {
			return true
		}
	}
	return false
}

// modelsDevKind 把 models.dev 的 type 归一到平台模型类型（缺省/未知
// 一律 chat，宽松容错）。
func modelsDevKind(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "embed", "embedding":
		return settings.AIModelKindEmbedding
	case "rerank":
		return settings.AIModelKindRerank
	case "image":
		return settings.AIModelKindImage
	default:
		return settings.AIModelKindChat
	}
}

// fetchModelsDev 拉取并解析 models.dev 目录（超时见 modelsDevTimeout）。
func fetchModelsDev(ctx context.Context) (*modelsDevSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsDevTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevAPIURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev status %d", resp.StatusCode)
	}
	var api modelsDevAPIJSON
	if err := json.NewDecoder(io.LimitReader(resp.Body, modelsDevFetchLimit)).Decode(&api); err != nil {
		return nil, err
	}
	snap := &modelsDevSnapshot{
		fetched:    time.Now().UTC(),
		byID:       make(map[string]modelsDevModel, 1024),
		byProvider: make(map[string]map[string]modelsDevModel, 64),
	}
	for slug, prov := range api {
		for key, m := range prov.Models {
			entry := modelsDevModel{
				Provider:    slug,
				DisplayName: m.Name,
				Kind:        modelsDevKind(m.Type),
				Reasoning:   m.Reasoning,
				Vision:      m.Attachment || hasModality(m.Modalities.Input, "image"),
				Audio:       hasModality(m.Modalities.Input, "audio"),
				Video:       hasModality(m.Modalities.Input, "video"),
			}
			set := func(providerScope string, id string) {
				norm := normalizeModelLookupKey(id)
				if norm == "" {
					return
				}
				if providerScope == "" {
					if _, exists := snap.byID[norm]; !exists {
						snap.byID[norm] = entry
					}
					return
				}
				bucket := snap.byProvider[providerScope]
				if bucket == nil {
					bucket = make(map[string]modelsDevModel, 16)
					snap.byProvider[providerScope] = bucket
				}
				if _, exists := bucket[norm]; !exists {
					bucket[norm] = entry
				}
			}
			set("", key)
			set(normalizeModelLookupKey(slug), key)
			if prov.Name != "" {
				set(normalizeModelLookupKey(prov.Name), key)
			}
			set("", m.ID)
			// 完整 id 带目录前缀（如 "deepseek/deepseek-chat"）时尾段
			// 单独入索引，支持只粘贴尾段模型 id 的常见输入。
			if i := strings.LastIndex(m.ID, "/"); i >= 0 && i+1 < len(m.ID) {
				set("", m.ID[i+1:])
			}
		}
	}
	return snap, nil
}

// currentModelsDev 返回缓存快照（TTL 内直接复用；过期重抓失败时保留
// 旧快照降级，首次失败无缓存返回错误）。
func currentModelsDev(ctx context.Context) (*modelsDevSnapshot, error) {
	modelsDevMu.Lock()
	defer modelsDevMu.Unlock()
	if modelsDevCache != nil && time.Since(modelsDevCache.fetched) < modelsDevCacheTTL {
		return modelsDevCache, nil
	}
	snap, err := fetchModelsDev(ctx)
	if err != nil {
		if modelsDevCache != nil {
			return modelsDevCache, nil
		}
		return nil, err
	}
	modelsDevCache = snap
	return snap, nil
}

// lookup 在快照中匹配模型 id：provider 参数（目录 slug 或显示名，如
// "deepseek"/"DeepSeek"）命中时优先在该目录内精确匹配，未命中回落全局
// 索引（provider 传平台 kind 如 openai_compatible 时自然跳过目录匹配）。
func (s *modelsDevSnapshot) lookup(modelID, provider string) (modelsDevModel, bool) {
	key := normalizeModelLookupKey(modelID)
	if key == "" {
		return modelsDevModel{}, false
	}
	if provider != "" {
		if pk := normalizeModelLookupKey(provider); pk != "" {
			if m, ok := s.byProvider[pk][key]; ok {
				return m, true
			}
		}
	}
	m, ok := s.byID[key]
	return m, ok
}

// aiModelLookup GET /api/v1/admin/settings/ai/models/lookup?model=&provider=
//（别名 /api/v1/ai/models/lookup）：查询 models.dev 匹配模型 id，返回
// {found, kind, reasoning, vision, audio, video, display_name?}；未命中/
// 网络失败 found:false（200，前端静默手动）。model 缺失 400。
func (h *Handler) aiModelLookup(c *gin.Context) {
	modelID := strings.TrimSpace(c.Query("model"))
	if modelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}
	snap, err := currentModelsDev(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"found": false})
		return
	}
	entry, ok := snap.lookup(modelID, strings.TrimSpace(c.Query("provider")))
	if !ok {
		c.JSON(http.StatusOK, gin.H{"found": false})
		return
	}
	resp := gin.H{
		"found":     true,
		"kind":      entry.Kind,
		"reasoning": entry.Reasoning,
		"vision":    entry.Vision,
		"audio":     entry.Audio,
		"video":     entry.Video,
	}
	if entry.DisplayName != "" {
		resp["display_name"] = entry.DisplayName
	}
	if entry.Provider != "" {
		resp["provider"] = entry.Provider
	}
	c.JSON(http.StatusOK, resp)
}
