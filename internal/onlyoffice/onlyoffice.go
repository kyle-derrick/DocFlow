// Package onlyoffice 实现 ONLYOFFICE Document Server 集成（v1.0）：
// 编辑配置生成（JWT 签名）、DocumentServer 回源下载（短期签名 token）、
// 保存回调（status 2/6 落新版本、status 4 仅清理）。
//
// 安全约束（与设计文档 11.1 一致）：
//   - 编辑配置与下载 URL 均以 ONLYOFFICE_JWT_SECRET（HS256）签名；下载 token
//     5 分钟过期且绑定 file_id+version_id，aud=onlyoffice-download、编辑配置
//     token aud=onlyoffice-config，回调解析拒绝携带这两种 aud 的 token（用途
//     混淆拦截；DS 自签回调 token 通常无 aud，允许通过）；
//   - 回调做 JWT 校验（Authorization: Bearer 或 body.token，同密钥），校验通过后
//     status/key/url 以 token claims 为唯一信任源，claims 缺失必需声明即拒绝，
//     禁止回退未签名 body（ONLYOFFICE_ENABLED 时 JWT_SECRET 必填，claims 必有）；
//   - 保存回调（status 2/6）执行写鉴权（fail closed）：以 claims users[0] 为
//     actor 调用注入的写授权器（SetWriteAuthorizer，ValidateReplaceTarget 语义），
//     未接线或无写权限一律拒绝；
//   - 回调下载 URL 仅允许与 ONLYOFFICE_SERVER_URL 同源（scheme/host/port 归一化
//     默认端口后全等，重定向每一跳同样校验），防 SSRF；
//   - 保存前校验文件未删除且 type=file；stream 到存储临时 key 再转正，SHA-256
//     校验后经 files.Store.AddVersion 落版本（复用内容去重与引用计数）；
//   - 幂等键 (file_id, document.key, url) 经 CallbackStore 持久化（默认进程内
//     实现，生产接线 Gorm 版落 onlyoffice_callbacks 表）：同 key 重复回调直接
//     视为成功不重复建版本，处理失败回滚记录允许 DocumentServer 重试。
package onlyoffice

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// DefaultTokenTTL 编辑配置 token 与下载 token 的默认有效期。
	DefaultTokenTTL = 5 * time.Minute
	// DefaultDownloadMaxBytes 回调保存单文件的默认大小上限（接线时可覆盖）。
	DefaultDownloadMaxBytes = 1 << 30
	// downloadAudience 下载 token 的用途声明，防止与编辑配置等其他 token 混用。
	downloadAudience = "onlyoffice-download"
	// configAudience 编辑配置 token 的用途声明；回调解析拦截携带该 aud 的
	// token（编辑配置 token 不得用于伪造保存回调）。
	configAudience = "onlyoffice-config"
)

var (
	// ErrInvalidToken 下载/回调 token 缺失、签名不符、过期、用途或绑定参数不符。
	ErrInvalidToken = errors.New("invalid onlyoffice token")
	// ErrInvalidKey 回调 document.key 无法解析出 file_id。
	ErrInvalidKey = errors.New("invalid onlyoffice document key")
	// ErrURLNotAllowed 回调下载 URL 与 ONLYOFFICE_SERVER_URL 不同源（防 SSRF）。
	ErrURLNotAllowed = errors.New("onlyoffice callback url is not allowed")
	// ErrDownloadTooLarge 回调下载内容超过大小上限。
	ErrDownloadTooLarge = errors.New("onlyoffice callback download exceeds size limit")
	// ErrSaveForbidden 保存回调写鉴权失败：actor（claims users[0]）无写权限，
	// 或未接线写授权器（SetWriteAuthorizer）时一律 fail closed 拒绝。
	ErrSaveForbidden = errors.New("onlyoffice save not authorized")
)

// Config 集成配置（来自 ONLYOFFICE_* 环境变量）。
type Config struct {
	// ServerURL DocumentServer 内网基地址（如 http://onlyoffice:80），
	// 同时是回调下载 URL 防 SSRF 校验的同源基准（始终以此为准）。
	ServerURL string
	// PublicURL 浏览器可达的 DocumentServer 地址（如经反向代理的
	// https://example.com/onlyoffice）；仅用于 /onlyoffice/config 暴露给前端
	// 加载 api.js，为空时回退 ServerURL；不影响 SSRF 校验基准。
	PublicURL string
	// DownloadBase DocumentServer 回源访问后端用的基地址（如 http://backend:8080），
	// 用于拼接 document.url 与 editorConfig.callbackUrl。
	DownloadBase string
	// JWTSecret 与 DocumentServer 共享的签名密钥（HS256，启用时 ≥32 字节）。
	JWTSecret string
	// TokenTTL 编辑配置与下载 token 有效期（默认 5 分钟）。
	TokenTTL time.Duration
	// DownloadMaxBytes 回调保存的单文件大小上限（默认 1GiB）。
	DownloadMaxBytes int64
}

// WriteAuthorizer 校验 user 能否将新版本写入 fileID（与 files 包
// ValidateReplaceTarget 同语义：个人文件 owner、团队文件 CanWrite）；
// 返回 nil 表示允许写入。
type WriteAuthorizer func(user, fileID uuid.UUID) error

// FileStore 抽象集成所需的文件/版本数据访问（生产实现 *files.Store）。
type FileStore interface {
	// Get 返回 user 可读的未删除文件（authorizeFileAccess 同规则）。
	Get(user, id uuid.UUID) (files.File, error)
	// CurrentVersion 返回文件当前版本及关联 blob。
	CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
	// GetFileByID 返回未删除文件行（不限 owner，授权由调用方保证）。
	GetFileByID(id uuid.UUID) (files.File, error)
	// GetVersionBlob 返回属于 fileID 的指定版本及关联 blob（不做用户鉴权）。
	GetVersionBlob(fileID, versionID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
	// AddVersion 向文件追加新版本并设为 current，返回新版本与是否新建了 blob。
	AddVersion(f files.File, storageKey, sha256 string, size int64, mimeType string, userID uuid.UUID) (files.FileVersion, bool, error)
}

// Service 提供 ONLYOFFICE 集成的三个核心能力：编辑配置生成、签名下载解析与回调处理。
type Service struct {
	cfg      Config
	files    FileStore
	storage  upload.Storage
	username func(uuid.UUID) (string, error)
	audit    audit.Recorder
	// callbacks 持久化回调幂等键 (file_id, document.key, url)；
	// 默认进程内实现，生产经 SetCallbackStore 接线 Gorm 版。
	callbacks CallbackStore
	// authorizeWrite 写权限判定（main 经 SetWriteAuthorizer 注入）；
	// 未接线时保存回调一律拒绝、编辑配置降级只读（fail closed）。
	authorizeWrite WriteAuthorizer
	// fetch 从 DocumentServer 下载保存后的文档，返回内容流与 Content-Type；
	// 可注入 fake 以便单测（无网络）。
	fetch func(rawURL string) (io.ReadCloser, string, error)
	now   func() time.Time
}

// New 构造集成服务；recorder 为 nil 时使用 NopRecorder，回调幂等默认进程内
// 存储（main 接线时经 SetCallbackStore 替换为数据库持久化实现）。
func New(cfg Config, store FileStore, storage upload.Storage, username func(uuid.UUID) (string, error), recorder audit.Recorder) *Service {
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = DefaultTokenTTL
	}
	if cfg.DownloadMaxBytes <= 0 {
		cfg.DownloadMaxBytes = DefaultDownloadMaxBytes
	}
	cfg.DownloadBase = strings.TrimSuffix(cfg.DownloadBase, "/")
	cfg.PublicURL = strings.TrimSuffix(cfg.PublicURL, "/")
	if recorder == nil {
		recorder = audit.NopRecorder{}
	}
	s := &Service{
		cfg:       cfg,
		files:     store,
		storage:   storage,
		username:  username,
		audit:     recorder,
		callbacks: newMemoryCallbackStore(),
		now:       time.Now,
	}
	s.fetch = s.defaultFetch
	return s
}

// SetCallbackStore 替换回调幂等存储（main 接线 Gorm 版；应在服务启用前调用）。
func (s *Service) SetCallbackStore(store CallbackStore) {
	if store != nil {
		s.callbacks = store
	}
}

// SetWriteAuthorizer 注入写权限判定器（main 接线；应在服务启用前调用）。
// 未注入时保存回调一律拒绝（{"error":1}）、编辑配置保守降级只读（fail closed）。
func (s *Service) SetWriteAuthorizer(fn WriteAuthorizer) {
	if fn != nil {
		s.authorizeWrite = fn
	}
}

// PublicServerURL 返回 /onlyoffice/config 暴露给前端的 DocumentServer 地址
// （用于加载 {server_url}/web-apps/apps/api/documents/api.js）：优先
// PublicURL（浏览器可达地址，如经反向代理的 https://example.com/onlyoffice），
// 未配置时回退内网 ServerURL（保持既有行为）。回调 SSRF 同源校验不经过
// 本方法，始终以 ServerURL 为基准。
func (s *Service) PublicServerURL() string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	return s.cfg.ServerURL
}

// documentKey 生成编辑会话与版本绑定的 document.key。
// 格式：<file_id 去连字符 32hex>-<version_id 去连字符 32hex>：DocumentServer
// 8.x 对 key 有字符白名单（仅 0-9-.a-zA-Z_=，实报 "unexpected key use key
// pattern"），uuid 原生的冒号分隔与连字符均需规避——冒号直接被拒，故用
// 去连字符 hex + 单个连字符分隔（总长 65，两段各 32 无歧义）。
func documentKey(fileID, versionID uuid.UUID) string {
	return strings.ReplaceAll(fileID.String(), "-", "") + "-" + strings.ReplaceAll(versionID.String(), "-", "")
}

// fileIDFromKey 从 document.key 解析 file_id（key 前段 32 hex，还原连字符
// 后交给 uuid.Parse——google/uuid 亦直接接受 32 位 hex，此处统一走还原路径）。
func fileIDFromKey(key string) (uuid.UUID, error) {
	prefix, _, _ := strings.Cut(key, "-")
	if len(prefix) != 32 {
		return uuid.Nil, ErrInvalidKey
	}
	var b [16]byte
	if _, err := hex.Decode(b[:], []byte(prefix)); err != nil {
		return uuid.Nil, ErrInvalidKey
	}
	id := uuid.UUID(b)
	// fileIDFromKey 仅用于回调定位文件，不要求 version 段存在（status 4 等
	// 通知可能只带前段），但前段必须可解析。
	return id, nil
}

func fileExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i < len(name)-1 {
		return strings.ToLower(name[i+1:])
	}
	return ""
}

// documentType 按扩展名映射 ONLYOFFICE 文档类型（默认 word，含 PDF 查看）。
func documentType(name string) string {
	switch fileExt(name) {
	case "xls", "xlsx", "xlsm", "xlt", "xltx", "ods", "ots", "csv":
		return "cell"
	case "ppt", "pptx", "pptm", "pot", "potx", "odp", "otp":
		return "slide"
	default:
		return "word"
	}
}

// SessionOptions 编辑/查看会话的可选项（HTTP 层从 /onlyoffice/session 请求体
// 解析后传入；零值 = 既有行为）。
type SessionOptions struct {
	// View 强制只读会话（mode=view 且 permissions.edit=false），用于在线预览；
	// false 时按写权限判定（可写 edit，否则 view）。
	View bool
	// Lang 编辑器界面语言（BCP 47 风格，如 zh-CN / en-US）；空或非法回退 zh。
	Lang string
}

// editorLangRe 合法语言标记形状（字母段 + 可选 -数字/字母子段）；防止把
// 任意请求体字符串原样写进编辑器配置。
var editorLangRe = regexp.MustCompile(`^[a-zA-Z]{2,3}(-[a-zA-Z0-9]{2,8}){0,2}$`)

// sanitizeEditorLang 归一化编辑器语言，非法值回退 zh（与历史默认一致）。
func sanitizeEditorLang(lang string) string {
	trimmed := strings.TrimSpace(lang)
	if trimmed == "" || !editorLangRe.MatchString(trimmed) {
		return "zh"
	}
	return trimmed
}

// NewEditConfig 生成可直接传给 DocsAPI.DocEditor 的编辑配置（含 JWT token）。
// 读取权限与 authorizeFileAccess 同规则（个人文件 owner、团队文件任意在册成员），
// 且当前版本 blob 须为 available；编辑能力按写权限降级：注入的写授权器
// （CanWrite/ValidateReplaceTarget 语义）判定通过时 mode=edit 且
// permissions.edit=true，否则（只读 viewer，或未接线授权器时保守 fail closed）
// mode=view 且 permissions.edit=false。document.key 绑定当前版本、document.url 为
// 5 分钟有效期的签名下载 URL，token 以 ONLYOFFICE_JWT_SECRET（HS256）对
// document+editorConfig 整体签名（aud=onlyoffice-config）。
func (s *Service) NewEditConfig(user, fileID uuid.UUID) (map[string]any, error) {
	return s.NewSessionConfig(user, fileID, SessionOptions{})
}

// NewSessionConfig 按 SessionOptions 生成编辑/查看会话配置：
// opts.View 强制只读（预览），opts.Lang 覆盖编辑器界面语言。
func (s *Service) NewSessionConfig(user, fileID uuid.UUID, opts SessionOptions) (map[string]any, error) {
	f, err := s.files.Get(user, fileID)
	if err != nil {
		return nil, err
	}
	if f.Type != "file" {
		return nil, files.ErrInvalidTarget
	}
	version, blob, err := s.files.CurrentVersion(user, fileID)
	if err != nil {
		return nil, err
	}
	if blob.Status != files.BlobStatusAvailable {
		return nil, files.ErrBlobUnavailable
	}
	// 写权限判定：缺授权器时保守只读（fail closed），viewer 只读会话；
	// 显式请求查看会话（预览）时一律只读。
	canEdit := !opts.View && s.authorizeWrite != nil && s.authorizeWrite(user, fileID) == nil
	mode := "view"
	if canEdit {
		mode = "edit"
	}
	token, err := s.signDownloadToken(fileID, version.ID)
	if err != nil {
		return nil, err
	}
	base := s.cfg.DownloadBase
	document := map[string]any{
		"fileType": fileExt(f.Name),
		"key":      documentKey(fileID, version.ID),
		"title":    f.Name,
		"url": fmt.Sprintf("%s/api/v1/onlyoffice/download/%s?v=%s&token=%s",
			base, fileID, version.ID, url.QueryEscape(token)),
		"permissions": map[string]any{"edit": canEdit, "print": true, "download": true},
	}
	name := user.String()
	if s.username != nil {
		if n, nerr := s.username(user); nerr == nil && n != "" {
			name = n
		}
	}
	editorConfig := map[string]any{
		"callbackUrl": base + "/api/v1/onlyoffice/callback",
		"mode":        mode,
		"lang":        sanitizeEditorLang(opts.Lang),
		"user":        map[string]any{"id": user.String(), "name": name},
	}
	config := map[string]any{
		"documentType": documentType(f.Name),
		"document":     document,
		"editorConfig": editorConfig,
	}
	signed, err := s.signConfig(config)
	if err != nil {
		return nil, err
	}
	config["token"] = signed
	return config, nil
}

// ResolveDownload 校验签名下载 token（DocumentServer 回源，无 Bearer）并返回
// 待流式响应的版本实体。token 须为 HS256 签名、aud=onlyoffice-download、
// sub 与路径 file_id 一致、v 与查询版本一致且未过期；blob 非 available 一律拒绝。
func (s *Service) ResolveDownload(fileID uuid.UUID, versionParam, token string) (files.File, files.FileVersion, files.ObjectBlob, error) {
	if token == "" || versionParam == "" {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, ErrInvalidToken
	}
	claims, err := s.parseDownloadToken(token)
	if err != nil {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, err
	}
	if claimString(claims["aud"]) != downloadAudience ||
		claimString(claims["sub"]) != fileID.String() ||
		claimString(claims["v"]) != versionParam {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, ErrInvalidToken
	}
	versionID, err := uuid.Parse(versionParam)
	if err != nil {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, ErrInvalidToken
	}
	version, blob, err := s.files.GetVersionBlob(fileID, versionID)
	if err != nil {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, err
	}
	if blob.Status != files.BlobStatusAvailable {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, files.ErrBlobUnavailable
	}
	f, err := s.files.GetFileByID(fileID)
	if err != nil {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, err
	}
	return f, version, blob, nil
}

// signConfig 对编辑配置整体签名（payload 含 document+editorConfig，附
// iat/exp 与 aud=onlyoffice-config 用途声明；回调解析拒绝携带该 aud 的 token）。
func (s *Service) signConfig(config map[string]any) (string, error) {
	raw, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	claims := jwt.MapClaims{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", err
	}
	now := s.now()
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(s.cfg.TokenTTL).Unix()
	claims["aud"] = configAudience
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.cfg.JWTSecret))
}

// signDownloadToken 签发短期下载 token：sub=file_id、v=version_id、
// aud=onlyoffice-download、exp=now+TTL。
func (s *Service) signDownloadToken(fileID, versionID uuid.UUID) (string, error) {
	now := s.now()
	claims := jwt.MapClaims{
		"sub": fileID.String(),
		"v":   versionID.String(),
		"aud": downloadAudience,
		"iat": now.Unix(),
		"exp": now.Add(s.cfg.TokenTTL).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.cfg.JWTSecret))
}

// parseDownloadToken 校验下载 token 的签名与有效期（exp 必填，按注入时钟校验）。
func (s *Service) parseDownloadToken(token string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return []byte(s.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired(), jwt.WithTimeFunc(s.now))
	if err != nil || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// parseCallbackToken 校验 DocumentServer 回调 token 的签名（同密钥）；
// exp 存在时按注入时钟校验，不强制存在（DS 回调 token 不一定携带 exp）。
// 用途隔离：aud 为 onlyoffice-config 或 onlyoffice-download 的 token 一律
// 拒绝（编辑配置/下载 token 不得混用为回调 token）；DS 自签回调 token 通常
// 无 aud，允许通过。
func (s *Service) parseCallbackToken(token string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return []byte(s.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithTimeFunc(s.now))
	if err != nil || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	for _, aud := range claimStrings(claims["aud"]) {
		if aud == configAudience || aud == downloadAudience {
			return nil, ErrInvalidToken
		}
	}
	return claims, nil
}

func claimString(v any) string {
	s, _ := v.(string)
	return s
}

// claimStrings 提取字符串或字符串数组声明（JWT aud 允许两种形态）。
func claimStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// claimNumber 提取 JSON 数字声明（jwt 解码后为 float64）。
func claimNumber(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

// urlAllowed 判断回调下载 URL 是否与 ONLYOFFICE_SERVER_URL 同源（防 SSRF）。
func (s *Service) urlAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return s.originAllowed(u)
}

// originAllowed 同源判定：scheme 限 http/https，host 不区分大小写，
// 端口按 scheme 默认值归一化后比较；带 userinfo 的 URL 一律拒绝。
func (s *Service) originAllowed(u *url.URL) bool {
	base, err := url.Parse(s.cfg.ServerURL)
	if err != nil || base.Host == "" {
		return false
	}
	if u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if !strings.EqualFold(u.Hostname(), base.Hostname()) {
		return false
	}
	tp, ok1 := effectivePort(u.Scheme, u.Port())
	bp, ok2 := effectivePort(base.Scheme, base.Port())
	return ok1 && ok2 && tp == bp
}

// effectivePort 返回归一化端口（显式端口优先，缺省按 scheme 默认）。
func effectivePort(scheme, port string) (string, bool) {
	if port != "" {
		return port, true
	}
	switch scheme {
	case "http":
		return "80", true
	case "https":
		return "443", true
	default:
		return "", false
	}
}

// defaultFetch 是生产下载实现：GET 下载 URL，重定向每跳都过同源校验（防 SSRF），
// 仅接受 2xx 响应。
func (s *Service) defaultFetch(rawURL string) (io.ReadCloser, string, error) {
	client := &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !s.originAllowed(req.URL) {
				return ErrURLNotAllowed
			}
			if len(via) >= 5 {
				return errors.New("onlyoffice: too many redirects")
			}
			return nil
		},
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, "", fmt.Errorf("onlyoffice: document server returned status %d", resp.StatusCode)
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

// countingReader 统计流经字节数（回调下载计数 + 超限判断）。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
