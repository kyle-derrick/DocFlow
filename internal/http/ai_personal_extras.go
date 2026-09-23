// Package http —— ai_personal_extras.go：个人 AI 配置扩展端点（个人技能
// / 个人 MCP 服务，user_ai_prefs 的 skills 与 mcp_servers 子集）。
//
// GET/PUT /api/v1/ai/personal/skills：个人技能/快捷指令模板整表读写（≤50，
// 校验同 ValidateAIPersonalPrefs 的 skills 分支）。技能不注入 system 提示
// ——前端在对话入口的「技能」弹层合并展示平台+个人技能，选中把 prompt
// 填入输入框（占位符 {selection}/{file} 由前端替换）。
//
// GET/PUT /api/v1/ai/personal/mcp-servers：个人 MCP 服务整表读写（≤8）。
// auth_headers 只写不读（语义同 providers api_key / 平台 ai.mcp 的
// auth_header）：PUT 留空 = 按 ID+键继承现值，读路径仅回显键名列表与
// auth_headers_configured 布尔。对话开启 use_mcp 时个人服务与平台服务
// 合并加载（仅本人对话生效，见 internal/ai/chat.go 的 mcpServices）。
//
// POST /api/v1/ai/personal/mcp-servers/test：用给定参数（支持草稿）实测
// 个人 MCP 服务器连通性（实现与平台 /admin/settings/ai/mcp/test 共用，
// 支持 auth_headers 多头）。
package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
)

// saveAIPersonalPrefs 读-改-写个人配置：读取现值 → mutate 原地修改 → 整块
// 校验 → 写回。专用子集端点（skills / mcp-servers）经此保证未涉及的子集
// 原样保留。校验失败返回包裹 auth.ErrInvalidAIPrefs 的错误（HTTP 层 400）。
func (h *Handler) saveAIPersonalPrefs(user uuid.UUID, mutate func(p *auth.AIPersonalPrefs)) (auth.AIPersonalPrefs, error) {
	prefs, _ := h.loadAIPersonalPrefs(user)
	mutate(&prefs)
	if err := auth.ValidateAIPersonalPrefs(prefs); err != nil {
		return auth.AIPersonalPrefs{}, err
	}
	raw, err := json.Marshal(prefs)
	if err != nil {
		return auth.AIPersonalPrefs{}, err
	}
	if err := h.aiPrefs.SetAIPrefs(user, raw); err != nil {
		return auth.AIPersonalPrefs{}, err
	}
	return prefs, nil
}

// ---------- 个人技能 ----------

// getAIPersonalSkills GET /api/v1/ai/personal/skills：本人技能列表（无密
// 纲字段，原样回显；未配置返回空数组）。
func (h *Handler) getAIPersonalSkills(c *gin.Context) {
	if h.aiPrefs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "personal ai settings are not configured"})
		return
	}
	prefs, _ := h.loadAIPersonalPrefs(userID(c))
	skills := prefs.Skills
	if skills == nil {
		skills = []auth.AIPersonalSkill{}
	}
	c.JSON(http.StatusOK, gin.H{"skills": skills})
}

// putAIPersonalSkills PUT /api/v1/ai/personal/skills {skills:[...]}：整表
// 替换本人技能（其余子集原样保留）；校验失败 400 INVALID_AI_PREFS（error
// 含字段定位）。
func (h *Handler) putAIPersonalSkills(c *gin.Context) {
	if h.aiPrefs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "personal ai settings are not configured"})
		return
	}
	var req struct {
		Skills []auth.AIPersonalSkill `json:"skills"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request", "code": "INVALID_AI_PREFS"})
		return
	}
	saved, err := h.saveAIPersonalPrefs(userID(c), func(p *auth.AIPersonalPrefs) {
		p.Skills = req.Skills
	})
	if err != nil {
		respondAIPersonalError(c, err)
		return
	}
	skills := saved.Skills
	if skills == nil {
		skills = []auth.AIPersonalSkill{}
	}
	c.JSON(http.StatusOK, gin.H{"skills": skills})
}

// ---------- 个人 MCP 服务 ----------

// getAIPersonalMCPServers GET /api/v1/ai/personal/mcp-servers：本人 MCP 服务
// 列表（掩码视图：认证头值绝不回显，仅键名列表 + configured 布尔）。
func (h *Handler) getAIPersonalMCPServers(c *gin.Context) {
	if h.aiPrefs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "personal ai settings are not configured"})
		return
	}
	prefs, _ := h.loadAIPersonalPrefs(userID(c))
	c.JSON(http.StatusOK, gin.H{"servers": prefs.Masked().MCPServers})
}

// putAIPersonalMCPServers PUT /api/v1/ai/personal/mcp-servers {servers:[...]}：
// 整表替换本人 MCP 服务（其余子集原样保留）。auth_headers 值留空 = 按
// ID+键继承现值（掩码回读视图的天然形态）；校验失败 400
// INVALID_AI_PREFS。成功返回掩码视图（认证头值不回显）。
func (h *Handler) putAIPersonalMCPServers(c *gin.Context) {
	if h.aiPrefs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "personal ai settings are not configured"})
		return
	}
	var req struct {
		Servers []auth.AIPersonalMCPServer `json:"servers"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request", "code": "INVALID_AI_PREFS"})
		return
	}
	user := userID(c)
	saved, err := h.saveAIPersonalPrefs(user, func(p *auth.AIPersonalPrefs) {
		auth.InheritAIPersonalMCPAuthHeaders(req.Servers, p.MCPServers)
		p.MCPServers = req.Servers
	})
	if err != nil {
		respondAIPersonalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"servers": saved.Masked().MCPServers})
}

// respondAIPersonalError 个人配置写路径的统一错误分流：ErrInvalidAIPrefs
// → 400（INVALID_AI_PREFS，error 含字段定位），其余 500。
func respondAIPersonalError(c *gin.Context, err error) {
	if errors.Is(err, auth.ErrInvalidAIPrefs) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_AI_PREFS"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save personal ai settings"})
}

// ---------- 个人 MCP 测试连接（与平台 /admin/settings/ai/mcp/test 共用实现） ----------

// testAIPersonalMCP POST /api/v1/ai/personal/mcp-servers/test
// {url, auth_headers?}：用给定参数（支持未保存的草稿）实测个人 MCP 服务
// 器连通性（initialize + tools/list，返回工具数/工具名/延迟）。auth_headers
// 为多认证头集合（键 = header 名，值 = 凭据）；HTTP 恒 200，连接失败的
// 信息在 body（ok=false + error）。
func (h *Handler) testAIPersonalMCP(c *gin.Context) {
	var req struct {
		URL         string            `json:"url"`
		AuthHeaders map[string]string `json:"auth_headers"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	aiMCPTestRespond(c, req.URL, "", req.AuthHeaders)
}
