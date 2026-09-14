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

// 档案字段长度上限（按 rune 计，与 migration 024 列宽一致）与语言白名单。
const (
	ProfileNicknameMax = 64
	ProfileTextMax     = 128 // department / position
	ProfilePhoneMax    = 32
	ProfileBioMax      = 512
	ProfileTimezoneMax = 64
)

// AllowedLanguages 界面语言白名单（PATCH /api/v1/me 的 language 取值）。
var AllowedLanguages = []string{"zh-CN", "en-US"}

var (
	// ErrProfileTooLong 档案字段超出长度上限。
	ErrProfileTooLong = errors.New("profile field exceeds length limit")
	// ErrInvalidLanguage language 不在白名单（zh-CN|en-US）。
	ErrInvalidLanguage = errors.New("language must be one of zh-CN, en-US")
	// ErrInvalidTimezone timezone 为空或超长（IANA 名称，≤64 字符）。
	ErrInvalidTimezone = errors.New("timezone must be 1-64 characters")
)

// ProfileUpdate 为用户档案更新字段（C21a）：指针区分「未提供」与「清空」
// （nil 表示不更新；非 nil 空串表示清空该字段，写入侧归一为 NULL）。
type ProfileUpdate struct {
	Nickname   *string
	Department *string
	Position   *string
	Phone      *string
	Bio        *string
	Language   *string
	Timezone   *string
}

// Validate 校验档案更新：各文本字段长度上限、language ∈ 白名单、
// timezone 非空且 ≤64 字符（language/timezone 空串视为未提供）。
func (u ProfileUpdate) Validate() error {
	for _, field := range []*string{u.Nickname, u.Department, u.Position, u.Phone, u.Bio} {
		if field == nil {
			continue
		}
		value := *field
		switch {
		case field == u.Nickname && len([]rune(value)) > ProfileNicknameMax:
			return ErrProfileTooLong
		case field == u.Department && len([]rune(value)) > ProfileTextMax:
			return ErrProfileTooLong
		case field == u.Position && len([]rune(value)) > ProfileTextMax:
			return ErrProfileTooLong
		case field == u.Phone && len([]rune(value)) > ProfilePhoneMax:
			return ErrProfileTooLong
		case field == u.Bio && len([]rune(value)) > ProfileBioMax:
			return ErrProfileTooLong
		}
	}
	if u.Language != nil && *u.Language != "" {
		valid := false
		for _, lang := range AllowedLanguages {
			if *u.Language == lang {
				valid = true
				break
			}
		}
		if !valid {
			return ErrInvalidLanguage
		}
	}
	if u.Timezone != nil {
		n := len([]rune(*u.Timezone))
		if n < 1 || n > ProfileTimezoneMax {
			return ErrInvalidTimezone
		}
	}
	return nil
}
