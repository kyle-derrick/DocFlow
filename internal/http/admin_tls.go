package http

import (
	"errors"
	"io"
	"mime/multipart"
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

// tlsCertJSON 为 GET /admin/tls 附带的证书摘要（custom 模式上传后非空）。
type tlsCertJSON struct {
	CN        string   `json:"cn"`
	NotBefore string   `json:"not_before"`
	NotAfter  string   `json:"not_after"`
	DNSNames  []string `json:"dns_names,omitempty"`
}

// tlsJSON 为 GET /admin/tls 响应：当前模式/域名/是否托管（CADDY_ADMIN_ADDR
// 配置与否决定页面是否展示可编辑表单）与已上传证书摘要（无则 null）。
type tlsJSON struct {
	Mode    string       `json:"mode"`
	Domain  string       `json:"domain"`
	Managed bool         `json:"managed"`
	Cert    *tlsCertJSON `json:"cert,omitempty"`
}

func tlsView(st caddytls.State, cert *tlsCertJSON) tlsJSON {
	return tlsJSON{Mode: string(st.Mode), Domain: st.Domain, Managed: true, Cert: cert}
}

// certView 读取服务端证书摘要；未上传/不可解析返回 nil（页面展示「未上传」）。
func certView(s *caddytls.Service) *tlsCertJSON {
	info, err := s.ReadCert()
	if err != nil {
		return nil
	}
	return &tlsCertJSON{
		CN:        info.CN,
		NotBefore: info.NotBefore.Format("2006-01-02 15:04:05 MST"),
		NotAfter:  info.NotAfter.Format("2006-01-02 15:04:05 MST"),
		DNSNames:  info.DNSNames,
	}
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
	c.JSON(http.StatusOK, tlsView(st, certView(h.caddyTLS)))
}

// adminUpdateTLS 切换 HTTPS 模式（热下发 Caddy，无需重启容器）：
//   - http     明文（本地验证默认）
//   - auto     域名 + ACME 自动签发（需公网 DNS 指向本机）
//   - internal 域名/IP + 自签（内网部署，浏览器不受信告警）
//   - custom   域名/IP + 已上传的自定义证书（须先 POST /admin/tls/cert）
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
	c.JSON(http.StatusOK, tlsView(st, certView(h.caddyTLS)))
}

// adminUploadTLSCert 上传自定义证书（multipart/form-data，字段 cert=证书
// PEM、key=私钥 PEM）：校验可解析且私钥匹配后原子落盘共享卷（0600），
// 返回证书摘要（CN / 有效期 / SAN）。仅存储，不改变当前模式——切换由
// PUT /admin/tls mode=custom 完成。
func (h *Handler) adminUploadTLSCert(c *gin.Context) {
	if h.caddyTLS == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tls runtime switching not configured (CADDY_ADMIN_ADDR empty)"})
		return
	}
	certFile, err := c.FormFile("cert")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "form field \"cert\" (certificate PEM file) is required"})
		return
	}
	keyFile, err := c.FormFile("key")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "form field \"key\" (private key PEM file) is required"})
		return
	}
	certPEM, err := readUploadedFile(certFile)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	keyPEM, err := readUploadedFile(keyFile)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	info, err := h.caddyTLS.UploadCert(certPEM, keyPEM, currentActorID(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	st, _ := h.caddyTLS.Get(c.Request.Context())
	c.JSON(http.StatusOK, tlsView(st, &tlsCertJSON{
		CN:        info.CN,
		NotBefore: info.NotBefore.Format("2006-01-02 15:04:05 MST"),
		NotAfter:  info.NotAfter.Format("2006-01-02 15:04:05 MST"),
		DNSNames:  info.DNSNames,
	}))
}

// readUploadedFile 读取上传文件内容（上限 1MiB——PEM 证书链远小于此）。
func readUploadedFile(fh *multipart.FileHeader) ([]byte, error) {
	if fh.Size > 1<<20 {
		return nil, errors.New("uploaded file too large (max 1MiB)")
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
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
