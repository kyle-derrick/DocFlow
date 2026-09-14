package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/docflow/docflow/internal/backup"
)

// backup-verify 对 BACKUP_DIR 下最近一次备份做只读 sha256 复核（与
// scripts/backup.sh|ps1 --verify 及 POST /admin/backups/verify 同语义），
// 结果写 verify.json 标记供 GET /admin/backups/status 展示。
func main() {
	dir := flag.String("dir", os.Getenv("BACKUP_DIR"), "备份目录")
	flag.Parse()
	if *dir == "" {
		*dir = "./backups"
	}
	r, err := backup.Verify(*dir)
	if r.Backup != "" {
		marker := backup.VerifyMarker{Verified: r.Verified, VerifiedAt: time.Now().UTC().Format(time.RFC3339), Manifest: r.Manifest, Files: r.Files, Errors: r.Errors}
		if markerErr := backup.WriteVerifyMarker(r.Backup, marker); markerErr != nil {
			log.Printf("write verify marker: %v", markerErr)
		}
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
}
