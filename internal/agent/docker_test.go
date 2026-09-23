package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

// recordingDockerTransport 捕获 /containers/create 载荷并返回固定成功
// 应答（create→{"Id"}、wait→StatusCode 0、其余 200 空对象），用于断言
// DockerRuntime 对 env/binds 的注入。
type recordingDockerTransport struct {
	mu      sync.Mutex
	creates []map[string]any
}

func (r *recordingDockerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body := []byte("{}")
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(req.URL.Path, "/containers/create") {
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err == nil {
				r.creates = append(r.creates, payload)
			}
			body = []byte(`{"Id":"abc123"}`)
		} else if strings.HasSuffix(req.URL.Path, "/wait") {
			body = []byte(`{"StatusCode":0}`)
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), Request: req,
	}, nil
}

func (r *recordingDockerTransport) lastCreate(t *testing.T) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.creates) == 0 {
		t.Fatal("no /containers/create request captured")
	}
	return r.creates[len(r.creates)-1]
}

// runDockerWithFakeEngine 在临时 workspace 上跑一次 DockerRuntime（假
// Engine），返回捕获的 create 载荷。
func runDockerWithFakeEngine(t *testing.T, volume, token string) map[string]any {
	t.Helper()
	transport := &recordingDockerTransport{}
	workspace, err := os.MkdirTemp("", "docflow-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	rt := DockerRuntime{
		AllowedImages: []string{"docflow/agent:1.0.0"},
		CPU:           1, Memory: 64 << 20,
		Client:   &http.Client{Transport: transport},
		AIVolume: volume,
	}
	req := RuntimeRequest{TaskID: "t", Image: "docflow/agent:1.0.0", Prompt: "p", Workspace: workspace, AIToken: token}
	if _, err := rt.Run(context.Background(), req); err != nil {
		t.Fatalf("docker run against fake engine: %v", err)
	}
	return transport.lastCreate(t)
}

func payloadStrings(t *testing.T, payload map[string]any, key string) []string {
	t.Helper()
	raw, ok := payload[key].([]any)
	if !ok {
		t.Fatalf("payload[%s] missing: %+v", key, payload)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestDockerRuntimeAIEnvInjection 令牌存在（agent.allow_ai=true 场景）：
// 注入 DOCFLOW_AI_SOCK/DOCFLOW_AI_TOKEN、挂载 AI 卷（:ro），
// NetworkMode 保持 none（断网容器经 unix socket 回调平台 AI）。
func TestDockerRuntimeAIEnvInjection(t *testing.T) {
	payload := runDockerWithFakeEngine(t, "docflow-agent-ipc", "tok-123")
	env := payloadStrings(t, payload, "Env")
	has := func(v string) bool {
		for _, e := range env {
			if e == v {
				return true
			}
		}
		return false
	}
	if !has("DOCFLOW_AI_SOCK=/run/docflow-ai/ai.sock") || !has("DOCFLOW_AI_TOKEN=tok-123") {
		t.Fatalf("AI env not injected: %v", env)
	}
	if !has("DOCFLOW_PROMPT_FILE=/run/docflow/prompt") || !has("DOCFLOW_WORKSPACE=/workspace") {
		t.Fatalf("base env missing: %v", env)
	}
	host, ok := payload["HostConfig"].(map[string]any)
	if !ok {
		t.Fatal("HostConfig missing")
	}
	binds := payloadStrings(t, host, "Binds")
	found := false
	for _, b := range binds {
		if b == "docflow-agent-ipc:/run/docflow-ai:ro" {
			found = true
		}
	}
	if !found {
		t.Fatalf("AI volume bind missing: %v", binds)
	}
	if host["NetworkMode"] != "none" {
		t.Fatalf("NetworkMode = %v, want none", host["NetworkMode"])
	}
}

// TestDockerRuntimeNoTokenNoInjection 无令牌（agent.allow_ai=false 场景）：
// 不注入 AI env、不挂 AI 卷；网络与基础 env 不变。
func TestDockerRuntimeNoTokenNoInjection(t *testing.T) {
	payload := runDockerWithFakeEngine(t, "docflow-agent-ipc", "")
	for _, e := range payloadStrings(t, payload, "Env") {
		if strings.HasPrefix(e, "DOCFLOW_AI_") {
			t.Fatalf("AI env must not be injected without token: %v", e)
		}
	}
	host, _ := payload["HostConfig"].(map[string]any)
	if host == nil {
		t.Fatal("HostConfig missing")
	}
	for _, b := range payloadStrings(t, host, "Binds") {
		if strings.Contains(b, "/run/docflow-ai") {
			t.Fatalf("AI volume must not be mounted without token: %v", b)
		}
	}
	if host["NetworkMode"] != "none" {
		t.Fatalf("NetworkMode = %v, want none", host["NetworkMode"])
	}
}
