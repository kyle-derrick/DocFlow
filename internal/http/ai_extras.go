// Package http —— ai_extras.go：AI 配置体系补全端点（平台人设 / 技能
// 模板 / 用户记忆）。
//
// 平台人设与技能（system_settings 的 ai.personas / ai.skills 键，管理员
// 维护、EffectImmediate）：登录用户经 GET /ai/personas|/ai/skills 读取
// （空配置返回空数组，不随 AI 总开关 404——人设/技能是配置数据而非对话
// 能力）；管理端 GET/PUT /admin/settings/ai/personas|/ai/skills 整块读替
// （校验见 settings 包：≤50 条、id 唯一、name ≤64、prompt ≤4000）。
//
// 用户记忆（ai_memory 表，migration 050，本人维度、仅手动维护——自动
// 提取落库 v1 不做）：GET/POST/PUT/DELETE /ai/memory（PUT 编辑 content、
// kind 不变）；/ai/chat 的 include_memory=true 时取最近 20 条拼入 system
// 上下文（总量按 aiMemoryContextMaxRunes 截断，「以下是用户的长期偏好
// 记忆…」，见 aiChat 的 aiMemoryContextBlock）。
//
// 外部 MCP 服务器（system_settings 的 ai.mcp 键）：登录用户经 GET
// /ai/mcp 读启用服务（仅 id/name，不泄 URL/凭据——前端 use_mcp 开关
// 展示）；管理端 GET/PUT /admin/settings/ai/mcp 整块读替（auth_header
// 只写不读：留空 = 按 ID 继承现值，回显 auth_header_configured 布尔）；
// POST /admin/settings/ai/mcp/test 按给定参数实测连通性（支持草稿）。
package http

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/mcpclient"
	"github.com/docflow/docflow/internal/settings"
)

// ---------- 平台人设 / 技能模板 ----------

// aiPersonasList GET /api/v1/ai/personas（登录可用）：平台人设数组（热
// 读取；未装配 settings 服务或读取失败返回空数组——配置数据 best-effort，
// 不阻塞前端人设下拉）。
func (h *Handler) aiPersonasList(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"personas": h.platformPersonas()})
}

func (h *Handler) platformPersonas() []settings.AIPersonaDef {
	if h.settings == nil {
		return []settings.AIPersonaDef{}
	}
	list, err := h.settings.AIPersonas()
	if err != nil || list == nil {
		return []settings.AIPersonaDef{}
	}
	return list
}

// aiSkillsList GET /api/v1/ai/skills（登录可用）：平台技能数组（语义同
// personas；数组顺序即展示顺序）。
func (h *Handler) aiSkillsList(c *gin.Context) {
	// 登录侧仅返回启用技能（停用项管理端可见，用户侧隐藏）。
	c.JSON(http.StatusOK, gin.H{"skills": h.platformSkills()})
}

func (h *Handler) platformSkills() []settings.AISkillDef {
	if h.settings == nil {
		return []settings.AISkillDef{}
	}
	list, err := h.settings.AISkills()
	if err != nil || list == nil {
		return []settings.AISkillDef{}
	}
	enabled := make([]settings.AISkillDef, 0, len(list))
	for _, s := range list {
		if s.Enabled == nil || *s.Enabled {
			enabled = append(enabled, s)
		}
	}
	return enabled
}

// adminAISkillsList GET /api/v1/admin/settings/ai/skills：管理端全量列表
// （含停用项，enabled 字段透传）。
func (h *Handler) adminAISkillsList(c *gin.Context) {
	if h.settings == nil {
		c.JSON(http.StatusOK, gin.H{"skills": []settings.AISkillDef{}})
		return
	}
	list, err := h.settings.AISkills()
	if err != nil || list == nil {
		c.JSON(http.StatusOK, gin.H{"skills": []settings.AISkillDef{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"skills": list})
}

// adminPutAIPersonas PUT /api/v1/admin/settings/ai/personas：整块替换平台
// 人设（校验失败 400 INVALID_AI_PERSONAS，error 含字段定位）。
func (h *Handler) adminPutAIPersonas(c *gin.Context) {
	if h.settings == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings service is not configured"})
		return
	}
	var req struct {
		Personas []settings.AIPersonaDef `json:"personas"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	list, err := h.settings.SetAIPersonas(req.Personas, userID(c))
	if err != nil {
		if errors.Is(err, settings.ErrInvalidValue) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_AI_PERSONAS"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save ai personas"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"personas": list})
}

// adminPutAISkills PUT /api/v1/admin/settings/ai/skills：整块替换平台技能
// （校验失败 400 INVALID_AI_SKILLS）。
func (h *Handler) adminPutAISkills(c *gin.Context) {
	if h.settings == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings service is not configured"})
		return
	}
	var req struct {
		Skills []settings.AISkillDef `json:"skills"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	list, err := h.settings.SetAISkills(req.Skills, userID(c))
	if err != nil {
		if errors.Is(err, settings.ErrInvalidValue) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_AI_SKILLS"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save ai skills"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"skills": list})
}

// ---------- 用户记忆（ai_memory，本人维度） ----------

// aiMemoryContextLimit include_memory 注入时取的最近记忆条数。
const aiMemoryContextLimit = 20

// aiMemoryContextMaxRunes 注入上下文的总字数上限（rune 计）。记忆只增不
// 减（单条 ≤2000、最多取 20 条，理论上限 4 万字符），不设总量上限会随
// 条数膨胀撑爆 system 上下文、挤占对话本身的预算，故拼装时按总量截断
// （最近记忆优先，见 aiMemoryContextBlock）。
const aiMemoryContextMaxRunes = 6000

// aiMemoryStore 抽象记忆存取（生产实现 *auth.UserStore，NewHandler 装配；
// 接口化便于单测注入内存实现，模式同 aiPrefsStore）。
type aiMemoryStore interface {
	ListAIMemory(userID uuid.UUID, limit int) ([]auth.AIMemory, error)
	CreateAIMemory(userID uuid.UUID, kind, content string) (auth.AIMemory, error)
	UpdateAIMemory(userID, id uuid.UUID, content string) (auth.AIMemory, bool, error)
	DeleteAIMemory(userID, id uuid.UUID) (bool, error)
}

var _ aiMemoryStore = (*auth.UserStore)(nil)

// aiMemoryContextBlock 拼装记忆上下文块（include_memory 开启时经
// appendContextSystem 并入首条 system 消息）：取最近 20 条，按时间正序
// 阅读更自然；注入总量按 aiMemoryContextMaxRunes 截断——从最新一条开始
// 累计，加入某条会超上限则丢弃该条及更旧条目（最近记忆优先注入；至少
// 保底注入最新 1 条，单条本身限 2000 不会超上限）。未装配存储 / 读取
// 失败 / 无记忆返回空串（静默降级，不阻塞对话）。
func (h *Handler) aiMemoryContextBlock(user uuid.UUID) string {
	if h.aiMemory == nil {
		return ""
	}
	items, err := h.aiMemory.ListAIMemory(user, aiMemoryContextLimit)
	if err != nil || len(items) == 0 {
		return ""
	}
	kept := make([]auth.AIMemory, 0, len(items))
	total := 0
	for _, it := range items { // List 为倒序：最新在前，故从第 0 条开始累计
		n := len([]rune(it.Content))
		if len(kept) > 0 && total+n > aiMemoryContextMaxRunes {
			break // 该条及更旧条目均丢弃，防止记忆膨胀撑爆 system 上下文
		}
		kept = append(kept, it)
		total += n
	}
	var b strings.Builder
	b.WriteString("以下是用户的长期偏好记忆（用户本人手动维护，回答时请结合这些偏好）：")
	for i := len(kept) - 1; i >= 0; i-- { // 注入反转为时间正序
		b.WriteString("\n- " + kept[i].Content)
	}
	return b.String()
}

// aiMemoryList GET /api/v1/ai/memory：本人记忆列表（最新在前，≤200 条）。
func (h *Handler) aiMemoryList(c *gin.Context) {
	if h.aiMemory == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai memory is not configured"})
		return
	}
	items, err := h.aiMemory.ListAIMemory(userID(c), 200)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load ai memory"})
		return
	}
	if items == nil {
		items = []auth.AIMemory{}
	}
	c.JSON(http.StatusOK, gin.H{"memories": items})
}

// aiMemoryCreate POST /api/v1/ai/memory {content, kind?}：新增一条手动
// 记忆（kind 缺省 manual；auto 为服务端自动提取专用值，API 拒绝——
// 内部写入走 auth.UserStore.CreateAutoAIMemory，见 ai_memory_auto.go）。
func (h *Handler) aiMemoryCreate(c *gin.Context) {
	if h.aiMemory == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai memory is not configured"})
		return
	}
	var req struct {
		Content string `json:"content"`
		Kind    string `json:"kind"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	item, err := h.aiMemory.CreateAIMemory(userID(c), strings.TrimSpace(req.Kind), req.Content)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidAIMemory) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_AI_MEMORY"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save ai memory"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"memory": item})
}

// aiMemoryDelete DELETE /api/v1/ai/memory/:id：删除本人一条记忆；不存在
// /非本人 404（不区分，避免越权探测）。
func (h *Handler) aiMemoryDelete(c *gin.Context) {
	if h.aiMemory == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai memory is not configured"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	found, err := h.aiMemory.DeleteAIMemory(userID(c), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to delete ai memory"})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "ai memory not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// aiMemoryUpdate PUT /api/v1/ai/memory/:id {content}：编辑本人一条记忆的
// content（kind 不变）。不存在/非本人 404（不区分，避免越权探测——语义
// 同 DELETE）；校验失败 400 INVALID_AI_MEMORY（语义同 POST）。
func (h *Handler) aiMemoryUpdate(c *gin.Context) {
	if h.aiMemory == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai memory is not configured"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	item, found, err := h.aiMemory.UpdateAIMemory(userID(c), id, req.Content)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidAIMemory) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_AI_MEMORY"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to update ai memory"})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "ai memory not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"memory": item})
}

// ---------- 外部 MCP 服务器（ai.mcp） ----------

// mcpSettingsStore 为 ai.mcp 键的读写切片（*settings.Store 天然满足）。
// 刻意不并入 admin.go 的 settingsService 接口：以类型断言解耦，旧测试
// fake 不实现这两个方法时端点按「未配置」降级（空数组/503）。
type mcpSettingsStore interface {
	AIMCPServices() ([]settings.AIMCPServiceDef, error)
	SetAIMCPServices(list []settings.AIMCPServiceDef, actor uuid.UUID) ([]settings.AIMCPServiceDef, error)
}

// h.mcpStore 返回支持 ai.mcp 读写的 settings 服务；未装配或不支持返回
// nil（GET /ai/mcp 降级空数组）。
func (h *Handler) mcpStore() mcpSettingsStore {
	if h.settings == nil {
		return nil
	}
	store, ok := h.settings.(mcpSettingsStore)
	if !ok {
		return nil
	}
	return store
}

// aiMCPList GET /api/v1/ai/mcp（登录可用）：启用中的 MCP 服务 [{id,name}]
// （热读取；不泄 URL/auth_header——前端 use_mcp 开关展示用。配置数据
// best-effort：未装配/读取失败返回空数组，不随 AI 总开关 404）。
func (h *Handler) aiMCPList(c *gin.Context) {
	services := []gin.H{}
	store := h.mcpStore()
	if store != nil {
		if list, err := store.AIMCPServices(); err == nil {
			for _, svc := range list {
				if svc.Enabled {
					services = append(services, gin.H{"id": svc.ID, "name": svc.Name})
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"services": services})
}

// aiMCPServiceView 管理端 MCP 服务视图：auth_header 只写不读，仅回显
// configured 布尔（照 search.tavily 的掩码模式）。
type aiMCPServiceView struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	URL                  string `json:"url"`
	Enabled              bool   `json:"enabled"`
	AuthHeaderConfigured bool   `json:"auth_header_configured"`
}

func aiMCPServiceViews(list []settings.AIMCPServiceDef) []aiMCPServiceView {
	out := make([]aiMCPServiceView, 0, len(list))
	for _, svc := range list {
		out = append(out, aiMCPServiceView{
			ID: svc.ID, Name: svc.Name, URL: svc.URL, Enabled: svc.Enabled,
			AuthHeaderConfigured: svc.AuthHeader != "",
		})
	}
	return out
}

// adminGetAIMCPServices GET /api/v1/admin/settings/ai/mcp：读 MCP 服务
// 列表（auth_header 掩码为 configured 布尔）。
func (h *Handler) adminGetAIMCPServices(c *gin.Context) {
	store := h.mcpStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings service is not configured"})
		return
	}
	list, err := store.AIMCPServices()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load ai mcp services"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"services": aiMCPServiceViews(list)})
}

// adminPutAIMCPServices PUT /api/v1/admin/settings/ai/mcp {services:[...]}：
// 整块替换（auth_header 留空 = 按 ID 继承现值；校验失败 400
// INVALID_AI_MCP，≤8 条、id 1..64 唯一、name 1..64、url http(s) ≤500）。
func (h *Handler) adminPutAIMCPServices(c *gin.Context) {
	store := h.mcpStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings service is not configured"})
		return
	}
	var req struct {
		Services []settings.AIMCPServiceDef `json:"services"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	list, err := store.SetAIMCPServices(req.Services, userID(c))
	if err != nil {
		if errors.Is(err, settings.ErrInvalidValue) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_AI_MCP"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save ai mcp services"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"services": aiMCPServiceViews(list)})
}

// aiMCPTestTimeout 连接测试总超时（覆盖 initialize → initialized →
// tools/list 全程，单请求仍受 mcpclient 的 8s 超时约束）。
const aiMCPTestTimeout = 10 * time.Second

// aiMCPTestNameLimit 响应回显的工具名上限（只看前若干个即可判断连通与
// 配置是否符合预期，避免超大工具列表撑爆响应）。
const aiMCPTestNameLimit = 10

// adminTestAIMCP POST /api/v1/admin/settings/ai/mcp/test {url, auth_header?}：
// 用给定参数（支持测试未保存的草稿配置）实测 MCP 服务器连通性（共用实现
// 见 aiMCPTestRespond；个人侧 /ai/personal/mcp-servers/test 同源）。
func (h *Handler) adminTestAIMCP(c *gin.Context) {
	var req struct {
		URL        string `json:"url"`
		AuthHeader string `json:"auth_header"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	aiMCPTestRespond(c, req.URL, req.AuthHeader, nil)
}

// aiMCPTestRespond MCP 连通性实测的共用实现（平台单 auth_header 与个人
// auth_headers 多头共用）：initialize + tools/list，返回工具数与前若干
// 工具名与往返延迟。HTTP 恒 200，连接失败的信息在 body（ok=false +
// error，中文友好，来自 mcpclient）。
func aiMCPTestRespond(c *gin.Context, url, authHeader string, headers map[string]string) {
	url = strings.TrimSpace(url)
	if url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), aiMCPTestTimeout)
	defer cancel()
	start := time.Now()
	tools, err := (&mcpclient.Client{URL: url, AuthHeader: strings.TrimSpace(authHeader), Headers: headers}).ListTools(ctx)
	// 实测必然有耗时；本地环回可能整段 <1ms，下限取 1 保证延迟语义非零。
	latency := max(int64(1), time.Since(start).Milliseconds())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"ok": false, "tools": 0, "names": []string{},
			"latency_ms": latency, "error": err.Error(),
		})
		return
	}
	names := make([]string, 0, min(len(tools), aiMCPTestNameLimit))
	for _, t := range tools {
		if len(names) >= aiMCPTestNameLimit {
			break
		}
		names = append(names, t.Name)
	}
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "tools": len(tools), "names": names, "latency_ms": latency,
	})
}
