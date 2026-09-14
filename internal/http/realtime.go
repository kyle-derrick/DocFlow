package http

import (
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"net/http"
	"strings"
)

func (h *Handler) wsNotifications(c *gin.Context) {
	if h.realtime == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "realtime service not configured"})
		return
	}
	origin := c.GetHeader("Origin")
	allowed := len(h.allowedOrigins) == 0
	for _, o := range h.allowedOrigins {
		if o == origin {
			allowed = true
		}
	}
	if !allowed {
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
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024, CheckOrigin: func(*http.Request) bool { return true }}
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
