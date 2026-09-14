package auth

import (
	"encoding/base32"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// fakeTOTPStore 是 TOTPStore 的内存实现（语义与 GormTOTPStore 同步）。
type fakeTOTPStore struct {
	records map[uuid.UUID]UserTOTP
}

func newFakeTOTPStore() *fakeTOTPStore {
	return &fakeTOTPStore{records: make(map[uuid.UUID]UserTOTP)}
}

func (f *fakeTOTPStore) Get(userID uuid.UUID) (UserTOTP, bool, error) {
	record, ok := f.records[userID]
	return record, ok, nil
}

func (f *fakeTOTPStore) CreateOrUpdate(record UserTOTP) error {
	if existing, ok := f.records[record.UserID]; ok {
		record.CreatedAt = existing.CreatedAt
	}
	f.records[record.UserID] = record
	return nil
}

func (f *fakeTOTPStore) Enable(userID uuid.UUID, now time.Time) error {
	record, ok := f.records[userID]
	if !ok {
		return ErrTOTPSetupRequired
	}
	record.Enabled = true
	record.ConfirmedAt = &now
	record.UpdatedAt = now
	f.records[userID] = record
	return nil
}

func (f *fakeTOTPStore) Delete(userID uuid.UUID) error {
	delete(f.records, userID)
	return nil
}

func (f *fakeTOTPStore) ReplaceRecoveryCodes(userID uuid.UUID, hashes []string) error {
	record, ok := f.records[userID]
	if !ok {
		return ErrTOTPSetupRequired
	}
	raw, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	record.RecoveryCodes = string(raw)
	f.records[userID] = record
	return nil
}

// ConsumeRecoveryCode 与 GormTOTPStore 语义一致：命中即从数组移除（一次性）。
func (f *fakeTOTPStore) ConsumeRecoveryCode(userID uuid.UUID, codeHash string, now time.Time) (bool, error) {
	record, ok := f.records[userID]
	if !ok {
		return false, nil
	}
	var hashes []string
	if err := json.Unmarshal([]byte(record.RecoveryCodes), &hashes); err != nil {
		return false, err
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
		return false, nil
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return false, err
	}
	record.RecoveryCodes = string(raw)
	record.UpdatedAt = now
	f.records[userID] = record
	return true, nil
}

// storedRecoveryHashes 读取库中剩余恢复码哈希（测试断言用）。
func storedRecoveryHashes(t *testing.T, store *fakeTOTPStore, userID uuid.UUID) []string {
	t.Helper()
	record, ok := store.records[userID]
	if !ok {
		return nil
	}
	var hashes []string
	if err := json.Unmarshal([]byte(record.RecoveryCodes), &hashes); err != nil {
		t.Fatalf("unmarshal recovery codes %q: %v", record.RecoveryCodes, err)
	}
	return hashes
}

// totpTestService 构造带 TOTP 存储与凭据源的服务及已 seed 用户。
func totpTestService(t *testing.T) (*Service, *fakeTOTPStore, User, string) {
	t.Helper()
	password := "Sup3rSecretPass1"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	user := User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", PasswordHash: string(hash), Status: StatusActive}
	store := newFakeTOTPStore()
	service := NewService(newFakeStore(), "a sufficiently long test secret", time.Minute, time.Hour)
	service.SetCredentials(newFakeCredentials(user))
	service.SetTOTPStore(store)
	return service, store, user, password
}

// secretOf 取库中当前 secret（测试断言用）。
func secretOf(t *testing.T, store *fakeTOTPStore, userID uuid.UUID) string {
	t.Helper()
	record, ok := store.records[userID]
	if !ok {
		t.Fatal("totp record missing")
	}
	return record.Secret
}

// currentCode 生成当前时刻的有效 6 位码（与 ValidTOTPCode 同参数）。
func currentCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, time.Now().UTC(), totp.ValidateOpts{
		Period: totpPeriodSeconds, Skew: 0, Digits: totpDigits, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// codeAt 生成 secret 在 now+offset 时刻的 6 位码（窗口矩阵用）。
func codeAt(t *testing.T, secret string, offset time.Duration) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, time.Now().UTC().Add(offset), totp.ValidateOpts{
		Period: totpPeriodSeconds, Skew: 0, Digits: totpDigits, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// setup：secret 为合法 Base32、otpauth URL 含 issuer 与账号邮箱；
// 落库行 enabled=false；重复 setup 轮换 secret。
func TestTOTPBeginSetupSecretAndURL(t *testing.T) {
	service, store, user, _ := totpTestService(t)
	secret, url, err := service.BeginTOTPSetup(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secret == "" || url == "" {
		t.Fatalf("secret/url must not be empty: %q %q", secret, url)
	}
	// secret 为合法 Base32（认证器可解码录入）。
	if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret); err != nil {
		t.Fatalf("secret %q must be base32: %v", secret, err)
	}
	// otpauth URL 含 issuer（DocFlow）与账号（用户邮箱）。
	for _, want := range []string{"otpauth://totp/", "DocFlow", "alice@example.com"} {
		if !strings.Contains(url, want) {
			t.Fatalf("otpauth url %q must contain %q", url, want)
		}
	}
	record, found := store.records[user.ID]
	if !found {
		t.Fatal("setup row must be persisted")
	}
	if record.Enabled || record.Secret != secret {
		t.Fatalf("stored row = %+v, want enabled=false secret=%q", record, secret)
	}
	// 重复 setup：作废旧 secret，生成新 secret。
	second, _, err := service.BeginTOTPSetup(user.ID)
	if err != nil || second == secret {
		t.Fatalf("re-setup must rotate secret (err=%v)", err)
	}
	if secretOf(t, store, user.ID) != second {
		t.Fatal("stored secret must follow the latest setup")
	}
	// 未注入 TOTPStore：ErrTOTPNotConfigured。
	bare := NewService(newFakeStore(), "a sufficiently long test secret", time.Minute, time.Hour)
	if _, _, err := bare.BeginTOTPSetup(user.ID); !errors.Is(err, ErrTOTPNotConfigured) {
		t.Fatalf("bare service error = %v, want ErrTOTPNotConfigured", err)
	}
}

// confirm ±1 窗口矩阵：0 与 ±30s 通过；±60s、非数字与长度不符拒绝；
// confirm 前置条件（未 setup / 错误码）与重复 confirm。
func TestTOTPConfirmWindowMatrix(t *testing.T) {
	service, store, user, _ := totpTestService(t)
	secret, _, err := service.BeginTOTPSetup(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 窗口矩阵（counter 偏移为整数，判定与时刻在窗口内的位置无关）：
	// ±1 窗口（±30s）通过，±2 窗口拒绝。
	for _, tc := range []struct {
		name   string
		offset time.Duration
		want   bool
	}{
		{name: "current", offset: 0, want: true},
		{name: "plus-one-window", offset: totpPeriodSeconds * time.Second, want: true},
		{name: "minus-one-window", offset: -totpPeriodSeconds * time.Second, want: true},
		{name: "plus-two-windows", offset: 2 * totpPeriodSeconds * time.Second, want: false},
		{name: "minus-two-windows", offset: -2 * totpPeriodSeconds * time.Second, want: false},
	} {
		if got := ValidTOTPCode(secret, codeAt(t, secret, tc.offset)); got != tc.want {
			t.Errorf("%s code: ValidTOTPCode = %v, want %v", tc.name, got, tc.want)
		}
	}
	// 形态拒绝：非数字 / 5 位 / 7 位 / 空白。
	for _, bad := range []string{"", "  ", "abcdef", "12345", "1234567", "12345x"} {
		if ValidTOTPCode(secret, bad) {
			t.Errorf("code %q must be rejected", bad)
		}
	}

	// confirm 全链路：当前码成功 → enabled + confirmed_at + 10 个恢复码。
	codes, err := service.ConfirmTOTPSetup(user.ID, codeAt(t, secret, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != TOTPRecoveryCount {
		t.Fatalf("recovery codes = %d, want %d", len(codes), TOTPRecoveryCount)
	}
	pattern := regexp.MustCompile(`^[a-z2-9]{4}-[a-z2-9]{4}$`)
	for _, code := range codes {
		if !pattern.MatchString(code) {
			t.Fatalf("recovery code %q must match xxxx-xxxx", code)
		}
	}
	record := store.records[user.ID]
	if !record.Enabled || record.ConfirmedAt == nil {
		t.Fatalf("record after confirm = %+v, want enabled with confirmed_at", record)
	}
	// 恢复码库中为哈希而非明文。
	for _, code := range codes {
		if strings.Contains(record.RecoveryCodes, code) {
			t.Fatal("plaintext recovery code must not be stored")
		}
	}
	if got := len(storedRecoveryHashes(t, store, user.ID)); got != TOTPRecoveryCount {
		t.Fatalf("stored hashes = %d, want %d", got, TOTPRecoveryCount)
	}
	// 已启用：重复 confirm 与重复 setup 均 ErrTOTPAlreadyEnabled。
	if _, err := service.ConfirmTOTPSetup(user.ID, codeAt(t, secret, 0)); !errors.Is(err, ErrTOTPAlreadyEnabled) {
		t.Fatalf("double confirm error = %v, want ErrTOTPAlreadyEnabled", err)
	}
	if _, _, err := service.BeginTOTPSetup(user.ID); !errors.Is(err, ErrTOTPAlreadyEnabled) {
		t.Fatalf("setup while enabled error = %v, want ErrTOTPAlreadyEnabled", err)
	}

	// 前置条件：未 setup 即 confirm → ErrTOTPSetupRequired；错误码 → ErrTOTPInvalidCode。
	fresh := User{ID: uuid.New(), Username: "bob", Email: "bob@example.com", Status: StatusActive}
	if fc, ok := service.creds.(*fakeCredentials); ok {
		fc.users[fresh.Email] = fresh
		fc.byID[fresh.ID] = fresh
	}
	if _, err := service.ConfirmTOTPSetup(fresh.ID, "123456"); !errors.Is(err, ErrTOTPSetupRequired) {
		t.Fatalf("confirm without setup error = %v, want ErrTOTPSetupRequired", err)
	}
	if _, _, err := service.BeginTOTPSetup(fresh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmTOTPSetup(fresh.ID, "000000"); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("wrong code error = %v, want ErrTOTPInvalidCode", err)
	}
}

// 恢复码消耗一次性：命中即消耗（大小写/空白容错）；重复使用失败；
// 未命中不产生写入；未启用用户恢复码路径直接拒绝。
func TestTOTPRecoveryCodeOneTime(t *testing.T) {
	service, store, user, _ := totpTestService(t)
	if _, _, err := service.BeginTOTPSetup(user.ID); err != nil {
		t.Fatal(err)
	}
	codes, err := service.ConfirmTOTPSetup(user.ID, currentCode(t, secretOf(t, store, user.ID)))
	if err != nil {
		t.Fatal(err)
	}
	first := codes[0]

	// 未启用/不存在用户：恢复码路径直接失败。
	if err := service.VerifyTOTP(uuid.New(), "", codes[1]); !errors.Is(err, ErrTOTPNotEnabled) {
		t.Fatalf("unknown user error = %v, want ErrTOTPNotEnabled", err)
	}

	// 命中（大写+空格容错）：成功且从库中移除。
	if err := service.VerifyTOTP(user.ID, "", strings.ToUpper(" "+first+" ")); err != nil {
		t.Fatalf("recovery code must verify (case/space tolerant): %v", err)
	}
	if got := len(storedRecoveryHashes(t, store, user.ID)); got != TOTPRecoveryCount-1 {
		t.Fatalf("remaining codes = %d, want %d", got, TOTPRecoveryCount-1)
	}
	// 重复使用同一码：失败（一次性）。
	if err := service.VerifyTOTP(user.ID, "", first); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("replayed recovery code error = %v, want ErrTOTPInvalidCode", err)
	}
	// 未知恢复码：失败且不产生写入。
	if err := service.VerifyTOTP(user.ID, "", "zzzz-zzzz"); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("unknown recovery code error = %v, want ErrTOTPInvalidCode", err)
	}
	if got := len(storedRecoveryHashes(t, store, user.ID)); got != TOTPRecoveryCount-1 {
		t.Fatalf("remaining codes after misses = %d, want %d", got, TOTPRecoveryCount-1)
	}
}

// VerifyTOTP：enabled 时 TOTP 码通过；未启用/错误码失败。
func TestTOTPVerifyCodeEnabledOnly(t *testing.T) {
	service, store, user, _ := totpTestService(t)
	secret, _, err := service.BeginTOTPSetup(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// setup 未 confirm（enabled=false）：VerifyTOTP 拒绝。
	if err := service.VerifyTOTP(user.ID, currentCode(t, secret), ""); !errors.Is(err, ErrTOTPNotEnabled) {
		t.Fatalf("pre-confirm verify error = %v, want ErrTOTPNotEnabled", err)
	}
	if _, err := service.ConfirmTOTPSetup(user.ID, currentCode(t, secret)); err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyTOTP(user.ID, currentCode(t, secretOf(t, store, user.ID)), ""); err != nil {
		t.Fatalf("enabled verify error = %v", err)
	}
	if err := service.VerifyTOTP(user.ID, "000000", ""); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("wrong code error = %v, want ErrTOTPInvalidCode", err)
	}
}

// TOTPEnabled：未配置 store 恒 false（登录不受阻）；setup 未 confirm 为
// false；confirm 后为 true；删除行后恢复 false。
func TestTOTPEnabledLifecycle(t *testing.T) {
	service, store, user, _ := totpTestService(t)
	bare := NewService(newFakeStore(), "a sufficiently long test secret", time.Minute, time.Hour)
	if enabled, err := bare.TOTPEnabled(user.ID); err != nil || enabled {
		t.Fatalf("bare service enabled = %v err = %v, want false nil", enabled, err)
	}
	if enabled, _ := service.TOTPEnabled(user.ID); enabled {
		t.Fatal("no setup must be disabled")
	}
	secret, _, err := service.BeginTOTPSetup(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if enabled, _ := service.TOTPEnabled(user.ID); enabled {
		t.Fatal("unconfirmed setup must not block login")
	}
	if _, err := service.ConfirmTOTPSetup(user.ID, currentCode(t, secret)); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := service.TOTPEnabled(user.ID); !enabled {
		t.Fatal("confirmed setup must be enabled")
	}
	if err := service.DisableTOTP(user.ID, "", currentCode(t, secretOf(t, store, user.ID))); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := service.TOTPEnabled(user.ID); enabled {
		t.Fatal("deleted record must disable")
	}
}

// disable 校验：错密码+错码/空凭据拒绝且行保留；恢复码不可用于禁用；
// 有效 TOTP 码或正确密码通过且删行；重复禁用 ErrTOTPNotEnabled。
func TestTOTPDisableRequiresCredential(t *testing.T) {
	service, store, user, password := totpTestService(t)
	secret, _, err := service.BeginTOTPSetup(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := service.ConfirmTOTPSetup(user.ID, currentCode(t, secret))
	if err != nil {
		t.Fatal(err)
	}

	// 错密码 / 错码 / 均空：拒绝且行保留。
	for _, tc := range []struct {
		name     string
		password string
		code     string
	}{
		{name: "wrong-password", password: "WrongPassword123", code: ""},
		{name: "wrong-code", password: "", code: "000000"},
		{name: "both-wrong", password: "WrongPassword123", code: "000000"},
		{name: "both-empty", password: "", code: "  "},
	} {
		if err := service.DisableTOTP(user.ID, tc.password, tc.code); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("%s: error = %v, want ErrInvalidCredentials", tc.name, err)
		}
		if _, found := store.records[user.ID]; !found {
			t.Fatalf("%s: record must survive failed disable", tc.name)
		}
	}

	// 恢复码不可用于禁用。
	if err := service.DisableTOTP(user.ID, "", codes[0]); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("recovery code must not disable: %v", err)
	}

	// 有效 TOTP 码路径：禁用成功且删行。
	if err := service.DisableTOTP(user.ID, "", currentCode(t, secret)); err != nil {
		t.Fatalf("disable with code error = %v", err)
	}
	if _, found := store.records[user.ID]; found {
		t.Fatal("record must be deleted after disable")
	}
	if err := service.VerifyTOTP(user.ID, currentCode(t, secret), ""); !errors.Is(err, ErrTOTPNotEnabled) {
		t.Fatalf("verify after disable error = %v, want ErrTOTPNotEnabled", err)
	}
	// 再次禁用：ErrTOTPNotEnabled。
	if err := service.DisableTOTP(user.ID, password, ""); !errors.Is(err, ErrTOTPNotEnabled) {
		t.Fatalf("double disable error = %v, want ErrTOTPNotEnabled", err)
	}

	// 密码路径：重新启用后以正确密码禁用。
	if _, _, err := service.BeginTOTPSetup(user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmTOTPSetup(user.ID, currentCode(t, secretOf(t, store, user.ID))); err != nil {
		t.Fatal(err)
	}
	if err := service.DisableTOTP(user.ID, password, ""); err != nil {
		t.Fatalf("disable with password error = %v", err)
	}
}
