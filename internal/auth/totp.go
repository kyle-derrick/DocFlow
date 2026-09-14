// 两步验证（TOTP，v2 设计）：user_totp 表的持久化抽象与 Service 方法。
//   - 协议：RFC 6238，SHA-1 / 6 位 / 30 秒周期，校验允许 ±1 窗口（±30s）；
//   - 恢复码：xxxx-xxxx（每段 4 字符，去混淆字母表），10 个一次性；
//     库存 SHA-256 hex 哈希，明文仅 ConfirmSetup 响应返回一次；
//   - 登录防绕过：/login 对 enabled 用户绝不发 token（HTTP 层 401
//     TOTP_REQUIRED），第二段经 /auth/login/totp 重新验证密码 + 码。
package auth

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TOTP 协议与恢复码约束。
const (
	// TOTPIssuer 为 otpauth URL 与认证器展示的签发方（config 无 SITE_NAME，
	// 固定常量；认证器以 issuer + 账号（用户邮箱）区分条目）。
	TOTPIssuer = "DocFlow"
	// TOTPRecoveryCount 为 confirm 时生成的恢复码个数。
	TOTPRecoveryCount = 10
	// totpPeriodSeconds / totpDigits 与主流认证器（Google Authenticator 等）
	// 默认一致；totpSkew 为允许的漂移窗口数（±1 = ±30s）。
	totpPeriodSeconds = 30
	totpDigits        = otp.DigitsSix
	totpSkew          = 1
)

// TOTP 链路哨兵错误（HTTP 层据此映射状态码）。
var (
	// ErrTOTPNotConfigured 表示 TOTPStore 未注入（端点 503）。
	ErrTOTPNotConfigured = errors.New("totp is not configured")
	// ErrTOTPNotEnabled 表示用户未启用两步验证（禁用/校验请求 404/401）。
	ErrTOTPNotEnabled = errors.New("totp is not enabled")
	// ErrTOTPAlreadyEnabled 表示已启用（重复 setup/confirm 409）。
	ErrTOTPAlreadyEnabled = errors.New("totp is already enabled")
	// ErrTOTPSetupRequired 表示 setup 未开始即 confirm（400）。
	ErrTOTPSetupRequired = errors.New("totp setup has not been started")
	// ErrTOTPInvalidCode 表示 6 位 TOTP 码或恢复码校验失败。
	ErrTOTPInvalidCode = errors.New("invalid totp code")
)

// TOTPStore 抽象 user_totp 的持久化（生产实现为 *GormTOTPStore；
// 测试可用内存实现）。
type TOTPStore interface {
	// Get 返回 user 的 TOTP 记录；未 setup 返回 found=false。
	Get(userID uuid.UUID) (UserTOTP, bool, error)
	// CreateOrUpdate 按 user_id UPSERT 整行（setup：enabled=false 重写）。
	CreateOrUpdate(record UserTOTP) error
	// Enable 置 enabled=true 并写 confirmed_at（行不存在返回错误）。
	Enable(userID uuid.UUID, now time.Time) error
	// Delete 删除 user 的记录（禁用即删行，重新启用走全新 setup）。
	Delete(userID uuid.UUID) error
	// ReplaceRecoveryCodes 整体替换恢复码哈希数组（confirm 时写入 10 个）。
	ReplaceRecoveryCodes(userID uuid.UUID, hashes []string) error
	// ConsumeRecoveryCode 原子消耗恢复码：JSON 数组移除匹配哈希，返回是否
	// 命中（一次性：并发/重复使用同一码仅一次成功）。
	ConsumeRecoveryCode(userID uuid.UUID, codeHash string, now time.Time) (bool, error)
}

// GormTOTPStore 是 TOTPStore 的 PostgreSQL 实现（user_totp 表见
// migrations/020）。
type GormTOTPStore struct{ db *gorm.DB }

func NewGormTOTPStore(db *gorm.DB) *GormTOTPStore { return &GormTOTPStore{db: db} }

func (s *GormTOTPStore) Get(userID uuid.UUID) (UserTOTP, bool, error) {
	var record UserTOTP
	err := s.db.First(&record, "user_id = ?", userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return UserTOTP{}, false, nil
	}
	if err != nil {
		return UserTOTP{}, false, err
	}
	return record, true, nil
}

// CreateOrUpdate 按 user_id UPSERT（ON CONFLICT 更新业务列；行不存在则插入，
// created_at 由冲突路径保留原值——updated_at 恒重写）。
func (s *GormTOTPStore) CreateOrUpdate(record UserTOTP) error {
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"secret", "enabled", "recovery_codes", "confirmed_at", "updated_at"}),
	}).Create(&record).Error
}

func (s *GormTOTPStore) Enable(userID uuid.UUID, now time.Time) error {
	result := s.db.Model(&UserTOTP{}).Where("user_id = ?", userID).
		Updates(map[string]any{"enabled": true, "confirmed_at": now, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrTOTPSetupRequired
	}
	return nil
}

func (s *GormTOTPStore) Delete(userID uuid.UUID) error {
	return s.db.Delete(&UserTOTP{}, "user_id = ?", userID).Error
}

func (s *GormTOTPStore) ReplaceRecoveryCodes(userID uuid.UUID, hashes []string) error {
	raw, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	return s.db.Model(&UserTOTP{}).Where("user_id = ?", userID).
		Updates(map[string]any{"recovery_codes": string(raw), "updated_at": time.Now().UTC()}).Error
}

// ConsumeRecoveryCode 原子消耗：事务内 SELECT ... FOR UPDATE 行锁串行化
// 并发消费，命中即从 JSON 数组移除该哈希并写回；未命中（无行/无匹配）
// 返回 false 且不产生写入。
func (s *GormTOTPStore) ConsumeRecoveryCode(userID uuid.UUID, codeHash string, now time.Time) (bool, error) {
	consumed := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var record UserTOTP
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&record, "user_id = ?", userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var hashes []string
		if err := json.Unmarshal([]byte(record.RecoveryCodes), &hashes); err != nil {
			return err
		}
		kept := make([]string, 0, len(hashes))
		found := false
		for _, h := range hashes {
			if !found && h == codeHash {
				found = true
				continue
			}
			kept = append(kept, h)
		}
		if !found {
			return nil
		}
		raw, err := json.Marshal(kept)
		if err != nil {
			return err
		}
		consumed = true
		return tx.Model(&UserTOTP{}).Where("user_id = ?", userID).
			Updates(map[string]any{"recovery_codes": string(raw), "updated_at": now}).Error
	})
	return consumed, err
}

// SetTOTPStore 注入两步验证存储（幂等；nil 不覆盖）。
func (s *Service) SetTOTPStore(store TOTPStore) {
	if store != nil {
		s.totp = store
	}
}

// TOTPStatus 为 GET /api/v1/auth/totp 的状态视图。
type TOTPStatus struct {
	Enabled     bool
	ConfirmedAt *time.Time
}

// TOTPStatus 返回当前状态；未 setup 时 enabled=false、confirmed_at 为空。
func (s *Service) TOTPStatus(userID uuid.UUID) (TOTPStatus, error) {
	if s.totp == nil {
		return TOTPStatus{}, ErrTOTPNotConfigured
	}
	record, found, err := s.totp.Get(userID)
	if err != nil {
		return TOTPStatus{}, err
	}
	if !found {
		return TOTPStatus{}, nil
	}
	return TOTPStatus{Enabled: record.Enabled, ConfirmedAt: record.ConfirmedAt}, nil
}

// TOTPEnabled 判断 user 是否启用两步验证（登录拦截用）。store 未配置恒
// false（两步验证功能整体未启用时不阻断登录）；查询失败返回错误（fail
// closed：无法确认 enabled 时不得发 token）。
func (s *Service) TOTPEnabled(userID uuid.UUID) (bool, error) {
	if s.totp == nil {
		return false, nil
	}
	record, found, err := s.totp.Get(userID)
	if err != nil {
		return false, err
	}
	return found && record.Enabled, nil
}

// BeginTOTPSetup 开始设置：生成新 secret（作废旧 setup）并落库 enabled=false，
// 返回 secret 与 otpauth URL（认证器手动录入或导入用）。已启用用户须先禁用。
func (s *Service) BeginTOTPSetup(userID uuid.UUID) (secret, otpauthURL string, err error) {
	if s.totp == nil || s.creds == nil {
		return "", "", ErrTOTPNotConfigured
	}
	record, found, err := s.totp.Get(userID)
	if err != nil {
		return "", "", err
	}
	if found && record.Enabled {
		return "", "", ErrTOTPAlreadyEnabled
	}
	user, err := s.creds.GetByID(userID)
	if err != nil {
		return "", "", err
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      TOTPIssuer,
		AccountName: user.Email,
		Period:      totpPeriodSeconds,
		Digits:      totpDigits,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", "", err
	}
	now := s.now().UTC()
	if err := s.totp.CreateOrUpdate(UserTOTP{
		UserID:        userID,
		Secret:        key.Secret(),
		Enabled:       false,
		RecoveryCodes: "[]",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

// ConfirmTOTPSetup 完成设置：校验 6 位 TOTP 码（±1 窗口）成功后启用
// （enabled=true + confirmed_at）并生成 10 个恢复码。恢复码明文（xxxx-xxxx）
// 仅本次返回，库中只存 SHA-256 哈希。
func (s *Service) ConfirmTOTPSetup(userID uuid.UUID, code string) ([]string, error) {
	if s.totp == nil {
		return nil, ErrTOTPNotConfigured
	}
	record, found, err := s.totp.Get(userID)
	if err != nil {
		return nil, err
	}
	switch {
	case !found:
		return nil, ErrTOTPSetupRequired
	case record.Enabled:
		return nil, ErrTOTPAlreadyEnabled
	}
	if !ValidTOTPCode(record.Secret, code) {
		return nil, ErrTOTPInvalidCode
	}
	if err := s.totp.Enable(userID, s.now().UTC()); err != nil {
		return nil, err
	}
	codes := make([]string, TOTPRecoveryCount)
	hashes := make([]string, TOTPRecoveryCount)
	for i := range codes {
		recovery, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes[i], hashes[i] = recovery, hashToken(NormalizeRecoveryCode(recovery))
	}
	if err := s.totp.ReplaceRecoveryCodes(userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// DisableTOTP 禁用（删行）：password 与 code 至少一项有效——密码须匹配，
// 或提供当前有效 TOTP 码；两者皆空/皆错返回 ErrInvalidCredentials。
func (s *Service) DisableTOTP(userID uuid.UUID, password, code string) error {
	if s.totp == nil || s.creds == nil {
		return ErrTOTPNotConfigured
	}
	record, found, err := s.totp.Get(userID)
	if err != nil {
		return err
	}
	if !found || !record.Enabled {
		return ErrTOTPNotEnabled
	}
	authorized := false
	if password != "" {
		user, err := s.creds.GetByID(userID)
		if err != nil {
			return err
		}
		if s.VerifyPassword(user.PasswordHash, password) == nil {
			authorized = true
		}
	}
	if !authorized && strings.TrimSpace(code) != "" && ValidTOTPCode(record.Secret, code) {
		authorized = true
	}
	if !authorized {
		return ErrInvalidCredentials
	}
	return s.totp.Delete(userID)
}

// VerifyTOTP 登录第二段校验（用户密码已在调用方复核）：code 为 6 位
// TOTP 码（±1 窗口）；未携带或未命中时尝试消耗 recoveryCode（命中即从
// 库中移除，一次性）。均失败返回 ErrTOTPInvalidCode；未启用返回
// ErrTOTPNotEnabled。
func (s *Service) VerifyTOTP(userID uuid.UUID, code, recoveryCode string) error {
	if s.totp == nil {
		return ErrTOTPNotConfigured
	}
	record, found, err := s.totp.Get(userID)
	if err != nil {
		return err
	}
	if !found || !record.Enabled {
		return ErrTOTPNotEnabled
	}
	if strings.TrimSpace(code) != "" && ValidTOTPCode(record.Secret, code) {
		return nil
	}
	if normalized := NormalizeRecoveryCode(recoveryCode); normalized != "" {
		consumed, err := s.totp.ConsumeRecoveryCode(userID, hashToken(normalized), s.now().UTC())
		if err != nil {
			return err
		}
		if consumed {
			return nil
		}
	}
	return ErrTOTPInvalidCode
}

// ValidTOTPCode 校验 6 位数字 TOTP 码（±1 窗口 = ±30s，与主流认证器一致）。
// 形态非 6 位数字直接失败，不进入 HMAC 计算。
func ValidTOTPCode(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	valid, err := totp.ValidateCustom(code, secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    totpPeriodSeconds,
		Skew:      totpSkew,
		Digits:    totpDigits,
		Algorithm: otp.AlgorithmSHA1,
	})
	return err == nil && valid
}

// recoveryCodeAlphabet 为恢复码字符集（32 字符，去 0/o/1/l/i 等易混淆项）。
const recoveryCodeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// newRecoveryCode 生成 xxxx-xxxx 格式恢复码（8 个随机字符 + 连字符）。
func newRecoveryCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, b := range raw {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, recoveryCodeAlphabet[int(b)%len(recoveryCodeAlphabet)])
	}
	return string(out), nil
}

// NormalizeRecoveryCode 归一恢复码输入：小写并去除空白与连字符
// （容忍 "XXXX XXXX"/"xxxxxxxx" 等录入形态），与生成侧哈希前的归一一致。
func NormalizeRecoveryCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(code)) {
		switch r {
		case ' ', '-', '\t':
			continue
		default:
			b.WriteRune(r)
		}
	}
	normalized := b.String()
	// 长度即哈希域：8 字符之外一律视为非法输入（不进入哈希比对）。
	if len(normalized) != 8 {
		return ""
	}
	return normalized
}
