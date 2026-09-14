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
const PATScopesContextKey = "pat_scopes"

func RequireScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if kind, _ := c.Get(AuthKindContextKey); kind == AuthKindPAT {
			values, exists := c.Get(PATScopesContextKey)
			if exists {
				allowed := false
				for _, value := range values.([]string) {
					if value == scope {
						allowed = true
						break
					}
				}
				if !allowed {
					c.JSON(http.StatusForbidden, gin.H{"error": "missing scope"})
					c.Abort()
					return
				}
			}
		}
		c.Next()
	}
}

// auth_kind 上下文标记：区分本次请求的 Bearer 凭证类型。
// PAT 无 session/refresh 语义，依赖方（如 refresh 流程）可据此区分。
const (
	AuthKindContextKey = "auth_kind"
	AuthKindJWT        = "jwt"
	AuthKindPAT        = "pat"
)

// AccessTokenVerifier 抽象 PAT 认证路径（生产实现为 *Service 的
// VerifyPersonalAccessToken）；未注入（nil）时 dfpat_ 前缀凭证一律 401。
type AccessTokenVerifier interface {
	VerifyPersonalAccessToken(token string) (uuid.UUID, error)
}
type ScopedAccessTokenVerifier interface {
	VerifyPersonalAccessTokenWithScopes(token string) (uuid.UUID, []string, error)
}

var _ AccessTokenVerifier = (*Service)(nil)

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

// RequireAccessToken 同时接受两类 Bearer 凭证：
//   - 以 dfpat_ 开头：走 PAT 路径（prefix 定位 + 哈希比对 + 未过期未撤销
//   - 属主 active + TouchLastUsed，见 Service.VerifyPersonalAccessToken），
//     成功后 context 标记 auth_kind=pat；PAT 无 refresh 语义；
//   - 其余：按 HS256 JWT 解析（subject 为用户 UUID），标记 auth_kind=jwt。
//
// 两类凭证统一注入 user_id；下游按 IP+user_id 的限流键不变。
func RequireAccessToken(secret string, pat AccessTokenVerifier) gin.HandlerFunc {
	key := []byte(secret)
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		parts := strings.Fields(header)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}
		if strings.HasPrefix(parts[1], PATPrefix) {
			if pat == nil {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				c.Abort()
				return
			}
			var id uuid.UUID
			var scopes []string
			var err error
			if scoped, ok := pat.(ScopedAccessTokenVerifier); ok {
				id, scopes, err = scoped.VerifyPersonalAccessTokenWithScopes(parts[1])
			} else {
				id, err = pat.VerifyPersonalAccessToken(parts[1])
			}
			if err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				c.Abort()
				return
			}
			c.Set(UserIDContextKey, id)
			if scopes != nil {
				c.Set(PATScopesContextKey, scopes)
			}
			c.Set(AuthKindContextKey, AuthKindPAT)
			c.Next()
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
		c.Set(AuthKindContextKey, AuthKindJWT)
		c.Next()
	}
}
