package onlyoffice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/metrics"
	"github.com/google/uuid"
)

// callbackRequest 是 DocumentServer 回调体的协议字段子集；
// 处理时以验签后的 token claims 为信任源（claims 被签名覆盖），body 不可信。
type callbackRequest struct {
	Key    string   `json:"key"`
	Status int      `json:"status"`
	URL    string   `json:"url"`
	Users  []string `json:"users"`
	Token  string   `json:"token"`
}

// HandleCallback 处理 DocumentServer 保存回调（无 Bearer，JWT 自校验）：
//   - JWT 取 body.token 或 Authorization: Bearer 头，HS256 同密钥校验；
//     aud 为 onlyoffice-config/onlyoffice-download 的 token 拒绝（用途隔离）；
//   - status/key/url 以验签 claims 为唯一信任源，缺失必需声明（保存类状态
//     含 url）即拒绝，不回退未签名 body；
//   - status=2/6：写鉴权（claims users[0] 为 actor，SetWriteAuthorizer 注入
//     的授权器判定；未接线 fail closed）通过后先经 CallbackStore.TryRecord
//     抢占幂等键 (file_id, document.key, url)（数据库持久化）；冲突（已处理）
//     直接成功，首次抢占后仅接受与 ONLYOFFICE_SERVER_URL 同源的 url（防 SSRF），
//     下载落为新版本（SHA-256 校验），失败回滚幂等记录允许 DS 重试；
//   - status=4：仅清理（写 onlyoffice.cleanup 审计）；
//   - 其他状态：空处理。
//
// 返回 nil 回复 {"error":0}，非 nil 回复 {"error":1}。
// 每次回调计入 docflow_onlyoffice_callbacks_total{status,result}：status 归一为
// 2（保存）/4（清理）/other（其余状态与校验失败），result 为 0/1。
func (s *Service) HandleCallback(body []byte, authorization, ip, userAgent string) error {
	status, err := s.processCallback(body, authorization, ip, userAgent)
	metrics.IncOnlyOfficeCallback(CallbackStatusLabel(status), CallbackResultLabel(err))
	if err != nil {
		var req callbackRequest
		_ = json.Unmarshal(body, &req)
		fileID, _ := fileIDFromKey(req.Key)
		log.Printf("[onlyoffice] callback failed (file=%s key=%s status=%d): %s", fileID, req.Key, status, redactCallbackError(err.Error()))
	}
	return err
}

func redactCallbackError(message string) string {
	for _, marker := range []string{"token=", "access_token=", "jwt="} {
		if i := strings.Index(strings.ToLower(message), marker); i >= 0 {
			return message[:i] + marker + "[redacted]"
		}
	}
	return message
}

// processCallback 执行回调主逻辑，返回归一化前的原始 status（校验失败且
// body 不可解析时为 0，指标层归一为 other）与处理错误。
func (s *Service) processCallback(body []byte, authorization, ip, userAgent string) (int64, error) {
	var req callbackRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return 0, ErrInvalidToken
	}
	token := req.Token
	if token == "" {
		if parts := strings.Fields(authorization); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			token = parts[1]
		}
	}
	if token == "" {
		return int64(req.Status), ErrInvalidToken
	}
	claims, err := s.parseCallbackToken(token)
	if err != nil {
		return int64(req.Status), err
	}
	// 安全字段（status/key/url）以签名的 claims 为唯一信任源：缺失必需声明
	// 即拒绝（{"error":1}），禁止回退未签名 body。url 仅保存类状态（2/6）
	// 必需——DS 对 status=4 等状态的回调体不携带 url。
	status, ok := claimNumber(claims["status"])
	if !ok {
		return int64(req.Status), ErrInvalidToken
	}
	key := claimString(claims["key"])
	if key == "" {
		return status, ErrInvalidToken
	}
	url := claimString(claims["url"])
	if url == "" && (status == 2 || status == 6) {
		return status, ErrInvalidToken
	}
	// users：claims 存在时为信任源（写鉴权 actor 与版本归属）；claims 缺失时
	// body users 仅用于审计归属展示，不作为授权依据。
	var claimsUsers []string
	if raw, ok := claims["users"].([]any); ok {
		claimsUsers = make([]string, 0, len(raw))
		for _, item := range raw {
			claimsUsers = append(claimsUsers, claimString(item))
		}
	}
	users := req.Users
	if claimsUsers != nil {
		users = claimsUsers
	}
	actor := callbackUser(claimsUsers, uuid.Nil)
	switch status {
	case 2, 6:
		return status, s.saveVersion(status, actor, key, url, users, ip, userAgent)
	case 4:
		fileID, _ := fileIDFromKey(key)
		s.recordAudit(audit.ActionOnlyOfficeCleanup, fileID, callbackUser(users, uuid.Nil),
			audit.StatusSuccess, callbackMetadata(key, 4, nil), ip, userAgent)
		return status, nil
	default:
		// 1/3/7 等状态：无本地动作，按协议确认。
		return status, nil
	}
}

// CallbackStatusLabel 把回调 status 归一为指标标签。设计约定取值为
// 2（保存）/4（清理）/other；status=6（强制保存）等其余状态均归 other。
func CallbackStatusLabel(status int64) string {
	switch status {
	case 2:
		return metrics.CallbackStatusSave
	case 4:
		return metrics.CallbackStatusCleanup
	default:
		return metrics.CallbackStatusOther
	}
}

// CallbackResultLabel 把处理错误归一为指标标签：nil -> "0"，非 nil -> "1"
// （对应回调应答体的 {"error":0/1}）。
func CallbackResultLabel(err error) string {
	if err != nil {
		return metrics.CallbackResultError
	}
	return metrics.CallbackResultOK
}

// saveVersion 处理 status=2/6：验签与写鉴权通过后、处理前先以幂等键
// (file_id, document.key, url) TryRecord 抢占（CallbackStore 持久化）：
//   - inserted=false：同 key 重复回调，视为已处理直接成功（{"error":0}，
//     指标照常计数），不重复建版本；
//   - inserted=true：执行保存（校验文件可用性与 URL 同源、下载落版本、记
//     onlyoffice.save 审计）；任何失败回滚幂等记录（Release）以允许
//     DocumentServer 重试，并返回非 nil（回复 error:1）。
func (s *Service) saveVersion(status int64, actor uuid.UUID, key, url string, users []string, ip, userAgent string) error {
	fileID, err := fileIDFromKey(key)
	if err != nil {
		return err
	}
	// 写鉴权先于幂等抢占：拒绝的回调不消耗幂等键（重试同样被拒）。
	if err := s.authorizeSave(actor, fileID, status, key, ip, userAgent); err != nil {
		return err
	}
	inserted, err := s.callbacks.TryRecord(fileID, key, url, strconv.FormatInt(status, 10), metrics.CallbackResultOK)
	if err != nil {
		return err
	}
	if !inserted {
		// 幂等命中：debug 日志即可，指标由 HandleCallback 照常计数。
		log.Printf("[onlyoffice] duplicate save callback ignored (file=%s key=%s)", fileID, key)
		return nil
	}
	if err := s.processSave(fileID, status, key, url, users, ip, userAgent); err != nil {
		// 处理失败：回滚幂等记录，DocumentServer 重试时可重新抢占。
		if relErr := s.callbacks.Release(fileID, key, url); relErr != nil {
			log.Printf("[onlyoffice] release callback idempotency record failed (file=%s key=%s): %v", fileID, key, relErr)
		}
		return err
	}
	return nil
}

// authorizeSave 保存前写鉴权（fail closed）：actor 取验签 claims 的 users[0]
// （无合法值时 uuid.Nil），经注入的写授权器（SetWriteAuthorizer，
// ValidateReplaceTarget 语义）判定；未接线授权器或判定失败一律拒绝并记
// onlyoffice.save 失败审计，不落任何版本。
func (s *Service) authorizeSave(actor, fileID uuid.UUID, status int64, key, ip, userAgent string) error {
	if s.authorizeWrite == nil {
		s.recordAudit(audit.ActionOnlyOfficeSave, fileID, actor, audit.StatusFailure,
			callbackMetadata(key, status, map[string]any{"reason": "write authorizer not configured"}), ip, userAgent)
		return ErrSaveForbidden
	}
	if err := s.authorizeWrite(actor, fileID); err != nil {
		s.recordAudit(audit.ActionOnlyOfficeSave, fileID, actor, audit.StatusFailure,
			callbackMetadata(key, status, map[string]any{"reason": "write denied"}), ip, userAgent)
		return ErrSaveForbidden
	}
	return nil
}

// processSave 执行保存主流程（幂等记录已抢占）：文件校验、URL 同源校验、
// 下载落版本与审计。
func (s *Service) processSave(fileID uuid.UUID, status int64, key, url string, users []string, ip, userAgent string) error {
	f, err := s.files.GetFileByID(fileID)
	if err != nil {
		s.recordAudit(audit.ActionOnlyOfficeSave, fileID, uuid.Nil, audit.StatusFailure,
			callbackMetadata(key, status, map[string]any{"reason": "file unavailable"}), ip, userAgent)
		return err
	}
	if f.Type != "file" {
		s.recordAudit(audit.ActionOnlyOfficeSave, fileID, uuid.Nil, audit.StatusFailure,
			callbackMetadata(key, status, map[string]any{"reason": "not a file"}), ip, userAgent)
		return files.ErrInvalidTarget
	}
	user := callbackUser(users, f.OwnerID)
	if !s.urlAllowed(url) {
		s.recordAudit(audit.ActionOnlyOfficeSave, fileID, user, audit.StatusFailure,
			callbackMetadata(key, status, map[string]any{"reason": "url not allowed"}), ip, userAgent)
		return ErrURLNotAllowed
	}
	version, size, err := s.downloadAndStore(f, url, user)
	if err != nil {
		s.recordAudit(audit.ActionOnlyOfficeSave, fileID, user, audit.StatusFailure,
			callbackMetadata(key, status, map[string]any{"reason": err.Error()}), ip, userAgent)
		return err
	}
	s.recordAudit(audit.ActionOnlyOfficeSave, fileID, user, audit.StatusSuccess,
		callbackMetadata(key, status, map[string]any{"version": version, "size": size}), ip, userAgent)
	return nil
}

// downloadAndStore 从 DocumentServer 下载保存后的文档并落为新版本：
// stream 到存储临时 key 再转正（objects/<owner>/<uuid>），SHA-256 边下边算，
// AddVersion 复用内容去重（命中去重时清理冗余物理对象）。
func (s *Service) downloadAndStore(f files.File, url string, userID uuid.UUID) (int, int64, error) {
	rc, contentType, err := s.fetch(url)
	if err != nil {
		return 0, 0, err
	}
	defer rc.Close()
	tmpKey := "tmp/onlyoffice-" + uuid.NewString()
	hash := sha256.New()
	counter := &countingReader{r: io.LimitReader(rc, s.cfg.DownloadMaxBytes+1)}
	if err := s.storage.Put(tmpKey, io.TeeReader(counter, hash)); err != nil {
		_ = s.storage.Delete(tmpKey)
		return 0, 0, err
	}
	if counter.n > s.cfg.DownloadMaxBytes {
		_ = s.storage.Delete(tmpKey)
		return 0, 0, ErrDownloadTooLarge
	}
	finalKey := fmt.Sprintf("objects/%s/%s", f.OwnerID, uuid.NewString())
	r, err := s.storage.Read(tmpKey)
	if err == nil {
		err = s.storage.Put(finalKey, r)
		if closeErr := r.Close(); err == nil {
			err = closeErr
		}
	}
	_ = s.storage.Delete(tmpKey)
	if err != nil {
		_ = s.storage.Delete(finalKey)
		return 0, 0, err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	version, newBlob, err := s.files.AddVersion(f, finalKey, sum, counter.n, normalizeContentType(contentType, f.Name), userID)
	if err != nil {
		_ = s.storage.Delete(finalKey)
		return 0, 0, err
	}
	if !newBlob {
		// 同 sha256 的 available blob 已存在（内容去重）：新物理对象冗余，尽力清理。
		_ = s.storage.Delete(finalKey)
	}
	return version.Version, counter.n, nil
}

// normalizeContentType normalizes callback MIME and falls back to the original
// file extension because Document Server commonly responds with octet-stream.
func normalizeContentType(contentType, name string) string {
	contentType = strings.TrimSpace(strings.Split(contentType, ";")[0])
	if contentType != "" && !strings.EqualFold(contentType, "application/octet-stream") {
		return contentType
	}
	if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); detected != "" {
		return strings.TrimSpace(strings.Split(detected, ";")[0])
	}
	return "application/octet-stream"
}

// callbackUser 取回调 users 中首个合法 UUID 作为版本归属用户，缺省回退 fallback。
func callbackUser(users []string, fallback uuid.UUID) uuid.UUID {
	for _, u := range users {
		if id, err := uuid.Parse(u); err == nil {
			return id
		}
	}
	return fallback
}

// callbackMetadata 构造审计 metadata（jsonb 列的 JSON 字符串）。
func callbackMetadata(key string, status int64, extra map[string]any) string {
	m := map[string]any{"key": key, "status": status}
	for k, v := range extra {
		m[k] = v
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// recordAudit 写审计日志（action 取 onlyoffice.save / onlyoffice.cleanup）；
// 写入失败不影响主流程。fileID/userID 为 uuid.Nil 时省略对应字段。
func (s *Service) recordAudit(action string, fileID, userID uuid.UUID, status, metadata, ip, userAgent string) {
	entry := audit.Entry{Action: action, ResourceType: audit.ResourceFile, UserAgent: userAgent, Status: status, Metadata: metadata}
	if fileID != uuid.Nil {
		entry.ResourceID = fileID.String()
	}
	if userID != uuid.Nil {
		entry.UserID = &userID
	}
	if ip != "" {
		entry.IP = &ip
	}
	_ = s.audit.Record(entry)
}
