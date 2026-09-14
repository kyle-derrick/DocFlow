package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// drawioConfig GET /api/v1/drawio/config：前端探测 draw.io 图表编辑集成
// 可用性与编辑器地址（决定文件行「图表」按钮与 /drawio/:fileId 编辑页的
// 加载）。该端点恒注册（不随 DRAWIO_ENABLED 开关 404，复用 onlyoffice
// config 端点模式）：禁用时返回 {enabled:false, url:null}，不暴露内部 URL；
// 启用时 url 返回浏览器可达地址（DRAWIO_PUBLIC_URL 优先，未配置回退
// DRAWIO_SERVER_URL——后者为 docker 内网名时浏览器无法加载，生产应配置
// PUBLIC_URL）。编辑与保存均在浏览器侧 iframe 内完成（postMessage JSON
// 协议），保存走通用「上传 file_id 覆盖新版本」链路，无其他后端 drawio 路由。
func (h *Handler) drawioConfig(c *gin.Context) {
	if !h.drawioEnabled {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "url": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "url": h.drawioURL})
}

// SetDrawio 注入 draw.io 图表编辑集成配置（幂等）：enabled 时 url 取
// publicURL 优先、空回退 serverURL（与 onlyoffice config 端点一致的解析
// 规则）；enabled=false 时保持禁用（零值态）。
func (h *Handler) SetDrawio(enabled bool, serverURL, publicURL string) {
	if !enabled {
		return
	}
	url := publicURL
	if url == "" {
		url = serverURL
	}
	h.drawioEnabled = true
	h.drawioURL = url
}
