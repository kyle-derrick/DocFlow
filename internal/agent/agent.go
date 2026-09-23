package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	StatusQueued     = "queued"
	StatusRunning    = "running"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
	StatusRolledBack = "rolled_back"
	StatusApplied    = "applied"
)

var (
	ErrImageNotAllowed   = errors.New("agent image is not allowed")
	ErrPathTraversal     = errors.New("unsafe workspace path")
	ErrInvalidTransition = errors.New("invalid agent task state transition")
)

// Harness 取值（settings 键 agent.harness 的配置值与 RuntimeRequest.Harness
// 携带的终值；docker.go 据终值注入容器 env DOCFLOW_HARNESS 供 entrypoint
// 选择执行器）。
const (
	// HarnessAuto 按平台默认模型协议自动选择（http 层在任务创建时解析为
	// 终值后下发：anthropic→HarnessClaudeCode、openai 兼容→HarnessPi、
	// 解析失败/其余 kind→HarnessBuiltin）。
	HarnessAuto = "auto"
	// HarnessClaudeCode Anthropic 协议 harness（Claude Code，经网关
	// /v1/messages 工具透传）。
	HarnessClaudeCode = "claude-code"
	// HarnessPi OpenAI 兼容协议 harness（pi，经网关 /v1/chat/completions
	// 工具直连）。
	HarnessPi = "pi"
	// HarnessBuiltin 内置轻量 runner（镜像默认路径，不注入
	// DOCFLOW_HARNESS）。
	HarnessBuiltin = "builtin"
)

type Config struct {
	Enabled               bool     `json:"enabled"`
	Runtime               string   `json:"runtime"`
	AllowedImages         []string `json:"allowed_images"`
	MaxConcurrent         int      `json:"max_concurrent"`
	DefaultTimeoutSeconds int      `json:"default_timeout_seconds"`
	MaxCPU                int64    `json:"max_cpu"`
	MaxMemoryBytes        int64    `json:"max_memory_bytes"`
	NetworkMode           string   `json:"network_mode"`
	CallbackBaseURL       string   `json:"mcp_callback_base_url"`
}

// DefaultImage 为默认允许的 Agent 镜像：由本仓库 Dockerfile.agent 构建
// （node:20-alpine + git/bash + agent-runner 驱动；Makefile 的 build-agent
// 与 scripts/deploy.ps1 随部署流程自动构建 `docker build -f
// Dockerfile.agent -t docflow/agent:1.0.0 .`）。管理员可在
// agent.allowed_images 中替换为任意镜像（配置非空时整体覆盖本默认值）。
const DefaultImage = "docflow/agent:1.0.0"

func DefaultConfig() Config {
	return Config{Runtime: "docker", AllowedImages: []string{DefaultImage}, MaxConcurrent: 1, DefaultTimeoutSeconds: 900, MaxCPU: 1, MaxMemoryBytes: 512 << 20, NetworkMode: "none"}
}
func (c Config) ValidateImage(image string) error {
	if strings.TrimSpace(image) == "" {
		// 空镜像给明确错误（而非笼统 not allowed）：未配置任何允许镜像且
		// 请求未指定镜像时，提示管理员配置 agent.allowed_images。
		return fmt.Errorf("%w: no image specified and no default allowed image configured (set agent.allowed_images)", ErrImageNotAllowed)
	}
	for _, allowed := range c.AllowedImages {
		if strings.TrimSpace(allowed) == image {
			return nil
		}
	}
	return ErrImageNotAllowed
}
func (c Config) Validate() error {
	if c.Runtime != "docker" && c.Runtime != "fake" || c.MaxConcurrent < 1 || c.DefaultTimeoutSeconds < 1 || c.MaxCPU < 1 || c.MaxMemoryBytes < 1 || (c.NetworkMode != "none" && c.NetworkMode != "restricted") {
		return errors.New("invalid agent configuration")
	}
	return nil
}

func Transition(from, to string) error {
	if from == to {
		return nil
	}
	valid := map[string]map[string]bool{StatusQueued: {StatusRunning: true, StatusCancelled: true}, StatusRunning: {StatusSucceeded: true, StatusFailed: true, StatusCancelled: true}, StatusSucceeded: {StatusRolledBack: true}, StatusFailed: {StatusRolledBack: true}, StatusCancelled: {StatusRolledBack: true}}
	if valid[from][to] {
		return nil
	}
	return ErrInvalidTransition
}

func NewCallbackToken() (plain, digest string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	plain = hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(plain))
	digest = hex.EncodeToString(sum[:])
	return
}
func TokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func SafeJoin(root, rel string) (string, error) {
	if rel == "" || strings.Contains(rel, "\\") || strings.Contains(rel, ":") || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") || strings.Contains("/"+rel+"/", "/../") || strings.Contains("/"+rel+"/", "/./") {
		return "", ErrPathTraversal
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrPathTraversal
	}
	p := filepath.Join(root, clean)
	base, _ := filepath.Abs(root)
	abs, _ := filepath.Abs(p)
	if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return "", ErrPathTraversal
	}
	return p, nil
}
func RejectSymlink(root string, path string, info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return ErrPathTraversal
	}
	_, err := SafeJoin(root, path)
	return err
}

type WorkspaceFile struct {
	Path   string
	Size   int64
	SHA256 string
}

// ScanWorkspace rescans generated files and rejects unsafe entries/symlinks.
func ScanWorkspace(root string) (map[string]WorkspaceFile, error) {
	out := make(map[string]WorkspaceFile)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrPathTraversal
		}
		if path == root || info.IsDir() || info.Name() == "agent-plan.json" || info.Name() == ChangesManifestFile {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, err = SafeJoin(root, filepath.ToSlash(rel)); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = WorkspaceFile{Path: filepath.ToSlash(rel), Size: n, SHA256: fmt.Sprintf("%x", h.Sum(nil))}
		return nil
	})
	return out, err
}

type Task struct {
	ID                 uuid.UUID  `json:"id"`
	UserID             uuid.UUID  `json:"user_id"`
	SpaceID            uuid.UUID  `json:"space_id"`
	RootFolderID       uuid.UUID  `json:"root_folder_id"`
	SnapshotID         uuid.UUID  `json:"snapshot_id"`
	Image              string     `json:"image"`
	Status             string     `json:"status"`
	Prompt             string     `json:"prompt"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	Error              string     `json:"error,omitempty"`
	WorkspacePath      string     `json:"workspace_path,omitempty"`
	WorkspaceExpiresAt *time.Time `json:"workspace_expires_at,omitempty"`
	ApplyResultJSON    string     `json:"-" gorm:"column:apply_result_json;default:'[]'"`
	ApplyDiffHash      string     `json:"apply_diff_hash,omitempty" gorm:"column:apply_diff_hash"`
	AppliedAt          *time.Time `json:"applied_at,omitempty"`
	DiffJSON           string     `json:"-" gorm:"column:diff_json;default:'[]'"`
	CreatedAt          time.Time  `json:"created_at"`
}
type Log struct {
	ID        int64     `json:"id"`
	TaskID    uuid.UUID `json:"task_id"`
	Stream    string    `json:"stream"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}
