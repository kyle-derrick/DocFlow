package http

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// originAllowed 为 WebSocket Origin 的唯一校验函数：ALLOWED_ORIGINS 为空
// 放行（同源/本地开发）；否则 Origin 须精确匹配白名单之一。upgrade 前
// 的显式校验与 gorilla Upgrader.CheckOrigin 共用本函数，保持单一策略
// （替代此前 CheckOrigin 恒 true、仅靠 upgrade 前检查兜底的分散写法）。
func (h *Handler) originAllowed(origin string) bool {
	if len(h.allowedOrigins) == 0 {
		return true
	}
	for _, o := range h.allowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// wsBearerToken 从 Sec-WebSocket-Protocol 头的「bearer, <token>」子协议
// 序列对里提取 JWT（浏览器 WebSocket API 无法自定义 Authorization 头的
// 通行做法）；非 production 环境回退 access_token 查询参数（本地调试）。
// /ws/notifications 与 /collab/:fileId/ws 两个 WS 端点共用。
func (h *Handler) wsBearerToken(c *gin.Context) string {
	token := ""
	protocols := c.GetHeader("Sec-WebSocket-Protocol")
	parts := strings.Split(protocols, ",")
	for i := range parts {
		if strings.TrimSpace(parts[i]) == "bearer" && i+1 < len(parts) {
			token = strings.TrimSpace(parts[i+1])
		}
	}
	if token == "" && h.environment != "production" {
		token = c.Query("access_token")
	}
	return token
}

// wsAuthenticate 校验 Bearer JWT（HS256、wsSecret 签名）并解析出用户 ID；
// 缺 token、验签失败或 Subject 非法 UUID 时写 401 并返回 ok=false。
func (h *Handler) wsAuthenticate(c *gin.Context) (uuid.UUID, bool) {
	token := h.wsBearerToken(c)
	if token == "" {
		c.Status(http.StatusUnauthorized)
		return uuid.Nil, false
	}
	claims := &jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(h.wsSecret), nil
	})
	if err != nil || !parsed.Valid {
		c.Status(http.StatusUnauthorized)
		return uuid.Nil, false
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil {
		c.Status(http.StatusUnauthorized)
		return uuid.Nil, false
	}
	return uid, true
}

// wsUpgrader 构造共用升级器：CheckOrigin 与 upgrade 前的显式校验共用
// originAllowed 单一策略。
func (h *Handler) wsUpgrader() websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		// 回显 bearer 子协议：客户端以 ['bearer', <JWT>] 发起握手，101 需
		// 选中一个子协议——部分浏览器实现对「客户端提供而服务端未选择」
		// 的握手严格处理（连接失败），显式声明以确保兼容。
		Subprotocols: []string{"bearer"},
		CheckOrigin: func(r *http.Request) bool {
			return h.originAllowed(r.Header.Get("Origin"))
		},
	}
}

func (h *Handler) wsNotifications(c *gin.Context) {
	if h.realtime == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "realtime service not configured"})
		return
	}
	if !h.originAllowed(c.GetHeader("Origin")) {
		c.Status(http.StatusForbidden)
		return
	}
	uid, ok := h.wsAuthenticate(c)
	if !ok {
		return
	}
	upgrader := h.wsUpgrader()
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	cleanup := h.realtime.Register(uid, conn)
	defer cleanup()
	for {
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
	}
}
