package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"time"

	"github.com/docflow/docflow/internal/agent"
	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type agentTaskRow struct{ agent.Task }

func (agentTaskRow) TableName() string { return "agent_tasks" }

type agentLogRow struct {
	ID              int64
	TaskID          uuid.UUID
	Stream, Content string
	CreatedAt       time.Time
}

func (agentLogRow) TableName() string     { return "agent_task_logs" }
func (h *Handler) SetAgentDB(db *gorm.DB) { h.agentDB = db }

func (h *Handler) appendAgentLog(taskID uuid.UUID, stream, content string) {
	if h.agentDB == nil || content == "" {
		return
	}
	// 日志只记录固定的运行时状态，不写入 prompt、令牌或数据库凭据。
	h.agentDB.Table("agent_task_logs").Create(&agentLogRow{TaskID: taskID, Stream: stream, Content: content, CreatedAt: time.Now().UTC()})
}

func (h *Handler) executeAgentTask(taskID uuid.UUID, aiToken, harness string) {
	if h.agentDB == nil {
		return
	}
	// 任务令牌随执行结束注销（succeeded/failed/cancelled 均终态）；
	// queued→cancelled（transitionAgentTask）路径单独注销。
	if h.agentAI != nil {
		defer h.agentAI.Revoke(taskID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.agentCancelMu.Lock()
	if h.agentCancels == nil {
		h.agentCancels = make(map[uuid.UUID]context.CancelFunc)
	}
	h.agentCancels[taskID] = cancel
	h.agentCancelMu.Unlock()
	defer func() { h.agentCancelMu.Lock(); delete(h.agentCancels, taskID); h.agentCancelMu.Unlock(); cancel() }()
	var task agent.Task
	if err := h.agentDB.Table("agent_tasks").Where("id = ? AND status = ?", taskID, agent.StatusQueued).First(&task).Error; err != nil {
		return
	}
	if err := agent.Transition(task.Status, agent.StatusRunning); err != nil {
		return
	}
	now := time.Now().UTC()
	if err := h.agentDB.Table("agent_tasks").Where("id = ? AND status = ?", taskID, agent.StatusQueued).Updates(map[string]any{"status": agent.StatusRunning, "started_at": now}).Error; err != nil {
		return
	}
	h.appendAgentLog(taskID, "system", "task started")
	cfg := agent.DefaultConfig()
	if v, ok := h.agentSetting("agent.runtime").(string); ok && v != "" {
		cfg.Runtime = v
	}
	if v, ok := h.agentSetting("agent.allowed_images").(string); ok {
		if list := splitAgentImages(v); len(list) > 0 {
			// 管理员配置非空时整体覆盖默认镜像（DefaultConfig 预填
			// docflow/agent:1.0.0；配置的首个镜像即未指定时的任务默认）。
			cfg.AllowedImages = list
		}
	}
	if v, ok := h.agentSetting("agent.max_cpu").(int64); ok {
		cfg.MaxCPU = v
	}
	if v, ok := h.agentSetting("agent.max_memory_bytes").(int64); ok {
		cfg.MaxMemoryBytes = v
	}
	if v, ok := h.agentSetting("agent.default_timeout_seconds").(int64); ok {
		cfg.DefaultTimeoutSeconds = int(v)
	}
	if cfg.Runtime != "fake" && cfg.Runtime != "docker" {
		errText := "invalid agent runtime"
		h.finishAgentTask(taskID, agent.StatusFailed, errText)
		return
	}
	snap, err := h.files.GetSnapshot(task.UserID, task.SnapshotID)
	if err != nil {
		h.finishAgentTask(taskID, agent.StatusFailed, err.Error())
		return
	}
	entries := make([]agent.ExportEntry, 0, len(snap.Entries))
	for _, e := range snap.Entries {
		entry := agent.ExportEntry{RelativePath: e.RelativePath, NodeType: e.NodeType, Size: e.Size}
		if e.NodeType == "file" && e.SourceFileID != nil && e.SourceVersionID != nil {
			fileID, versionID := *e.SourceFileID, *e.SourceVersionID
			entry.Open = func() (io.ReadCloser, error) {
				_, blob, openErr := h.files.GetVersionBlob(fileID, versionID)
				if openErr != nil {
					return nil, openErr
				}
				if blob.Status != files.BlobStatusAvailable {
					return nil, fmt.Errorf("blob unavailable")
				}
				if h.storage == nil {
					return nil, fmt.Errorf("file storage unavailable")
				}
				return h.storage.Read(blob.StorageKey)
			}
		}
		entries = append(entries, entry)
	}
	var runtime agent.Runtime = agent.FakeRuntime{}
	if cfg.Runtime == "docker" {
		docker := agent.DockerRuntime{AllowedImages: cfg.AllowedImages, CPU: cfg.MaxCPU, Memory: cfg.MaxMemoryBytes}
		// AI IPC 通道：令牌已签发（agent.allow_ai=true 且网关已装配）时
		// 挂载 socket 卷并注入 env；否则容器纯断网运行（runner 降级）。
		if aiToken != "" {
			docker.AIVolume = agent.AgentAIVolumeName
		}
		runtime = docker
	}
	syncMode := "git"
	if v, ok := h.agentSetting("agent.sync_mode").(string); ok && v != "" {
		syncMode = v
	}
	exec := agent.Executor{Runtime: runtime, Timeout: time.Duration(cfg.DefaultTimeoutSeconds) * time.Second, MaxEntries: 10000, MaxBytes: 512 << 20, AIToken: aiToken, Harness: harness, SyncMode: syncMode}
	result, runErr := exec.Execute(ctx, task.ID.String(), task.Image, task.Prompt, entries)
	if runErr != nil {
		status := agent.StatusFailed
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			status = agent.StatusCancelled
		}
		h.finishAgentTask(taskID, status, runErr.Error())
		return
	}
	diffJSON, _ := json.Marshal(result.Diff)
	expires := time.Now().UTC().Add(24 * time.Hour)
	if result.Workspace == "" {
		expires = time.Time{}
	}
	h.agentDB.Table("agent_tasks").Where("id = ? AND status = ?", taskID, agent.StatusRunning).Updates(map[string]any{"status": agent.StatusSucceeded, "finished_at": time.Now().UTC(), "error": "", "workspace_path": result.Workspace, "workspace_expires_at": expires, "diff_json": string(diffJSON)})
	h.appendAgentLog(taskID, "stdout", result.Detail)
}

func (h *Handler) finishAgentTask(id uuid.UUID, status, errText string) {
	h.agentDB.Table("agent_tasks").Where("id = ? AND status = ?", id, agent.StatusRunning).Updates(map[string]any{"status": status, "finished_at": time.Now().UTC(), "error": errText})
	h.appendAgentLog(id, "stderr", errText)
}

// agentEnabled 判定 Agent 创作舱是否可用：
//   - AI 总开关（ai.enabled + 存在启用中的 Provider）关闭时 Agent 一并
//     不可用——Agent 任务依赖 AI 生态，关闭即整体下线（false）；
//   - agent.enabled 未配置时默认启用（此前默认 false 需管理员手动开启；
//     GetAll 恒返回定义键，未落库行取定义默认值 true，见 settings 包）；
//   - 显式配置 false 时关闭。
func (h *Handler) agentEnabled() bool {
	if h.aiSvc == nil || !h.aiSvc.EnabledNow() {
		return false
	}
	if h.settings == nil {
		return false
	}
	if v, err := h.settings.GetBool("agent.enabled"); err == nil {
		return v
	}
	// GetBool 不可用（键缺失/读取失败）时回退遍历 GetAll：键缺失 = 未配置，
	// 按默认启用处理；显式 false 以存储值为准。
	rows, e := h.settings.GetAll()
	if e != nil {
		return false
	}
	for _, row := range rows {
		if row.Key == "agent.enabled" {
			v, ok := row.Value.(bool)
			return ok && v
		}
	}
	return true
}
func (h *Handler) agentSetting(key string) any {
	rows, e := h.settings.GetAll()
	if e != nil {
		return nil
	}
	for _, row := range rows {
		if row.Key == key {
			return row.Value
		}
	}
	return nil
}

// splitAgentImages 解析 agent.allowed_images（逗号分隔，trim 空白，丢弃
// 空段）；全部为空/未配置返回 nil（沿用 DefaultConfig 的默认镜像）。
func splitAgentImages(raw string) []string {
	var out []string
	for _, image := range strings.Split(raw, ",") {
		if image = strings.TrimSpace(image); image != "" {
			out = append(out, image)
		}
	}
	return out
}

// resolveAgentHarness 在任务创建时解析 harness 终值（agent.harness）：
// 配置显式值（非 auto/空）原样；auto/缺省按任务归属用户解析平台默认
// 对话 Provider 的 kind 路由——与网关两透传端点的 kind 要求一致
// （anthropic→claude-code 经 /v1/messages、openai 兼容→pi 经
// /v1/chat/completions）；解析失败/其余 kind（含 mock）→builtin。
func (h *Handler) resolveAgentHarness(user uuid.UUID) string {
	configured := ""
	if v, ok := h.agentSetting("agent.harness").(string); ok {
		configured = v
	}
	return agentHarnessTerminal(configured, func() (string, error) {
		if h.aiSvc == nil {
			return "", errors.New("ai service is not configured")
		}
		target, err := h.aiSvc.ForUser(user).ResolveChatTargetFor("", "", settings.AIScenarioChat)
		if err != nil {
			return "", err
		}
		return target.Provider.Kind, nil
	})
}

// agentHarnessTerminal 为 harness 终值解析的纯函数（便于单测）：显式值
// 原样透传；auto/空时经 resolveKind 解析 Provider kind 路由，失败或未知
// kind 一律 builtin（内置轻量 runner 兜底）。
func agentHarnessTerminal(configured string, resolveKind func() (string, error)) string {
	if c := strings.TrimSpace(configured); c != "" && c != agent.HarnessAuto {
		return c
	}
	kind, err := resolveKind()
	if err != nil {
		return agent.HarnessBuiltin
	}
	switch kind {
	case settings.AIKindAnthropic:
		return agent.HarnessClaudeCode
	case settings.AIKindOpenAICompatible:
		return agent.HarnessPi
	default:
		return agent.HarnessBuiltin
	}
}

func (h *Handler) requireAgent(c *gin.Context) bool {
	if !h.agentEnabled() {
		// 友好文案（曾被用户看到英文 "agent is disabled"）：指明开启路径。
		c.JSON(http.StatusNotFound, gin.H{"error": "AI 创作舱未启用：请管理员在「平台管理 → AI 创作舱」开启后重试"})
		return false
	}
	return true
}

// agentConfig/putAgentConfig 为管理员配置接口，**不**经 requireAgent——
// 即便 agent.enabled 关闭（或 AI 总开关关闭连带 Agent 不可用），配置读取
// 也不可被拦截（否则关闭后将永远无法重新开启，此前表现为面板一直
// 「加载中…」+ 404 "agent is disabled"）。
// 只有任务执行类接口（创建/取消/应用/放弃/回滚/日志）才 requireAgent。
func (h *Handler) agentConfig(c *gin.Context) {
	if h.settings == nil {
		c.JSON(500, gin.H{"error": "settings service is not configured"})
		return
	}
	keys := []string{"agent.enabled", "agent.runtime", "agent.allowed_images", "agent.max_concurrent", "agent.default_timeout_seconds", "agent.max_cpu", "agent.max_memory_bytes", "agent.network_mode", "agent.mcp_callback_base_url", "agent.allow_ai", "agent.ai_max_calls", "agent.sync_mode", "agent.harness"}
	out := gin.H{}
	for _, k := range keys {
		v := h.agentSetting(k)
		if v == nil {
			c.JSON(500, gin.H{"error": "unable to load agent settings"})
			return
		}
		if k == "agent.allowed_images" {
			if s, ok := v.(string); ok {
				out["allowed_images"] = strings.Split(s, ",")
			}
		} else {
			out[strings.TrimPrefix(k, "agent.")] = v
		}
	}
	c.JSON(200, out)
}
func (h *Handler) putAgentConfig(c *gin.Context) {
	if h.settings == nil {
		c.JSON(500, gin.H{"error": "settings service is not configured"})
		return
	}
	var req map[string]any
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	for key, value := range req {
		if !strings.HasPrefix(key, "agent.") {
			key = "agent." + key
		}
		if _, err := h.settings.Set(key, value, actor); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
	}
	if !h.agentEnabled() {
		c.JSON(200, gin.H{"enabled": false})
		return
	}
	h.agentConfig(c)
}
func (h *Handler) createAgentTask(c *gin.Context) {
	if !h.requireAgent(c) || h.agentDB == nil {
		return
	}
	root, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req struct {
		Prompt, Image string
		Timeout       int `json:"timeout_seconds"`
	}
	if c.ShouldBindJSON(&req) != nil || strings.TrimSpace(req.Prompt) == "" {
		c.JSON(400, gin.H{"error": "prompt is required"})
		return
	}
	actor := userID(c)
	snap, e := h.files.CreateSnapshot(actor, root, "AI Agent task")
	if h.fileError(c, e) {
		return
	}
	var folder files.File
	if e = h.agentDB.Where("id = ?", root).First(&folder).Error; e != nil {
		c.JSON(404, gin.H{"error": "folder not found"})
		return
	}
	cfg := agent.DefaultConfig()
	if v, ok := h.agentSetting("agent.runtime").(string); ok {
		cfg.Runtime = v
	}
	if v, ok := h.agentSetting("agent.allowed_images").(string); ok {
		if list := splitAgentImages(v); len(list) > 0 {
			cfg.AllowedImages = list
		}
	}
	if req.Image == "" && len(cfg.AllowedImages) > 0 {
		req.Image = cfg.AllowedImages[0]
	}
	if e = cfg.ValidateImage(req.Image); e != nil {
		c.JSON(400, gin.H{"error": e.Error(), "code": "AGENT_IMAGE_NOT_ALLOWED"})
		return
	}
	task := agent.Task{ID: uuid.New(), UserID: actor, SpaceID: folder.SpaceID, RootFolderID: root, SnapshotID: snap.ID, Image: req.Image, Status: agent.StatusQueued, Prompt: req.Prompt, CreatedAt: time.Now().UTC()}
	if e = h.agentDB.Table("agent_tasks").Create(&task).Error; e != nil {
		c.JSON(500, gin.H{"error": "unable to create agent task"})
		return
	}
	// AI IPC 令牌（agent.allow_ai，默认 true）：任务创建时签发并注册到
	// socket 网关（limit 取 agent.ai_max_calls），经 Executor→RuntimeRequest
	// 注入容器 env；关闭时容器纯断网运行（runner 降级执行 ```run 块）。
	var aiToken string
	if h.agentAI != nil {
		if allow, aerr := h.settings.GetBool("agent.allow_ai"); aerr == nil && allow {
			limit := int64(40)
			if v, ok := h.agentSetting("agent.ai_max_calls").(int64); ok && v > 0 {
				limit = v
			}
			aiToken = h.agentAI.Register(task.ID, actor, limit)
		}
	}
	// Agent 执行引擎终值（agent.harness）：创建任务时解析，经
	// Executor→RuntimeRequest 注入容器 env DOCFLOW_HARNESS。
	harness := h.resolveAgentHarness(actor)
	go h.executeAgentTask(task.ID, aiToken, harness)
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: "agent.task.create", ResourceType: audit.ResourceFolder, ResourceID: root.String()})
	c.JSON(201, task)
}
func (h *Handler) listAgentTasks(c *gin.Context) {
	if !h.requireAgent(c) || h.agentDB == nil {
		return
	}
	actor := userID(c)
	var rows []agent.Task
	if e := h.agentDB.Table("agent_tasks").Where("user_id = ?", actor).Order("created_at DESC").Limit(100).Find(&rows).Error; e != nil {
		c.JSON(500, gin.H{"error": "unable to list agent tasks"})
		return
	}
	c.JSON(200, gin.H{"tasks": rows})
}
func (h *Handler) listAgentTaskLogs(c *gin.Context) {
	if !h.requireAgent(c) || h.agentDB == nil {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	var task agent.Task
	if e := h.agentDB.Table("agent_tasks").Where("id = ? AND user_id = ?", id, actor).First(&task).Error; e != nil {
		c.JSON(404, gin.H{"error": "agent task not found"})
		return
	}
	var logs []agentLogRow
	if e := h.agentDB.Table("agent_task_logs").Where("task_id = ?", id).Order("created_at").Find(&logs).Error; e != nil {
		c.JSON(500, gin.H{"error": "unable to list agent task logs"})
		return
	}
	c.JSON(200, gin.H{"logs": logs})
}

func (h *Handler) getAgentTask(c *gin.Context) {
	if !h.requireAgent(c) || h.agentDB == nil {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	var t agent.Task
	if e := h.agentDB.Table("agent_tasks").Where("id = ? AND user_id = ?", id, actor).First(&t).Error; e != nil {
		c.JSON(404, gin.H{"error": "agent task not found"})
		return
	}
	var logs []agentLogRow
	h.agentDB.Table("agent_task_logs").Where("task_id = ?", id).Order("created_at").Find(&logs)
	dryRun := t.Status == agent.StatusQueued || t.Status == agent.StatusRunning
	if v, ok := h.agentSetting("agent.runtime").(string); ok && v == "fake" {
		dryRun = true
	}
	var diff []agent.WorkspaceDiff
	if t.DiffJSON != "" {
		_ = json.Unmarshal([]byte(t.DiffJSON), &diff)
	}
	c.JSON(200, gin.H{"task": t, "logs": logs, "dry_run": dryRun, "diff": diff})
}

func (h *Handler) getAgentTaskDiff(c *gin.Context) {
	if !h.requireAgent(c) || h.agentDB == nil {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var t agent.Task
	if err := h.agentDB.Table("agent_tasks").Where("id = ? AND user_id = ?", id, userID(c)).First(&t).Error; err != nil {
		c.JSON(404, gin.H{"error": "agent task not found"})
		return
	}
	var diff []agent.WorkspaceDiff
	if t.DiffJSON != "" {
		_ = json.Unmarshal([]byte(t.DiffJSON), &diff)
	}
	c.JSON(200, gin.H{"diff": diff, "expires_at": t.WorkspaceExpiresAt})
}

func (h *Handler) applyAgentTask(c *gin.Context) {
	if !h.requireAgent(c) || h.agentDB == nil {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var t agent.Task
	if err := h.agentDB.Table("agent_tasks").Where("id = ? AND user_id = ?", id, userID(c)).First(&t).Error; err != nil {
		c.JSON(404, gin.H{"error": "agent task not found"})
		return
	}
	var req struct {
		SelectedPaths      []string  `json:"selected_paths"`
		AllowDeletes       bool      `json:"allow_deletes"`
		ExpectedSnapshotID uuid.UUID `json:"expected_snapshot_id"`
		ExpectedDiffHash   string    `json:"expected_diff_hash"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "invalid request"})
		return
	}
	if t.Status == agent.StatusApplied {
		c.JSON(http.StatusConflict, gin.H{"error": "agent task already applied", "results": json.RawMessage(t.ApplyResultJSON)})
		return
	}
	if t.Status != agent.StatusSucceeded || t.WorkspacePath == "" || t.WorkspaceExpiresAt == nil || time.Now().UTC().After(*t.WorkspaceExpiresAt) {
		c.JSON(http.StatusConflict, gin.H{"error": "task is not writable or workspace expired"})
		return
	}
	if req.ExpectedSnapshotID != t.SnapshotID {
		c.JSON(http.StatusConflict, gin.H{"error": "snapshot mismatch"})
		return
	}
	if req.ExpectedDiffHash == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "expected_diff_hash is required"})
		return
	}
	var diff []agent.WorkspaceDiff
	if json.Unmarshal([]byte(t.DiffJSON), &diff) != nil {
		c.JSON(500, gin.H{"error": "invalid persisted diff"})
		return
	}
	canonical, _ := json.Marshal(diff)
	sum := sha256.Sum256(canonical)
	actualHash := hex.EncodeToString(sum[:])
	if !strings.EqualFold(req.ExpectedDiffHash, actualHash) {
		c.JSON(http.StatusConflict, gin.H{"error": "diff hash mismatch"})
		return
	}
	current, err := agent.ScanWorkspace(t.WorkspacePath)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "workspace rescan rejected: " + err.Error()})
		return
	}
	byPath := map[string]agent.WorkspaceDiff{}
	for _, d := range diff {
		byPath[d.Path] = d
	}
	selected := map[string]bool{}
	for _, p := range req.SelectedPaths {
		if _, err := agent.SafeJoin(t.WorkspacePath, p); err != nil || strings.Contains(p, "\\") {
			c.JSON(400, gin.H{"error": "unsafe selected path"})
			return
		}
		selected[p] = true
	}
	snap, err := h.files.GetSnapshot(userID(c), t.SnapshotID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "snapshot unavailable"})
		return
	}
	entries := map[string]files.DirectorySnapshotEntry{}
	for _, e := range snap.Entries {
		entries[e.RelativePath] = e
	}
	type item struct {
		Path   string `json:"path"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]item, 0, len(selected))
	var preflight []string
	for p := range selected {
		d, exists := byPath[p]
		f, inWorkspace := current[p]
		if !exists {
			results = append(results, item{p, "failed", "path is not in task diff"})
			continue
		}
		if d.Action == "deleted" {
			if !req.AllowDeletes {
				results = append(results, item{p, "skipped", "deletion requires confirmation"})
				continue
			}
			if inWorkspace {
				results = append(results, item{p, "failed", "deleted file reappeared"})
			}
			continue
		}
		if !inWorkspace || d.SHA256 != f.SHA256 || d.Size != f.Size {
			results = append(results, item{p, "failed", "workspace changed since preview"})
			continue
		}
		if d.Action != "added" && d.Action != "modified" {
			results = append(results, item{p, "failed", "unsupported action"})
			continue
		}
		if e, ok := entries[p]; ok {
			if e.NodeType != "file" || e.SourceFileID == nil || d.Action != "modified" {
				results = append(results, item{p, "failed", "target type or action conflict"})
				continue
			}
			live, le := h.files.GetFileByID(*e.SourceFileID)
			if le != nil || live.CurrentVersionID == nil || e.SourceVersionID == nil || *live.CurrentVersionID != *e.SourceVersionID {
				results = append(results, item{p, "failed", "target changed since snapshot"})
				continue
			}
			preflight = append(preflight, p)
			continue
		}
		if d.Action != "added" {
			results = append(results, item{p, "failed", "target missing since snapshot"})
			continue
		}
		parent := path.Dir(p)
		if parent == "." {
			parent = ""
		}
		if parent != "" {
			if pe, ok := entries[parent]; !ok || pe.NodeType != "folder" || pe.SourceFileID == nil {
				results = append(results, item{p, "failed", "parent directory is not in snapshot"})
				continue
			}
		}
		preflight = append(preflight, p)
	}
	// All selected paths are validated before any mutation. Upload/replace remains per-item transactional.
	for _, p := range preflight {
		data, readErr := os.ReadFile(filepath.Join(t.WorkspacePath, filepath.FromSlash(p)))
		if readErr != nil {
			results = append(results, item{p, "failed", readErr.Error()})
			continue
		}
		if e, ok := entries[p]; ok && e.SourceFileID != nil {
			s, se := h.uploads.StartReplace(userID(c), *e.SourceFileID, int64(len(data)), "")
			if se == nil {
				_, se = h.uploads.Append(s.ID, 0, bytes.NewReader(data))
				if se == nil {
					_, se = h.uploads.Complete(s.ID)
				}
			}
			if se != nil {
				results = append(results, item{p, "failed", se.Error()})
			} else {
				results = append(results, item{Path: p, Status: "applied"})
			}
		} else {
			parent := path.Dir(p)
			parentID := t.RootFolderID
			if parent != "." && parent != "" {
				parentID = *entries[parent].SourceFileID
			}
			if _, se := h.uploads.UploadBytes(userID(c), parentID, path.Base(p), data); se != nil {
				results = append(results, item{p, "failed", se.Error()})
			} else {
				results = append(results, item{Path: p, Status: "applied"})
			}
		}
	}
	if req.AllowDeletes {
		for p := range selected {
			if d, ok := byPath[p]; ok && d.Action == "deleted" {
				if _, exists := current[p]; exists {
					continue
				}
				if e, ok := entries[p]; ok && e.SourceFileID != nil {
					live, le := h.files.GetFileByID(*e.SourceFileID)
					if le != nil || live.Type != e.NodeType || (e.NodeType == "file" && (live.CurrentVersionID == nil || e.SourceVersionID == nil || *live.CurrentVersionID != *e.SourceVersionID)) {
						results = append(results, item{p, "failed", "target changed since snapshot"})
						continue
					}
					if de := h.files.Delete(userID(c), *e.SourceFileID); de != nil {
						results = append(results, item{p, "failed", de.Error()})
					} else {
						results = append(results, item{Path: p, Status: "deleted"})
					}
				}
			}
		}
	}
	encoded, _ := json.Marshal(results)
	now := time.Now().UTC()
	status := agent.StatusApplied
	for _, r := range results {
		if r.Status == "failed" {
			status = agent.StatusSucceeded
			break
		}
	}
	h.agentDB.Table("agent_tasks").Where("id = ? AND status = ?", id, agent.StatusSucceeded).Updates(map[string]any{"status": status, "apply_result_json": string(encoded), "apply_diff_hash": actualHash, "applied_at": now, "workspace_path": "", "workspace_expires_at": nil})
	if status == agent.StatusApplied {
		_ = os.RemoveAll(t.WorkspacePath)
	}
	c.JSON(200, gin.H{"status": status, "results": results, "rollback_hint": "如需回滚，请使用任务快照回滚；上传版本与删除均保留可恢复记录"})
}

func (h *Handler) discardAgentTask(c *gin.Context) {
	if !h.requireAgent(c) || h.agentDB == nil {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var t agent.Task
	if err := h.agentDB.Table("agent_tasks").Where("id = ? AND user_id = ?", id, userID(c)).First(&t).Error; err != nil {
		c.JSON(404, gin.H{"error": "agent task not found"})
		return
	}
	if t.WorkspacePath != "" {
		_ = os.RemoveAll(t.WorkspacePath)
	}
	h.agentDB.Table("agent_tasks").Where("id = ?", id).Updates(map[string]any{"workspace_path": "", "workspace_expires_at": nil, "diff_json": "[]"})
	c.JSON(200, gin.H{"status": "discarded"})
}
func (h *Handler) cancelAgentTask(c *gin.Context) { h.transitionAgentTask(c, agent.StatusCancelled) }
func (h *Handler) rollbackAgentTask(c *gin.Context) {
	if !h.requireAgent(c) || h.agentDB == nil {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	var t agent.Task
	if e := h.agentDB.Table("agent_tasks").Where("id = ? AND user_id = ?", id, actor).First(&t).Error; e != nil {
		c.JSON(404, gin.H{"error": "agent task not found"})
		return
	}
	if e := agent.Transition(t.Status, agent.StatusRolledBack); e != nil {
		c.JSON(409, gin.H{"error": e.Error()})
		return
	}
	if _, e := h.files.RestoreSnapshotVersionsOnly(actor, t.SnapshotID); e != nil {
		c.JSON(409, gin.H{"error": "rollback failed"})
		return
	}
	now := time.Now().UTC()
	if e := h.agentDB.Table("agent_tasks").Where("id = ?", id).Updates(map[string]any{"status": agent.StatusRolledBack, "finished_at": now}).Error; e != nil {
		c.JSON(500, gin.H{"error": "unable to update task"})
		return
	}
	c.JSON(200, gin.H{"status": agent.StatusRolledBack})
}
func (h *Handler) transitionAgentTask(c *gin.Context, to string) {
	if !h.requireAgent(c) {
		return
	}
	if h.agentDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "agent database is not configured"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	var t agent.Task
	if e := h.agentDB.Table("agent_tasks").Where("id = ? AND user_id = ?", id, actor).First(&t).Error; e != nil {
		c.JSON(404, gin.H{"error": "agent task not found"})
		return
	}
	if e := agent.Transition(t.Status, to); e != nil {
		c.JSON(409, gin.H{"error": e.Error()})
		return
	}
	if e := h.agentDB.Table("agent_tasks").Where("id = ?", id).Updates(map[string]any{"status": to, "finished_at": time.Now().UTC()}).Error; e != nil {
		c.JSON(500, gin.H{"error": "unable to update task"})
		return
	}
	if to == agent.StatusCancelled {
		// queued→cancelled：执行 goroutine 尚未启动（或已在收尾），
		// 该路径令牌由此处注销兜底（Revoke 幂等）。
		if h.agentAI != nil {
			h.agentAI.Revoke(id)
		}
		h.agentCancelMu.Lock()
		cancel := h.agentCancels[id]
		h.agentCancelMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	c.JSON(200, gin.H{"status": to})
}
