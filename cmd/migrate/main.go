// Command migrate 按文件名序执行迁移 SQL（默认 migrations/*.sql）。
//
// 迁移脚本内建幂等（CREATE TABLE/INDEX IF NOT EXISTS、ADD COLUMN IF NOT
// EXISTS、先 DROP CONSTRAINT IF EXISTS 再 ADD CONSTRAINT），因此整组迁移可
// 重复执行；本工具不记录版本、不支持回滚（--down 不提供）。
//
// 用法（DATABASE_URL 复用服务端约定）：
//
//	DATABASE_URL=postgres://docflow:change-me@localhost:5432/docflow?sslmode=disable \
//	  go run ./cmd/migrate [-dir migrations]
//
// 说明：通过 PreferSimpleProtocol 走 simple query 协议，单次 Exec 即可执行
// 整个迁移文件内的多条语句（默认扩展协议不支持多语句 Exec）。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func main() {
	dir := flag.String("dir", "migrations", "迁移 SQL 目录（相对当前工作目录）")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		log.Fatalf("read migrations dir %s: %v", *dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		log.Fatalf("no .sql files found in %s", *dir)
	}
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true}), &gorm.Config{})
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	for _, name := range files {
		content, err := os.ReadFile(filepath.Join(*dir, name))
		if err != nil {
			log.Fatalf("read %s: %v", name, err)
		}
		if err := db.Exec(string(content)).Error; err != nil {
			// 单文件失败即退出：保持「失败于哪一步」可诊断，由调用方修复后重跑（幂等）。
			log.Fatalf("apply %s: %v", name, err)
		}
		fmt.Printf("applied %s\n", name)
	}
	fmt.Printf("done: %d migration file(s) applied from %s\n", len(files), *dir)
}
