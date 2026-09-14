// Command migrate applies SQL migrations in filename order and records each version.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const advisoryLockKey int64 = 0x444F4346

func main() {
	dir := flag.String("dir", "migrations", "迁移 SQL 目录")
	dryRun := flag.Bool("dry-run", false, "仅列出待执行迁移")
	flag.Parse()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		log.Fatalf("read migrations dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		log.Fatal("no .sql files found")
	}
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true}), &gorm.Config{})
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	if *dryRun {
		if err := listPending(db, names); err != nil {
			log.Fatal(err)
		}
		return
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		log.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version BIGINT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`).Error; err != nil {
		log.Fatalf("create schema_migrations: %v", err)
	}
	applied := map[int64]bool{}
	var versions []int64
	if err := db.Raw("SELECT version FROM schema_migrations").Scan(&versions).Error; err != nil {
		log.Fatal(err)
	}
	for _, v := range versions {
		applied[v] = true
	}
	count := 0
	for _, name := range names {
		v, err := migrationVersion(name)
		if err != nil {
			log.Fatal(err)
		}
		if applied[v] {
			continue
		}
		content, err := os.ReadFile(filepath.Join(*dir, name))
		if err != nil {
			log.Fatal(err)
		}
		if err := db.Exec(string(content)).Error; err != nil {
			log.Fatalf("apply %s: %v", name, err)
		}
		if err := db.Exec("INSERT INTO schema_migrations(version) VALUES (?)", v).Error; err != nil {
			log.Fatalf("record %s: %v", name, err)
		}
		fmt.Printf("applied %s\n", name)
		count++
	}
	fmt.Printf("done: %d migration file(s) applied from %s\n", count, *dir)
}

func migrationVersion(name string) (int64, error) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	first := strings.SplitN(base, "_", 2)[0]
	v, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid migration filename %s", name)
	}
	return v, nil
}
func listPending(db *gorm.DB, names []string) error {
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version BIGINT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`).Error; err != nil {
		return err
	}
	var versions []int64
	if err := db.Raw("SELECT version FROM schema_migrations").Scan(&versions).Error; err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, v := range versions {
		seen[v] = true
	}
	for _, name := range names {
		v, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if !seen[v] {
			fmt.Println(name)
		}
	}
	return nil
}
