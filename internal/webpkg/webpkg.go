// Package webpkg 实现网页包（zip）的安全解包与内容提供。
//
// 安全基线（设计文档 6.3.6 / 13.3）：
//   - Zip Slip 防护：拒绝 ../、绝对路径、盘符、反斜杠与空路径段；
//   - 符号链接（及一切非常规文件类型）条目拒绝；
//   - 解压炸弹限制：条目数、单文件大小、展开总大小、目录深度；
//   - 文件名与 zip 元数据（条目/归档注释）控制字符拒绝；
//   - 内容与主站隔离：/content 路径无 Bearer/Cookie 依赖，仅从内容前缀提供子资源；
//   - 压缩比异常（>100:1）仅记日志不拒绝——展开总量已被硬限制覆盖。
package webpkg

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 状态与 web_packages.status CHECK 约束一致。
const (
	StatusExtracting = "extracting"
	StatusReady      = "ready"
	StatusFailed     = "failed"
	StatusBlocked    = "blocked"
)

// manifestName 为解包前缀下的内部清单文件名（无白名单扩展名，Resolve 永不提供），
// 记录全部已写入条目的相对路径，供幂等重建时删除旧 key。
const manifestName = ".webpkg-manifest"

var (
	// ErrWebpkgInvalid 表示包未通过安全校验（HTTP 侧映射为 blocked 状态）。
	ErrWebpkgInvalid = errors.New("invalid web package")
	// ErrNotFound 表示 web_packages 行不存在。
	ErrNotFound = errors.New("web package not found")
	// ErrBlobUnavailable 表示文件当前版本对象不可用（非 available 状态）。
	ErrBlobUnavailable = errors.New("web package blob unavailable")
)

// Limits 为解包安全限制；零值字段会被 DefaultLimits 的对应默认值兜底。
type Limits struct {
	// MaxEntries 条目数上限（默认 500，WEBPKG_MAX_ENTRIES）。
	MaxEntries int
	// MaxFileSize 单文件展开大小上限（默认 32MiB，WEBPKG_MAX_FILE_SIZE）。
	MaxFileSize int64
	// MaxTotalSize 展开总大小上限（默认 256MiB，WEBPKG_MAX_TOTAL_SIZE）。
	MaxTotalSize int64
	// MaxDepth 目录深度上限（默认 10，WEBPKG_MAX_DEPTH）。
	MaxDepth int
}

// DefaultLimits 返回内置默认限制。
func DefaultLimits() Limits {
	return Limits{MaxEntries: 500, MaxFileSize: 32 << 20, MaxTotalSize: 256 << 20, MaxDepth: 10}
}

// withDefaults 用 DefaultLimits 兜底非正值的字段（供测试注入局部覆盖）。
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxEntries <= 0 {
		l.MaxEntries = d.MaxEntries
	}
	if l.MaxFileSize <= 0 {
		l.MaxFileSize = d.MaxFileSize
	}
	if l.MaxTotalSize <= 0 {
		l.MaxTotalSize = d.MaxTotalSize
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	return l
}

// Storage 是 webpkg 依赖的最小存储接口（upload.LocalStorage / S3Storage 均满足）。
type Storage interface {
	Put(key string, r io.Reader) error
	Read(key string) (io.ReadCloser, error)
	Delete(key string) error
}

// Package 对应 web_packages 表一行。
type Package struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey"`
	FileID     uuid.UUID `gorm:"type:uuid;not null;uniqueIndex"`
	PublicID   string    `gorm:"size:43;not null;uniqueIndex"`
	EntryCount int       `gorm:"not null;default:0"`
	TotalSize  int64     `gorm:"not null;default:0"`
	Status     string    `gorm:"size:16;not null"`
	// Error 为失败/拦截原因（可空指针，空值落 NULL，修复旧模型 string 空串歧义）。
	Error *string `gorm:"type:text"`
	// SourceBlobSHA256 记录解包时源文件当前版本的 blob sha256（可空，migration 013；
	// 旧数据不回填）。Resolve 比对其与文件当前版本是否一致，版本更替后旧包
	// 内容不再对外提供。
	SourceBlobSHA256 *string   `gorm:"type:char(64)"`
	CreatedAt        time.Time `gorm:"not null"`
	UpdatedAt        time.Time `gorm:"not null"`
}

// TableName 显式映射到 web_packages。
func (Package) TableName() string { return "web_packages" }

// NewPublicID 生成 43 字符 URL-safe base64 随机串（32 字节熵，防枚举）。
func NewPublicID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// URLSafePublicID 校验 public_id 形如 43 字符 URL-safe base64（路由参数防注入）。
func URLSafePublicID(pid string) bool {
	if len(pid) != 43 {
		return false
	}
	if strings.ContainsAny(pid, "/+") {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(pid)
	return err == nil
}

// contentTypes 为 Resolve 的扩展名白名单与推断的 Content-Type
// （text/* 一律 charset=utf-8；svg 以最严格 CSP 由 HTTP 层提供）。
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".svg":   "image/svg+xml",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".map":   "application/json; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
}

// ZipCandidate 判定文件是否为网页包候选：MIME 为 application/zip
// （现有上传链路统一记 application/octet-stream，故同时接受 .zip 文件名后缀）。
func ZipCandidate(mime, name string) bool {
	base := strings.TrimSpace(mime)
	if idx := strings.IndexByte(base, ';'); idx >= 0 {
		base = strings.TrimSpace(base[:idx])
	}
	if strings.EqualFold(base, "application/zip") {
		return true
	}
	dot := strings.LastIndexByte(name, '.')
	return dot >= 0 && strings.EqualFold(name[dot:], ".zip")
}
