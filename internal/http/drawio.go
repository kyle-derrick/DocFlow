package http

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// drawioConfig GET /api/v1/drawio/config：前端探测 draw.io 图表编辑集成
// 可用性与编辑器地址（决定文件行「图表」按钮与 /drawio/:fileId 编辑页的
// 加载）。该端点恒注册（不随 DRAWIO_ENABLED 开关 404，复用 onlyoffice
// config 端点模式）：禁用时返回 {enabled:false, url:null}，不暴露内部 URL；
// 启用时 url 返回浏览器可达地址——DRAWIO_PUBLIC_URL 优先（须为外部可达
// 地址，本机回环视为未配置），未配置时按请求 Host 推导同源反代路径
// {scheme}://{host}/drawio（drawio webapp 为 caddy 静态层 /srv/drawio，
// 同源路径恒可达；旧 DRAWIO_SERVER_URL 内网名仅供服务端 SSRF 白名单，
// 浏览器不可达，不再作为回退）。编辑与保存均在浏览器侧 iframe 内完成
// （postMessage JSON 协议），保存走通用「上传 file_id 覆盖新版本」链路。
func (h *Handler) drawioConfig(c *gin.Context) {
	if !h.drawioEnabled {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "url": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "url": h.drawioURLFor(c)})
}

// drawioURLFor 解析浏览器可达的 drawio 编辑器地址：显式 PUBLIC_URL 非空
// 且非本机回环照旧；否则按请求 scheme/host 推导同源 /drawio（与
// onlyoffice.PublicServerURLFor 同模式）。
func (h *Handler) drawioURLFor(c *gin.Context) string {
	if h.drawioURL != "" && !isLoopbackPublicURL(h.drawioURL) {
		return h.drawioURL
	}
	scheme, host := requestSchemeHost(c)
	if host == "" {
		return h.drawioURL
	}
	return scheme + "://" + host + "/drawio"
}

// isLoopbackPublicURL 判定 URL host 是否本机回环（127.x / localhost /
// [::1]——容器内网名如 http://drawio:8080 也按不可达处理，浏览器侧
// 恒走推导路径）。
func isLoopbackPublicURL(raw string) bool {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	host := s
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		host = s[:i]
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasPrefix(host, "127.") || strings.Contains(host, ".internal") ||
		// 容器服务名（无点且非 IP/localhost）在同源浏览器上下文不可解析
		(!strings.Contains(host, ".") && host != "" && !isIPv4(host))
}

// isIPv4 粗判 host 是否点分 IPv4（含端口场景已在上层剥离）。
func isIPv4(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}

// SetDrawio 注入 draw.io 图表编辑集成配置（幂等）：enabled 时记录
// publicURL 优先、空回退 serverURL 作为「显式配置」候选——两者均可能
// 在运行时被 drawioURLFor 判为浏览器不可达（内网名/回环）而改用同源
// 推导；enabled=false 时保持禁用（零值态）。
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
