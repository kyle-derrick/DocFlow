package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const UserIDContextKey = "user_id"

// RoleLookup 按 user id 返回角色（生产实现为 *UserStore），
// 供 RequireRole 鉴权；用户不存在返回 ErrUserNotFound。
type RoleLookup interface {
	Role(id uuid.UUID) (string, error)
}

var _ RoleLookup = (*UserStore)(nil)

// RequireRole 校验当前用户具备指定角色；须挂在 RequireAccessToken 之后使用
// （依赖其注入的 user_id）。角色不足或用户不存在返回 403，角色查询故障返回 500。
func RequireRole(role string, lookup RoleLookup) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := c.Get(UserIDContextKey)
		uid, valid := id.(uuid.UUID)
		if !ok || !valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}
		if lookup == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "role lookup is not configured"})
			c.Abort()
			return
		}
		actual, err := lookup.Role(uid)
		if err != nil {
			if errors.Is(err, ErrUserNotFound) {
				c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
				c.Abort()
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "role lookup failed"})
			c.Abort()
			return
		}
		if actual != role {
			c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func RequireAccessToken(secret string) gin.HandlerFunc {
	key := []byte(secret)
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		parts := strings.Fields(header)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}
		claims := &jwt.RegisteredClaims{}
		token, err := jwt.ParseWithClaims(parts[1], claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, jwt.ErrSignatureInvalid
			}
			return key, nil
		})
		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}
		id, err := uuid.Parse(claims.Subject)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}
		c.Set(UserIDContextKey, id)
		c.Next()
	}
}
