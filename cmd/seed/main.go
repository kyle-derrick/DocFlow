// Command seed 创建开发/运维用的管理员账号。
// 邮箱与密码必须通过环境变量提供，不允许硬编码默认密码。
//
//	SEED_ADMIN_EMAIL     管理员邮箱（必填）
//	SEED_ADMIN_PASSWORD  管理员密码（必填，至少 12 字符，须含大小写字母和数字）
//	SEED_ADMIN_USERNAME  管理员用户名（默认取邮箱 @ 前部分，3-32 字符）
//	SEED_ADMIN_ROLE      账号角色：user | admin（默认 admin）
//	DATABASE_URL         PostgreSQL 连接串（必填，复用服务端配置）
package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"unicode"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/auth"
)

func main() {
	email := strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL"))
	password := os.Getenv("SEED_ADMIN_PASSWORD")
	username := strings.TrimSpace(os.Getenv("SEED_ADMIN_USERNAME"))
	role := strings.TrimSpace(os.Getenv("SEED_ADMIN_ROLE"))
	if email == "" || password == "" {
		log.Fatal("SEED_ADMIN_EMAIL and SEED_ADMIN_PASSWORD are required")
	}
	if err := validateEmail(email); err != nil {
		log.Fatal(err)
	}
	if err := validatePassword(password); err != nil {
		log.Fatal(err)
	}
	if username == "" {
		username = strings.SplitN(email, "@", 2)[0]
	}
	if err := validateUsername(username); err != nil {
		log.Fatal(err)
	}
	if role == "" {
		role = auth.RoleAdmin
	}
	if role != auth.RoleUser && role != auth.RoleAdmin {
		log.Fatal("SEED_ADMIN_ROLE must be user or admin")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Fatal(err)
	}
	if err := seedAdmin(db, username, email, hash, role); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("admin user %s (%s, role=%s) is ready\n", username, email, role)
}

func seedAdmin(db *gorm.DB, username, email, passwordHash, role string) error {
	result := db.Exec(`
INSERT INTO users (username, email, password_hash, status, role)
VALUES (?, ?, ?, 'active', ?)
ON CONFLICT (email) DO UPDATE
SET username = EXCLUDED.username, password_hash = EXCLUDED.password_hash, status = 'active', role = EXCLUDED.role, updated_at = now()`,
		username, email, passwordHash, role)
	return result.Error
}

// validatePassword 校验密码强度：至少 12 字节，且同时包含大写、小写和数字。
func validatePassword(password string) error {
	if len(password) < 12 {
		return fmt.Errorf("password must be at least 12 characters")
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
		return fmt.Errorf("password must contain upper case, lower case and digit characters")
	}
	return nil
}

func validateEmail(email string) error {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.Count(email, "@") != 1 {
		return fmt.Errorf("invalid email address")
	}
	return nil
}

func validateUsername(username string) error {
	n := len([]rune(username))
	if n < 3 || n > 32 {
		return fmt.Errorf("username must be 3-32 characters")
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return fmt.Errorf("username may only contain letters, digits, '_' and '-'")
		}
	}
	return nil
}
