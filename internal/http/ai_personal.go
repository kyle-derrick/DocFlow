// Package http —— ai_personal.go：用户个人 AI 配置端点（双轨制）。
//
// GET/PUT /api/v1/ai/personal-settings：本人维度的 providers/default_models/
// personas/prefer_personal 整块读写（user_ai_prefs，migration 049）。api_key
// 语义同平台 Provider：PUT 留空 = 继承现值（服务端按 Provider ID 合并），
// 任何读路径以掩码视图回显（api_key_configured 布尔），绝不返回明文。
// 解析链接线见 ai.go：aiChat/aiSummarize 经 aiServiceFor 挂载个人池，
// aiModels 合并个人池模型（personal:true + 「（个人）」名后缀）。
package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/auth"
)

// aiPrefsStore 抽象个人 AI 配置存取（生产实现 *auth.UserStore，NewHandler
// 装配；接口化便于单测注入内存实现，模式同 openWithStore）。
type aiPrefsStore interface {
	GetAIPrefs(userID uuid.UUID) (json.RawMessage, error)
	SetAIPrefs(userID uuid.UUID, prefs json.RawMessage) error
}

var _ aiPrefsStore = (*auth.UserStore)(nil)

// aiPersonalFor 读取并解析请求用户的个人配置（gin 上下文版：认证中间件
// 未注入 user_id 的调用路径返回无个人池——aiModels 等端点在旧测试路由下
// 无用户上下文）。best-effort：未注入存储 / 无记录 / 解析失败一律返回
// false——个人池故障不阻塞平台 AI 链路。
func (h *Handler) aiPersonalFor(c *gin.Context) (auth.AIPersonalPrefs, bool) {
	raw, ok := c.Get(auth.UserIDContextKey)
	if !ok {
		return auth.AIPersonalPrefs{}, false
	}
	id, ok := raw.(uuid.UUID)
	if !ok {
		return auth.AIPersonalPrefs{}, false
	}
	return h.loadAIPersonalPrefs(id)
}

// loadAIPersonalPrefs 读取并解析指定用户的个人配置（best-effort：未注入
// 存储 / 无记录 / 解析失败一律返回零值与 false——个人池故障不阻塞平台 AI
// 链路）。已入库但校验不过的存量数据同样按无个人池处理（fail closed）。
func (h *Handler) loadAIPersonalPrefs(userID uuid.UUID) (auth.AIPersonalPrefs, bool) {
	if h.aiPrefs == nil {
		return auth.AIPersonalPrefs{}, false
	}
	raw, err := h.aiPrefs.GetAIPrefs(userID)
	if err != nil || len(raw) == 0 {
		return auth.AIPersonalPrefs{}, false
	}
	var prefs auth.AIPersonalPrefs
	if err := json.Unmarshal(raw, &prefs); err != nil {
		return auth.AIPersonalPrefs{}, false
	}
	if len(prefs.Providers) == 0 {
		return auth.AIPersonalPrefs{}, false
	}
	return prefs, true
}

// aiServiceFor 构造带用户身份与个人池的服务克隆（ForUser 记账 + 掩码跳过
// 的个人 prefs 挂载）；未配置个人池时与 ForUser 等价。
func (h *Handler) aiServiceFor(svc *ai.Service, userID uuid.UUID) *ai.Service {
	userSvc := svc.ForUser(userID)
	if prefs, ok := h.loadAIPersonalPrefs(userID); ok {
		return userSvc.WithPersonalPrefs(prefs)
	}
	return userSvc
}

// getAIPersonalSettings GET /api/v1/ai/personal-settings：本人个人 AI 配置
// （掩码视图：provider 仅报 api_key_configured，绝不含明文 key；无记录返回
// 空结构）。
func (h *Handler) getAIPersonalSettings(c *gin.Context) {
	if h.aiPrefs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "personal ai settings are not configured"})
		return
	}
	prefs, ok := h.loadAIPersonalPrefs(userID(c))
	if !ok {
		c.JSON(http.StatusOK, gin.H{"prefs": auth.AIPersonalPrefs{}.Masked()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"prefs": prefs.Masked()})
}

// putAIPersonalSettings PUT /api/v1/ai/personal-settings：整块替换本人配置。
//   - provider 的 api_key 留空 = 继承现值（按 Provider ID 服务端合并）；
//   - 校验失败 400（code INVALID_AI_PREFS，error 含字段定位）；
//   - 成功返回掩码视图（api_key 不回显）。
func (h *Handler) putAIPersonalSettings(c *gin.Context) {
	if h.aiPrefs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "personal ai settings are not configured"})
		return
	}
	user := userID(c)
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20)) // 1MiB 上限
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	var in auth.AIPersonalPrefs
	if err := json.Unmarshal(body, &in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request", "code": "INVALID_AI_PREFS"})
		return
	}
	// api_key 留空继承现值：按 Provider ID 与库中现值合并（同平台
	// SetAI 的元素级合并语义；新 Provider 空串保持为空）。
	current, _ := h.loadAIPersonalPrefs(user)
	if current.Providers != nil {
		existing := make(map[string]string, len(current.Providers))
		for _, p := range current.Providers {
			existing[p.ID] = p.APIKey
		}
		for i := range in.Providers {
			if in.Providers[i].APIKey == "" {
				in.Providers[i].APIKey = existing[in.Providers[i].ID]
			}
		}
	}
	if err := auth.ValidateAIPersonalPrefs(in); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, auth.ErrInvalidAIPrefs) {
			c.JSON(status, gin.H{"error": err.Error(), "code": "INVALID_AI_PREFS"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save personal ai settings"})
		return
	}
	raw, err := json.Marshal(in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save personal ai settings"})
		return
	}
	if err := h.aiPrefs.SetAIPrefs(user, raw); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save personal ai settings"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"prefs": in.Masked()})
}
