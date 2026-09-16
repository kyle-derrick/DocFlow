package http

import (
	"errors"
	"net/http"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/caddytls"
	"github.com/gin-gonic/gin"
)

// SetCaddyTLS 注入 HTTPS 运行时切换服务（幂等 Setter，模式参照
// SetStatsSource；nil 忽略——端点按未托管降级）。
func (h *Handler) SetCaddyTLS(s *caddytls.Service) {
	if s != nil {
		h.caddyTLS = s
	}
}

// tlsJSON 为 GET /admin/tls 响应：当前模式/域名/是否托管（CADDY_ADMIN_ADDR
// 配置与否决定页面是否展示可编辑表单）。
type tlsJSON struct {
	Mode    string `json:"mode"`
	Domain  string `json:"domain"`
	Managed bool   `json:"managed"`
}

// adminGetTLS 返回当前 TLS 状态。未托管（caddyTLS nil）时也返回 200 +
// managed=false：页面据此提示「TLS 由部署配置决定」。
func (h *Handler) adminGetTLS(c *gin.Context) {
	if h.caddyTLS == nil {
		c.JSON(http.StatusOK, tlsJSON{Mode: string(caddytls.ModeHTTP), Managed: false})
		return
	}
	st, err := h.caddyTLS.Get(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tlsJSON{Mode: string(st.Mode), Domain: st.Domain, Managed: true})
}

// adminUpdateTLS 切换 HTTPS 模式（热下发 Caddy，无需重启容器）：
//   - http     明文（本地验证默认）
//   - auto     域名 + ACME 自动签发（需公网 DNS 指向本机）
//   - internal 域名/IP + 自签（内网部署，浏览器不受信告警）
//
// 下发失败（caddy 不可达/拒绝配置）原子拒绝，旧配置保持。
func (h *Handler) adminUpdateTLS(c *gin.Context) {
	if h.caddyTLS == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tls runtime switching not configured (CADDY_ADMIN_ADDR empty)"})
		return
	}
	var req struct {
		Mode   string `json:"mode" binding:"required"`
		Domain string `json:"domain"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode is required"})
		return
	}
	actor := currentActorID(c)
	st, err := h.caddyTLS.Apply(c.Request.Context(), caddytls.Mode(req.Mode), req.Domain, actor)
	if err != nil {
		if errors.Is(err, caddytls.ErrNotManaged) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tlsJSON{Mode: string(st.Mode), Domain: st.Domain, Managed: true})
}

// currentActorID 从 gin context 提取当前用户 UUID（auth 中间件注入；
// 未识别返回空串，审计/状态记录按空处理）。
func currentActorID(c *gin.Context) string {
	if v, ok := c.Get(auth.UserIDContextKey); ok {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}
