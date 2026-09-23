package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ErrRuntimeUnavailable = errors.New("agent runtime unavailable: docker runtime is not configured")

// Runtime 执行受限的 Agent 任务。实现不得接收平台凭据或数据库密钥。
type Runtime interface {
	Run(context.Context, RuntimeRequest) (RuntimeResult, error)
}

type RuntimeRequest struct {
	TaskID    string
	Image     string
	Prompt    string
	Workspace string
	// AIToken 为该任务的平台 AI IPC 令牌（agentsock 网关签发；空 = 容器
	// 内无 AI 能力，DockerRuntime 不注入 DOCFLOW_AI_TOKEN）。
	AIToken string
	// Harness 为任务创建时解析的 Agent 执行引擎终值（claude-code/pi/
	// builtin，见 agent.go 常量；DockerRuntime 对非空且非 builtin 值注入
	// DOCFLOW_HARNESS，builtin/空走镜像默认 runner 不注入）。
	Harness string
	// Model 为任务创建时用户选择的模型意图（model_id）：网关两透传端点
	// 刻意不透传 Model（按平台默认对话目标替换执行），DockerRuntime 仅
	// 注入 env DOCFLOW_MODEL 记录意图供 runner/审计感知；空 = 未指定。
	Model string
}

type RuntimeResult struct {
	DryRun    bool
	Detail    string
	Workspace string
	Diff      []WorkspaceDiff
}

type WorkspaceDiff struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type workspaceEntry struct {
	Size   int64
	SHA256 string
}

// diffIgnoreDirs 产物扫描默认忽略的目录名（依赖/版本库/构建缓存/IDE
// 配置）：node_modules 等动辄数万文件，逐文件哈希会拖垮任务收尾且
// 依赖产物不属于「创作内容」——不进 diff、不参与评审 apply。被忽略
// 目录以目录级汇总条目上报（action=ignored），评审台可见已跳过范围。
var diffIgnoreDirs = map[string]bool{
	"node_modules": true, ".git": true, ".hg": true, ".svn": true,
	".venv": true, "venv": true, "__pycache__": true, ".mypy_cache": true,
	".pytest_cache": true, "dist": true, "build": true, "out": true,
	"target": true, ".next": true, ".nuxt": true, ".cache": true,
	".gradle": true, ".idea": true, ".vscode": true, "coverage": true,
	".terraform": true, "bower_components": true, "vendor/pkg": true,
}

// diffIgnoreFiles 产物扫描默认忽略的文件名（锁文件保留可读性、二进制
// 大文件超限单独判定）。
var diffIgnoreFiles = map[string]bool{
	".DS_Store": true, "Thumbs.db": true, "*.pyc": true,
}

// diffMaxSingleFile 单文件超过该大小不进 diff（以 ignored 上报），
// 防止容器内生成的二进制大产物撑爆 diff/评审与后续 apply 传输。
const diffMaxSingleFile = 8 << 20

// ChangesManifestFile 为容器内 agent-runner（agent-runner/runner.mjs）在
// 收尾时写入 workspace 的变更清单文件：git 基线提交后由
// `git status --porcelain` 解析生成（A/M/D）；平台侧 agent.sync_mode=git
// 时优先按它构造 diff，缺失/非法回退 scanWorkspace。两侧扫描均跳过该
// 文件本身（与 agent-plan.json 同语义，防止自递归/误入 apply）。
const ChangesManifestFile = ".docflow-changes.json"

// manifestMaxEntries git 模式清单条目上限（防伪造超大清单拖垮 diff 构造）。
const manifestMaxEntries = 500

// changesManifest 为 .docflow-changes.json 的载荷结构。
type changesManifest struct {
	Changes []struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	} `json:"changes"`
}

// readChangesManifest 读取并校验 runner 产出的变更清单：文件不存在返回
// ok=false（调用方回退全量扫描）；JSON 损坏/条目非法（status 非 A|M|D、
// path 走私、条目超限、A/M 对应文件缺失、D 对应文件仍存在）同样返回
// ok=false——清单不被信任为唯一事实源，任何不一致都回退扫描兜底。
func readChangesManifest(root string) (diff []WorkspaceDiff, ok bool, err error) {
	raw, err := os.ReadFile(filepath.Join(root, ChangesManifestFile))
	if err != nil {
		return nil, false, nil
	}
	var m changesManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false, nil
	}
	if len(m.Changes) == 0 || len(m.Changes) > manifestMaxEntries {
		return nil, false, nil
	}
	seen := make(map[string]bool, len(m.Changes))
	out := make([]WorkspaceDiff, 0, len(m.Changes))
	for _, c := range m.Changes {
		var action string
		switch c.Status {
		case "A":
			action = "added"
		case "M":
			action = "modified"
		case "D":
			action = "deleted"
		default:
			return nil, false, nil
		}
		rel := filepath.ToSlash(strings.TrimSpace(c.Path))
		if rel == "" || seen[rel] {
			return nil, false, nil
		}
		seen[rel] = true
		full, err := SafeJoin(root, rel)
		if err != nil {
			return nil, false, nil
		}
		info, err := os.Lstat(full)
		switch {
		case action == "deleted":
			if err == nil {
				return nil, false, nil // 声称删除但文件仍在：清单不可信
			}
			out = append(out, WorkspaceDiff{Path: rel, Action: action})
		case err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Size() > diffMaxSingleFile:
			return nil, false, nil
		default:
			f, err := os.Open(full)
			if err != nil {
				return nil, false, nil
			}
			h := sha256.New()
			n, readErr := io.Copy(h, f)
			_ = f.Close()
			if readErr != nil {
				return nil, false, nil
			}
			out = append(out, WorkspaceDiff{Path: rel, Action: action, Size: n, SHA256: fmt.Sprintf("%x", h.Sum(nil))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, true, nil
}

// workspaceDiff 按配置的同步模式构造产物 diff：git 模式优先消费 runner
// 产出的 .docflow-changes.json（精确 A/M/D，免全量扫描）；清单缺失/非法
// 或模式为 scan 时回退 scanWorkspace（内置忽略清单的全量对比）。
func (e Executor) workspaceDiff(root string, baseline map[string]workspaceEntry) ([]WorkspaceDiff, error) {
	if e.SyncMode != "scan" {
		if diff, ok, _ := readChangesManifest(root); ok {
			return diff, nil
		}
	}
	return scanWorkspace(root, baseline)
}

// scanWorkspace 全量重扫 workspace 与快照基线对比（沿用内置忽略清单）。
func scanWorkspace(root string, baseline map[string]workspaceEntry) ([]WorkspaceDiff, error) {
	current := make(map[string]workspaceEntry)
	ignored := make(map[string]int) // 目录/原因 → 忽略文件数（汇总上报）
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrPathTraversal
		}
		if path == root {
			return nil
		}
		name := info.Name()
		if info.IsDir() {
			if diffIgnoreDirs[name] {
				// 整目录剪枝：依赖/缓存/版本库不逐文件哈希（node_modules
				// 动辄数万文件），以目录级 ignored 汇总条目进入 diff。
				rel, rerr := filepath.Rel(root, path)
				if rerr == nil {
					ignored[filepath.ToSlash(rel)+"/"] = dirFileCount(path)
				}
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() == "agent-plan.json" || info.Name() == ChangesManifestFile {
			return nil
		}
		if diffIgnoreFiles[name] || strings.HasSuffix(name, ".pyc") {
			rel, rerr := filepath.Rel(root, path)
			if rerr == nil {
				ignored[filepath.ToSlash(rel)] = 1
			}
			return nil
		}
		if info.Size() > diffMaxSingleFile {
			rel, rerr := filepath.Rel(root, path)
			if rerr == nil {
				ignored[filepath.ToSlash(rel)+" (oversize)"] = 1
			}
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
		current[filepath.ToSlash(rel)] = workspaceEntry{Size: n, SHA256: fmt.Sprintf("%x", h.Sum(nil))}
		return nil
	})
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool, len(baseline)+len(current))
	for k := range baseline {
		keys[k] = true
	}
	for k := range current {
		keys[k] = true
	}
	paths := make([]string, 0, len(keys))
	for k := range keys {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	out := make([]WorkspaceDiff, 0)
	for _, p := range paths {
		old, hadOld := baseline[p]
		now, hadNow := current[p]
		action := ""
		switch {
		case !hadOld && hadNow:
			action = "added"
		case hadOld && !hadNow:
			action = "deleted"
		case hadOld && (old.Size != now.Size || old.SHA256 != now.SHA256):
			action = "modified"
		}
		if action != "" {
			d := WorkspaceDiff{Path: p, Action: action}
			if hadNow {
				d.Size, d.SHA256 = now.Size, now.SHA256
			}
			out = append(out, d)
		}
	}
	// 忽略汇总追加在末尾（action=ignored；Size 复用为该目录/条目被
	// 跳过的文件数），评审台可见「已跳过 node_modules/（N 个文件）」
	// 而非数万条噪声；baseline 中位于忽略目录下的历史文件同样不参与。
	ignoredPaths := make([]string, 0, len(ignored))
	for p := range ignored {
		ignoredPaths = append(ignoredPaths, p)
	}
	sort.Strings(ignoredPaths)
	for _, p := range ignoredPaths {
		out = append(out, WorkspaceDiff{Path: p, Action: "ignored", Size: int64(ignored[p])})
	}
	return out, nil
}

// dirFileCount 统计目录内文件总数（含子目录），5000 上限即止——仅用于
// ignored 汇总展示，避免超大依赖目录拖慢收尾。
func dirFileCount(root string) int {
	count := 0
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			count++
		}
		if count >= 5000 {
			return filepath.SkipAll
		}
		return nil
	})
	return count
}

// FakeRuntime 是 v1 的安全默认实现：只在临时 workspace 写计划文件，不声称修改平台文件。
type FakeRuntime struct{}

func (FakeRuntime) Run(ctx context.Context, req RuntimeRequest) (RuntimeResult, error) {
	select {
	case <-ctx.Done():
		return RuntimeResult{}, ctx.Err()
	default:
	}
	if req.Workspace == "" {
		return RuntimeResult{}, errors.New("workspace is required")
	}
	entries := make([]string, 0)
	err := filepath.Walk(req.Workspace, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == req.Workspace || info.Name() == "agent-plan.json" {
			return nil
		}
		rel, e := filepath.Rel(req.Workspace, path)
		if e != nil {
			return e
		}
		if info.IsDir() {
			return nil
		}
		entries = append(entries, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return RuntimeResult{}, err
	}
	sort.Strings(entries)
	plan := struct {
		DryRun bool     `json:"dry_run"`
		Action string   `json:"action"`
		Files  []string `json:"files"`
	}{true, "inspect workspace; no platform files changed", entries}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return RuntimeResult{}, err
	}
	if err := os.WriteFile(filepath.Join(req.Workspace, "agent-plan.json"), append(data, '\n'), 0600); err != nil {
		return RuntimeResult{}, err
	}
	return RuntimeResult{DryRun: true, Detail: "dry-run: plan generated; platform files were not updated", Workspace: req.Workspace}, nil
}

// ExportEntry 描述快照中的一个当前版本文件或目录。
type ExportEntry struct {
	RelativePath string
	NodeType     string
	Size         int64
	Open         func() (io.ReadCloser, error)
}

type ExportLimits struct {
	MaxEntries int
	MaxBytes   int64
}

// ExportWorkspace 只导出普通目录和文件；拒绝路径穿越与符号链接，并限制数量和总大小。
func ExportWorkspace(entries []ExportEntry, limits ExportLimits) (string, error) {
	if limits.MaxEntries < 1 || limits.MaxBytes < 1 {
		return "", errors.New("invalid workspace export limits")
	}
	root, err := os.MkdirTemp("", "docflow-agent-")
	if err != nil {
		return "", err
	}
	cleanup := func(e error) (string, error) { _ = os.RemoveAll(root); return "", e }
	if len(entries) > limits.MaxEntries {
		return cleanup(errors.New("workspace entry limit exceeded"))
	}
	var total int64
	for _, entry := range entries {
		rel, err := SafeJoin(root, filepath.ToSlash(entry.RelativePath))
		if err != nil || entry.RelativePath == "" || strings.Contains(entry.RelativePath, "\\") {
			return cleanup(ErrPathTraversal)
		}
		if entry.NodeType == "folder" {
			if err := os.MkdirAll(rel, 0700); err != nil {
				return cleanup(err)
			}
			continue
		}
		if entry.NodeType != "file" || entry.Size < 0 || total > limits.MaxBytes-entry.Size {
			return cleanup(errors.New("workspace size limit exceeded"))
		}
		total += entry.Size
		if err := os.MkdirAll(filepath.Dir(rel), 0700); err != nil {
			return cleanup(err)
		}
		if entry.Open == nil {
			return cleanup(errors.New("workspace file source is missing"))
		}
		r, err := entry.Open()
		if err != nil {
			return cleanup(err)
		}
		f, err := os.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0666)
		if err != nil {
			r.Close()
			return cleanup(err)
		}
		_, copyErr := io.CopyN(f, r, entry.Size)
		_ = f.Close()
		_ = r.Close()
		if copyErr != nil && !(entry.Size == 0 && errors.Is(copyErr, io.EOF)) {
			return cleanup(copyErr)
		}
	}
	// 跨容器 uid 错配放行：backend（app uid）导出的临时目录默认 0700/
	// 0600，agent 容器以 65534 运行（Claude Code 拒绝 root）会无权读写。
	// 导出内容本就是给容器使用的临时数据，统一放开组/其他读写位。
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(p, 0777)
		} else {
			_ = os.Chmod(p, 0666)
		}
		return nil
	})
	return root, nil
}

type Executor struct {
	Runtime    Runtime
	Timeout    time.Duration
	MaxEntries int
	MaxBytes   int64
	// AIToken 为该任务的平台 AI IPC 令牌（agentsock 网关签发；空 = 容器
	// 内无 AI 能力，DockerRuntime 不注入 DOCFLOW_AI_TOKEN 相关 env）。
	AIToken string
	// Harness 为该任务的 Agent 执行引擎终值（见 agent.go 常量；随
	// RuntimeRequest 下发）。
	Harness string
	// Model 为该任务的模型意图（model_id；随 RuntimeRequest 下发，仅
	// 记录用户选择——实际执行模型由网关按平台默认替换）。
	Model string
	// SyncMode 产物同步模式（agent.sync_mode）：git（默认，优先消费
	// runner 产出的 .docflow-changes.json）| scan（恒全量扫描）。
	SyncMode string
}

func (e Executor) Execute(ctx context.Context, taskID, image, prompt string, entries []ExportEntry) (RuntimeResult, error) {
	if e.Runtime == nil {
		return RuntimeResult{}, ErrRuntimeUnavailable
	}
	workspace, err := ExportWorkspace(entries, ExportLimits{MaxEntries: e.MaxEntries, MaxBytes: e.MaxBytes})
	if err != nil {
		return RuntimeResult{}, fmt.Errorf("workspace export: %w", err)
	}
	baseline := make(map[string]workspaceEntry, len(entries))
	for _, entry := range entries {
		if entry.NodeType != "file" || entry.Open == nil {
			continue
		}
		r, openErr := entry.Open()
		if openErr != nil {
			_ = os.RemoveAll(workspace)
			return RuntimeResult{}, openErr
		}
		h := sha256.New()
		n, readErr := io.Copy(h, r)
		_ = r.Close()
		if readErr != nil {
			_ = os.RemoveAll(workspace)
			return RuntimeResult{}, readErr
		}
		baseline[filepath.ToSlash(entry.RelativePath)] = workspaceEntry{Size: n, SHA256: fmt.Sprintf("%x", h.Sum(nil))}
	}
	if e.Timeout <= 0 {
		e.Timeout = 15 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	result, err := e.Runtime.Run(execCtx, RuntimeRequest{TaskID: taskID, Image: image, Prompt: prompt, Workspace: workspace, AIToken: e.AIToken, Harness: e.Harness, Model: e.Model})
	if err != nil {
		_ = os.RemoveAll(workspace)
		return RuntimeResult{}, err
	}
	result.Workspace = workspace
	result.Diff, err = e.workspaceDiff(workspace, baseline)
	if err != nil {
		_ = os.RemoveAll(workspace)
		return RuntimeResult{}, fmt.Errorf("workspace diff: %w", err)
	}
	return result, nil
}
