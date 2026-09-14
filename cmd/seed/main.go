// Command seed 创建开发/运维用的管理员账号。
// 邮箱与密码必须通过环境变量提供，不允许硬编码默认密码。
// 邮箱统一小写归一后写入与匹配（重跑幂等）。
//
//	SEED_ADMIN_EMAIL     管理员邮箱（必填，写入前统一转小写）
//	SEED_ADMIN_PASSWORD  管理员密码（必填，至少 12 个字符，须含大小写字母和数字）
//	SEED_ADMIN_USERNAME  管理员用户名（默认取邮箱 @ 前部分，3-32 字符）
//	SEED_ADMIN_ROLE      账号角色：user | admin（默认 admin）
//	DATABASE_URL         PostgreSQL 连接串（必填，复用服务端配置）
package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/auth"
)

func main() {
	email := auth.NormalizeEmail(os.Getenv("SEED_ADMIN_EMAIL"))
	password := os.Getenv("SEED_ADMIN_PASSWORD")
	username := strings.TrimSpace(os.Getenv("SEED_ADMIN_USERNAME"))
	role := strings.TrimSpace(os.Getenv("SEED_ADMIN_ROLE"))
	if email == "" || password == "" {
		log.Fatal("SEED_ADMIN_EMAIL and SEED_ADMIN_PASSWORD are required")
	}
	if err := auth.ValidateEmail(email); err != nil {
		log.Fatal(err)
	}
	if err := auth.ValidatePasswordStrength(password); err != nil {
		log.Fatal(err)
	}
	if username == "" {
		username = strings.SplitN(email, "@", 2)[0]
	}
	if err := auth.ValidateUsername(username); err != nil {
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

// seedAdmin upsert 管理员账号：
//   - 冲突更新带 WHERE users.status='active'：disabled/locked 账号不被重跑
//     seed 复活、也不改写其密码/角色/用户名（取舍：需运维显式重新启用账号
//     后再次 seed 才会更新；active 账号的 username/password/role 仍按最新
//     环境变量更新，保持重跑即改密的运维习惯）。
func seedAdmin(db *gorm.DB, username, email, passwordHash, role string) error {
	result := db.Exec(`
INSERT INTO users (username, email, password_hash, status, role)
VALUES (?, ?, ?, 'active', ?)
ON CONFLICT (email) DO UPDATE
SET username = EXCLUDED.username, password_hash = EXCLUDED.password_hash, role = EXCLUDED.role, updated_at = now()
WHERE users.status = 'active'`,
		username, email, passwordHash, role)
	return result.Error
}
