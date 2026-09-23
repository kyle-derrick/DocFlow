package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentAIVolumeName 为承载平台 AI IPC socket 的 named volume 固定名
// （与 docker-compose.yml 的 docflow-agent-ipc 卷一致；compose 侧用
// `name:` 钉死实际卷名，避免 compose 项目前缀导致 backend 创建的 agent
// 容器与 backend 服务挂到两个不同卷）。backend 在该卷内监听
// /run/docflow-ipc/ai.sock，agent 容器只读挂载到 /run/docflow-ai。
const AgentAIVolumeName = "docflow-agent-ipc"

// DockerRuntime communicates with a local Docker Engine. Images must consume
// DOCFLOW_PROMPT_FILE and write results to DOCFLOW_WORKSPACE; nothing is uploaded.
type DockerRuntime struct {
	AllowedImages []string
	CPU           int64
	Memory        int64
	Client        *http.Client
	// AIVolume 为承载平台 AI IPC socket 的 named volume 名（compose 固定
	// 卷名 docflow-agent-ipc；空 = 不挂载）。设置且请求携带 AIToken 时：
	// 只读挂载到容器 /run/docflow-ai 并注入 DOCFLOW_AI_SOCK/DOCFLOW_AI_TOKEN
	// ——容器 NetworkMode=none 仍可经 unix socket 回调平台 AI。
	AIVolume string
}

func (d DockerRuntime) Run(ctx context.Context, req RuntimeRequest) (RuntimeResult, error) {
	if err := (Config{AllowedImages: d.AllowedImages}).ValidateImage(req.Image); err != nil {
		return RuntimeResult{}, err
	}
	if d.CPU < 1 || d.CPU > 64 || d.Memory < 1<<20 || d.Memory > 1<<40 {
		return RuntimeResult{}, errors.New("invalid docker resource limits")
	}
	if err := ctx.Err(); err != nil {
		return RuntimeResult{}, err
	}
	workspace, err := filepath.Abs(req.Workspace)
	if err != nil || req.Workspace == "" || filepath.Dir(workspace) == workspace {
		return RuntimeResult{}, errors.New("invalid agent workspace")
	}
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return RuntimeResult{}, errors.New("invalid agent workspace")
	}
	// The exported workspace is a temporary directory, not a host directory
	// chosen by the request. Never expose the daemon socket or host internals.
	if !strings.HasPrefix(filepath.Base(workspace), "docflow-agent-") || filepath.Dir(workspace) != os.TempDir() {
		return RuntimeResult{}, errors.New("agent workspace must be a temporary export")
	}
	promptDir, err := os.MkdirTemp("", "docflow-prompt-")
	if err != nil {
		return RuntimeResult{}, errors.New("unable to prepare agent prompt")
	}
	defer os.RemoveAll(promptDir)
	// prompt 文件 0644：backend 与 agent 容器用户 uid 不同（app vs
	// 65534），0600 会让挂载只读的 agent 读不到 prompt。
	if err := os.WriteFile(filepath.Join(promptDir, "prompt"), []byte(req.Prompt), 0644); err != nil {
		return RuntimeResult{}, errors.New("unable to prepare agent prompt")
	}
	client := d.Client
	if client == nil {
		client = dockerHTTPClient()
	}
	api := dockerAPI{client: client}
	env := []string{"DOCFLOW_PROMPT_FILE=/run/docflow/prompt", "DOCFLOW_WORKSPACE=/workspace"}
	binds := []string{workspace + ":/workspace:rw", promptDir + ":/run/docflow:ro"}
	// AI IPC 通道（核心卖点：NetworkMode=none 断网容器仍可调平台 AI）：
	// socket 所在 named volume 只读挂载 + 注入 socket 路径与一次性任务
	// 令牌；令牌随任务终态在网关注销，卷只读防容器篡改 socket。
	if d.AIVolume != "" && req.AIToken != "" {
		binds = append(binds, d.AIVolume+":/run/docflow-ai:ro")
		env = append(env, "DOCFLOW_AI_SOCK=/run/docflow-ai/ai.sock", "DOCFLOW_AI_TOKEN="+req.AIToken)
	}
	// Agent 执行引擎（agent.harness 终值）：claude-code/pi 时注入
	// DOCFLOW_HARNESS 供 entrypoint 按协议启动对应 harness（Claude Code
	// 走 /v1/messages、pi 走 /v1/chat/completions）；builtin/空 = 内置
	// 轻量 runner，走镜像默认路径不注入。与 AI 令牌解耦：无令牌（断网
	// 纯本地执行）的 harness 任务同样注入。
	if req.Harness != "" && req.Harness != HarnessBuiltin {
		env = append(env, "DOCFLOW_HARNESS="+req.Harness)
	}
	// 模型意图（任务创建时的 model 选择）：仅注入 env DOCFLOW_MODEL 记录
	// 用户意图——网关两透传端点刻意不透传 Model（按平台默认对话目标替换
	// 执行），runner 侧可据此展示/审计；空 = 未指定。
	if req.Model != "" {
		env = append(env, "DOCFLOW_MODEL="+req.Model)
	}
	create := map[string]any{
		"Image": req.Image, "WorkingDir": "/workspace", "Env": env,
		"User": "65534:65534", "NetworkDisabled": true, "AttachStdout": false, "AttachStderr": false,
		"HostConfig": map[string]any{
			"Binds":       binds,
			"NetworkMode": "none", "Privileged": false, "CapDrop": []string{"ALL"},
			"SecurityOpt": []string{"no-new-privileges:true"}, "ReadonlyRootfs": true,
			"Tmpfs":     map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=67108864,mode=1777"},
			"PidsLimit": 64, "NanoCpus": d.CPU * 1_000_000_000, "Memory": d.Memory,
			"MemorySwap": d.Memory, "AutoRemove": false,
		},
	}
	var created struct{ ID string }
	if err := api.call(ctx, http.MethodPost, "/v1.41/containers/create", create, &created); err != nil {
		return RuntimeResult{}, err
	}
	if created.ID == "" || strings.ContainsAny(created.ID, "/?\\") {
		return RuntimeResult{}, errors.New("docker returned an invalid container id")
	}
	id := created.ID
	// Cleanup must work after a timeout/cancellation, independently of run context.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = api.call(cleanupCtx, http.MethodPost, "/v1.41/containers/"+id+"/stop?t=1", nil, nil)
		_ = api.call(cleanupCtx, http.MethodDelete, "/v1.41/containers/"+id+"?force=true&v=true", nil, nil)
	}()
	if err := api.call(ctx, http.MethodPost, "/v1.41/containers/"+id+"/start", nil, nil); err != nil {
		return RuntimeResult{}, err
	}
	var wait struct {
		StatusCode int                       `json:"StatusCode"`
		Error      *struct{ Message string } `json:"Error"`
	}
	if err := api.call(ctx, http.MethodPost, "/v1.41/containers/"+id+"/wait?condition=not-running", nil, &wait); err != nil {
		return RuntimeResult{}, err
	}
	// Never store raw stdout/stderr: image output can echo the prompt or secrets.
	// Only emit a fixed status string, not daemon error bodies.
	if wait.Error != nil || wait.StatusCode != 0 {
		return RuntimeResult{}, fmt.Errorf("agent container exited with status %d", wait.StatusCode)
	}
	// Collect and discard logs so the Engine stream is closed, but never persist
	// their contents: container output may contain prompt material or secrets.
	if err := api.call(ctx, http.MethodGet, "/v1.41/containers/"+id+"/logs?stdout=1&stderr=1&timestamps=0", nil, nil); err != nil {
		return RuntimeResult{}, err
	}
	return RuntimeResult{Detail: "container completed; outputs discarded with temporary workspace; platform files were not updated"}, nil
}

type dockerAPI struct{ client *http.Client }

func (a dockerAPI) call(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return errors.New("invalid docker request")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return errors.New("invalid docker request")
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("docker daemon unavailable: %w", ErrRuntimeUnavailable)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("docker engine request failed (HTTP %d)", res.StatusCode)
	}
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(output); err != nil {
			return errors.New("invalid docker engine response")
		}
	}
	return nil
}
