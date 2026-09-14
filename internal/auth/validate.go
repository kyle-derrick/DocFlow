package auth

import (
	"errors"
	"strings"
	"unicode"
)

// 用户名/密码/邮箱校验的哨兵错误（HTTP 层据此映射 400）。
var (
	ErrPasswordTooShort = errors.New("password must be at least 12 characters")
	ErrPasswordTooWeak  = errors.New("password must contain upper case, lower case and digit characters")
	ErrInvalidUsername  = errors.New("username must be 3-32 characters and may only contain letters, digits, '_' and '-'")
	ErrInvalidEmail     = errors.New("invalid email address")
	// ErrUserExists 表示用户名或邮箱已被占用（任意状态的用户均占用唯一约束）。
	ErrUserExists = errors.New("username or email already exists")
)

// ValidatePasswordStrength 校验密码强度（与 cmd/seed 规则一致）：
// 至少 12 个字符（按 Unicode 字符计数，避免多字节字符被字节计数误判），
// 且同时包含大写、小写和数字。seed 与邀请注册/改密/重置共用。
func ValidatePasswordStrength(password string) error {
	if len([]rune(password)) < 12 {
		return ErrPasswordTooShort
	}
	var hasUpper, hasLower, hasDigit bool
	for _, r := range password {
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if !hasUpper || !hasLower || !hasDigit {
		return ErrPasswordTooWeak
	}
	return nil
}

// ValidateUsername 校验用户名规则（与 cmd/seed 一致）：3-32 字符，
// 仅允许字母、数字、下划线与连字符。
func ValidateUsername(username string) error {
	n := len([]rune(username))
	if n < 3 || n > 32 {
		return ErrInvalidUsername
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return ErrInvalidUsername
		}
	}
	return nil
}

// ValidateEmail 校验最基本的邮箱格式（单个 @、非首尾、非空），
// 不做完整 RFC 校验；配合 NormalizeEmail 使用。
func ValidateEmail(email string) error {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.Count(email, "@") != 1 {
		return ErrInvalidEmail
	}
	return nil
}

// NormalizeEmail 归一邮箱：去首尾空白后转小写。
// 邀请创建、注册（邮箱继承邀请）与登录/找回密码匹配均按归一后值进行。
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
