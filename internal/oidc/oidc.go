// OIDC 单点登录客户端（通用 IdP，授权码流程 + PKCE S256）。
// 不引入 golang.org/x/oauth2（go.mod 无该依赖）：以标准库 net/http 手写
// 授权码流程——发现文档启动时拉取并缓存（失败由 main fatal）；
//   - GET  /auth/oidc/login    → 302 IdP authorization_endpoint（state+PKCE）
//   - GET  /auth/oidc/callback → token endpoint 换 access_token →
//     userinfo endpoint 解析 sub/email/preferred_username（userinfo 不可用
//     时回退解析 ID token 未验签 claims——token 经 TLS 从 token endpoint
//     直接取得，属 OIDC 规范允许的信任路径）。
//
// email 统一小写归一后参与用户匹配（与 auth.NormalizeEmail 一致）。
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// discoveryTimeout 为启动拉取发现文档的超时。
const discoveryTimeout = 10 * time.Second

// httpTimeout 为 token/userinfo 出站请求的默认超时。
const httpTimeout = 15 * time.Second

// Provider 为 OIDC 发现文档的最小字段集（授权码流程所需端点）。
type Provider struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// Discover 拉取 {issuer}/.well-known/openid-configuration 并解析端点。
// 调用方（main）在启动时执行一次并缓存结果，失败即 fatal。
func Discover(ctx context.Context, issuer string) (Provider, error) {
	wellKnown := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return Provider{}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return Provider{}, fmt.Errorf("oidc discovery: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Provider{}, fmt.Errorf("oidc discovery: %s: HTTP %d", wellKnown, response.StatusCode)
	}
	var provider Provider
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&provider); err != nil {
		return Provider{}, fmt.Errorf("oidc discovery: parse: %w", err)
	}
	if provider.AuthorizationEndpoint == "" || provider.TokenEndpoint == "" {
		return Provider{}, errors.New("oidc discovery: authorization/token endpoint missing")
	}
	return provider, nil
}

// Client 是与单个 IdP 交互的授权码客户端（Provider 已发现缓存）。
type Client struct {
	provider     Provider
	clientID     string
	clientSecret string
	redirectURL  string
	httpClient   *http.Client
	// issuer 记录于 oidc_links（多 IdP 预留；当前单 issuer 部署）。
	issuer string
}

// New 以已发现的 Provider 构造客户端（iss 统一去尾部斜杠）。
func New(provider Provider, clientID, clientSecret, redirectURL string) *Client {
	return &Client{
		provider:     provider,
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  redirectURL,
		httpClient:   &http.Client{Timeout: httpTimeout},
		issuer:       strings.TrimSuffix(provider.Issuer, "/"),
	}
}

// Issuer 返回归一后的签发方地址（oidc_links.issuer）。
func (c *Client) Issuer() string { return c.issuer }

// AuthorizeURL 构造跳转 IdP 的授权地址（response_type=code + PKCE S256）。
// state 与 codeChallenge 均由调用方（Service）生成并保管。
func (c *Client) AuthorizeURL(state, codeChallenge string) string {
	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", c.clientID)
	values.Set("redirect_uri", c.redirectURL)
	values.Set("scope", "openid email profile")
	values.Set("state", state)
	values.Set("code_challenge", codeChallenge)
	values.Set("code_challenge_method", "S256")
	separator := "?"
	if strings.Contains(c.provider.AuthorizationEndpoint, "?") {
		separator = "&"
	}
	return c.provider.AuthorizationEndpoint + separator + values.Encode()
}

// TokenResponse 为 token endpoint 的最小响应字段。
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
}

// Exchange 以授权码 + PKCE verifier 换取令牌（client_secret_post 认证；
// 简单通用，覆盖 Google/Keycloak/Authentik 等主流 IdP）。
func (c *Client) Exchange(ctx context.Context, code, verifier string) (TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.redirectURL)
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("code_verifier", verifier)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.provider.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("oidc token exchange: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return TokenResponse{}, err
	}
	if response.StatusCode != http.StatusOK {
		return TokenResponse{}, fmt.Errorf("oidc token exchange: HTTP %d: %s", response.StatusCode, truncateForLog(body))
	}
	var tokens TokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return TokenResponse{}, fmt.Errorf("oidc token exchange: parse: %w", err)
	}
	if tokens.AccessToken == "" {
		return TokenResponse{}, errors.New("oidc token exchange: access_token missing")
	}
	return tokens, nil
}

// Claims 为身份断言的最小字段（sub 为 IdP 内稳定标识，email 小写归一）。
type Claims struct {
	Sub               string
	Email             string
	PreferredUsername string
}

// ResolveClaims 解析身份：优先 userinfo endpoint（Bearer access_token）；
// userinfo 不可用（未配置/404/请求失败）时回退解析 ID token 的未验签
// claims（token 经 TLS 后端信道直接取自 token endpoint，符合 OIDC 规范
// 的信任边界；本项目不额外引 JWKS 验签库）。两者皆不可用返回错误。
func (c *Client) ResolveClaims(ctx context.Context, tokens TokenResponse) (Claims, error) {
	if c.provider.UserinfoEndpoint != "" {
		if claims, err := c.userinfo(ctx, tokens.AccessToken); err == nil {
			return claims, nil
		}
	}
	if tokens.IDToken != "" {
		if claims, err := parseIDTokenClaims(tokens.IDToken); err == nil && claims.Sub != "" {
			return claims, nil
		}
	}
	return Claims{}, errors.New("oidc claims: no usable identity source (userinfo/id_token)")
}

// userinfo GET userinfo_endpoint（Bearer），解析 sub/email/preferred_username。
func (c *Client) userinfo(ctx context.Context, accessToken string) (Claims, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.provider.UserinfoEndpoint, nil)
	if err != nil {
		return Claims{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc userinfo: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Claims{}, err
	}
	if response.StatusCode != http.StatusOK {
		return Claims{}, fmt.Errorf("oidc userinfo: HTTP %d", response.StatusCode)
	}
	var payload struct {
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Claims{}, fmt.Errorf("oidc userinfo: parse: %w", err)
	}
	if payload.Sub == "" {
		return Claims{}, errors.New("oidc userinfo: sub missing")
	}
	return normalizedClaims(payload.Sub, payload.Email, payload.PreferredUsername), nil
}

// parseIDTokenClaims 解析 ID token 的 payload claims（未验签——仅作
// userinfo 不可用时的回退，token 来源于后端直连 token endpoint）。
func parseIDTokenClaims(idToken string) (Claims, error) {
	token, _, err := jwt.NewParser().ParseUnverified(idToken, jwt.MapClaims{})
	if err != nil {
		return Claims{}, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return Claims{}, errors.New("oidc id_token: unexpected claims type")
	}
	stringClaim := func(key string) string {
		if value, ok := claims[key].(string); ok {
			return value
		}
		return ""
	}
	sub := stringClaim("sub")
	if sub == "" {
		return Claims{}, errors.New("oidc id_token: sub missing")
	}
	return normalizedClaims(sub, stringClaim("email"), stringClaim("preferred_username")), nil
}

// normalizedClaims 归一 claims：去首尾空白、email 转小写（与
// auth.NormalizeEmail 匹配逻辑一致）。
func normalizedClaims(sub, email, preferredUsername string) Claims {
	return Claims{
		Sub:               strings.TrimSpace(sub),
		Email:             strings.ToLower(strings.TrimSpace(email)),
		PreferredUsername: strings.TrimSpace(preferredUsername),
	}
}

// ---- PKCE（RFC 7636，S256） ----

// NewVerifier 生成 code_verifier：64 字符的 unreserved 字母表随机串
// （RFC 7636 §4.1 允许 43-128 字符）。
func NewVerifier() (string, error) {
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ChallengeS256 计算 code_challenge：BASE64URL(SHA256(verifier)) 去填充。
func ChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// truncateForLog 截断错误响应体用于日志/错误信息（防泄漏大响应）。
func truncateForLog(body []byte) string {
	const max = 200
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "..."
}
