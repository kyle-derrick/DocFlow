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

func (h *Handler) wsNotifications(c *gin.Context) {
	if h.realtime == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "realtime service not configured"})
		return
	}
	if !h.originAllowed(c.GetHeader("Origin")) {
		c.Status(http.StatusForbidden)
		return
	}
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
	if token == "" {
		c.Status(http.StatusUnauthorized)
		return
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
		return
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil {
		c.Status(http.StatusUnauthorized)
		return
	}
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return h.originAllowed(r.Header.Get("Origin"))
		},
	}
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
