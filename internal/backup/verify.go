// Package backup 提供 DocFlow 备份产物的只读校验（管理闭环的读侧）：
// 发现 BACKUP_DIR 下最近的 manifest.json（脚本 v2 的 docflow-backup-<ts>/
// 目录布局，兼容 v1 顶层 docflow-*.manifest.json 散落清单），解析组件清单
// 并对每文件重算 sha256。所有操作只读备份文件，不执行外部命令、不触达
// 数据库（备份由 scripts/backup.sh|ps1 在服务进程外执行，run 端点恒 501）。
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry 为 manifest.json 中的单文件条目（v2 含 size；v1 清单无 size 时为 0，
// 校验只比对 sha256）。
type Entry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Type   string `json:"type"`
	Size   int64  `json:"size,omitempty"`
}

// Manifest 为备份清单（scripts/backup v2 格式；timestamp / components /
// object_store / encryption 为 v2 新增，解析 v1 清单时为零值不影响校验）。
type Manifest struct {
	Version     int      `json:"version"`
	Timestamp   string   `json:"timestamp"`
	Components  []string `json:"components"`
	ObjectStore string   `json:"object_store"`
	Encryption  string   `json:"encryption"`
	Entries     []Entry  `json:"entries"`
}

// VerifyMarker 为 verify.json 校验标记：scripts/backup --verify、
// cmd/backup-verify 与 POST /admin/backups/verify 写入，GET /admin/backups/status
// 读取它展示「最近备份是否验证」。
type VerifyMarker struct {
	Verified   bool     `json:"verified"`
	VerifiedAt string   `json:"verified_at"`
	Manifest   string   `json:"manifest"`
	Files      int      `json:"files"`
	Errors     []string `json:"errors,omitempty"`
}

// Result 为一次校验的结构化结果（供管理 API 与 CLI 输出）。
type Result struct {
	Manifest  string   `json:"manifest"`
	Backup    string   `json:"backup"`
	Timestamp string   `json:"timestamp,omitempty"`
	Files     int      `json:"files"`
	Verified  bool     `json:"verified"`
	Errors    []string `json:"errors,omitempty"`
}

// ErrNoBackup 表示备份目录下没有任何 manifest（目录缺失或从未备份）。
var ErrNoBackup = errors.New("no backup manifest found")

// markerName 为校验标记文件名（位于备份目录内，与 manifest.json 同级）。
const markerName = "verify.json"

// Latest 在 dir 下发现最近的备份清单：优先 v2 目录布局
// docflow-backup-<ts>/manifest.json，兼容 v1 顶层 *.manifest.json；
// 按 manifest 修改时间取最近，返回清单路径与其所在备份目录。
func Latest(dir string) (manifestPath, backupDir string, err error) {
	var candidates []string
	for _, pattern := range []string{
		filepath.Join(dir, "docflow-backup-*", "manifest.json"),
		filepath.Join(dir, "*.manifest.json"),
	} {
		matches, _ := filepath.Glob(pattern)
		candidates = append(candidates, matches...)
	}
	if len(candidates) == 0 {
		return "", "", ErrNoBackup
	}
	latest, latestMod := candidates[0], time.Time{}
	for _, p := range candidates[1:] {
		info, statErr := os.Stat(p)
		if statErr != nil {
			continue
		}
		if latestMod.IsZero() || info.ModTime().After(latestMod) {
			latest, latestMod = p, info.ModTime()
		}
	}
	return latest, filepath.Dir(latest), nil
}

// ReadVerifyMarker 读取备份目录的校验标记；不存在或不可解析返回 ok=false。
func ReadVerifyMarker(backupDir string) (VerifyMarker, bool) {
	raw, err := os.ReadFile(filepath.Join(backupDir, markerName))
	if err != nil {
		return VerifyMarker{}, false
	}
	var marker VerifyMarker
	if json.Unmarshal(raw, &marker) != nil {
		return VerifyMarker{}, false
	}
	return marker, true
}

// WriteVerifyMarker 写入校验标记（调用方决定是否忽略错误：BACKUP_DIR 对
// 服务进程只读时失败可安全忽略，状态端点只是不再展示校验状态）。
func WriteVerifyMarker(backupDir string, marker VerifyMarker) error {
	raw, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(backupDir, markerName), raw, 0o644)
}

// Verify 对 dir 下最近一次备份做 sha256 复核：只读备份文件，不执行脚本、
// 不触达数据库。清单缺失返回 ErrNoBackup；其余问题（IO/解析/哈希不符/
// 缺 postgres 组件）记入 Result.Errors 并返回非 nil error，调用方仍可用
// Result 展示细节。
func Verify(dir string) (Result, error) {
	var r Result
	manifestPath, backupDir, err := Latest(dir)
	if err != nil {
		return r, err
	}
	r.Manifest, r.Backup = manifestPath, backupDir
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		r.Errors = append(r.Errors, "manifest: "+err.Error())
		return r, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		r.Errors = append(r.Errors, "manifest: "+err.Error())
		return r, fmt.Errorf("read manifest: %w", err)
	}
	r.Timestamp = m.Timestamp
	postgresDump := false
	for _, e := range m.Entries {
		r.Files++
		if e.Type == "postgres_dump" {
			postgresDump = true
		}
		// 清单条目由本仓库脚本生成；防御性拒绝绝对路径与穿越片段。
		if e.Path == "" || filepath.IsAbs(e.Path) || strings.Contains(e.Path, "..") {
			r.Errors = append(r.Errors, e.Path+": invalid manifest path")
			continue
		}
		digest, size, err := fileDigest(filepath.Join(backupDir, filepath.FromSlash(e.Path)))
		if err != nil {
			r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", e.Path, err))
			continue
		}
		if !strings.EqualFold(digest, e.SHA256) {
			r.Errors = append(r.Errors, e.Path+": sha256 mismatch")
			continue
		}
		if e.Size > 0 && size != e.Size {
			r.Errors = append(r.Errors, fmt.Sprintf("%s: size mismatch (manifest %d, actual %d)", e.Path, e.Size, size))
		}
	}
	if !postgresDump {
		r.Errors = append(r.Errors, "postgres dump is missing")
	}
	r.Verified = len(r.Errors) == 0
	if !r.Verified {
		return r, errors.New("backup verification failed")
	}
	return r, nil
}

// fileDigest 流式计算文件 sha256 与字节数。
func fileDigest(path string) (digest string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
