package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportWorkspaceRejectsUnsafeAndLimits(t *testing.T) {
	open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("hello")), nil }
	if _, err := ExportWorkspace([]ExportEntry{{RelativePath: "../x", NodeType: "file", Size: 5, Open: open}}, ExportLimits{MaxEntries: 2, MaxBytes: 10}); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("path traversal: %v", err)
	}
	if _, err := ExportWorkspace([]ExportEntry{{RelativePath: "x", NodeType: "file", Size: 5, Open: open}}, ExportLimits{MaxEntries: 1, MaxBytes: 4}); err == nil {
		t.Fatal("size limit accepted")
	}
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "source"), link); err != nil {
		t.Skip("symlink unavailable")
	}
	if _, err := os.Stat(link); err != nil {
		t.Fatal(err)
	}
}

func TestFakeRuntimeWritesDryRunPlan(t *testing.T) {
	root := t.TempDir()
	got, err := (FakeRuntime{}).Run(context.Background(), RuntimeRequest{Workspace: root, Prompt: "make a plan"})
	if err != nil || !got.DryRun {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "agent-plan.json"))
	if err != nil || !strings.Contains(string(data), "dry_run") {
		t.Fatalf("plan=%q err=%v", data, err)
	}
}

func TestTransitionCancellationAndTimeout(t *testing.T) {
	if Transition(StatusQueued, StatusCancelled) != nil || Transition(StatusRunning, StatusCancelled) != nil {
		t.Fatal("cancel transition rejected")
	}
	if Transition(StatusCancelled, StatusRunning) == nil {
		t.Fatal("cancelled task restarted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (FakeRuntime{}).Run(ctx, RuntimeRequest{Workspace: t.TempDir()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

// captureRuntime 捕获 Executor 下发的 RuntimeRequest。
type captureRuntime struct{ req RuntimeRequest }

func (c *captureRuntime) Run(_ context.Context, req RuntimeRequest) (RuntimeResult, error) {
	c.req = req
	return RuntimeResult{DryRun: true}, nil
}

// TestExecutorPassesHarnessAndAIToken Executor 把 Harness/AIToken 原样
// 透传进 RuntimeRequest（harness 终值链路的 Executor 一环）。
func TestExecutorPassesHarnessAndAIToken(t *testing.T) {
	rt := &captureRuntime{}
	exec := Executor{Runtime: rt, MaxEntries: 10, MaxBytes: 1 << 20, AIToken: "tok-9", Harness: HarnessPi}
	if _, err := exec.Execute(context.Background(), "task-1", "docflow/agent:1.0.0", "p", nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rt.req.Harness != HarnessPi || rt.req.AIToken != "tok-9" || rt.req.TaskID != "task-1" {
		t.Fatalf("runtime request = %+v, want harness=%q token=%q", rt.req, HarnessPi, "tok-9")
	}
}

// 产物扫描忽略规则：node_modules/.git/dist 等依赖与缓存目录整目录
// 剪枝（不逐文件哈希），以 action=ignored 汇总条目上报；正常源码
// 文件照常进 diff。
func TestScanWorkspaceIgnoresDepsAndCaches(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<h1>ok</h1>")
	write("node_modules/left-pad/package.json", "{}")
	write("node_modules/dep2/index.js", "x")
	write(".git/config", "core")
	write("dist/bundle.js", "compiled")
	write("__pycache__/m.pyc", "bytecode")

	diff, err := scanWorkspace(root, map[string]workspaceEntry{})
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]WorkspaceDiff, len(diff))
	for _, d := range diff {
		byPath[d.Path] = d
	}
	if d, ok := byPath["index.html"]; !ok || d.Action != "added" {
		t.Fatalf("源码文件应进 diff：%v", byPath)
	}
	for _, noise := range []string{"node_modules/left-pad/package.json", "node_modules/dep2/index.js", ".git/config", "dist/bundle.js", "__pycache__/m.pyc"} {
		if _, ok := byPath[noise]; ok {
			t.Fatalf("依赖/缓存不应逐文件进 diff：%s", noise)
		}
	}
	if d, ok := byPath["node_modules/"]; !ok || d.Action != "ignored" || d.Size != 2 {
		t.Fatalf("node_modules 应以 ignored 汇总（2 个文件）：%+v", d)
	}
	if _, ok := byPath[".git/"]; !ok {
		t.Fatal(".git 应以 ignored 汇总")
	}
	// 单文件超限（8MB）以 ignored (oversize) 上报，不进 diff。
	write("big.bin", string(make([]byte, diffMaxSingleFile+1)))
	diff2, err := scanWorkspace(root, map[string]workspaceEntry{})
	if err != nil {
		t.Fatal(err)
	}
	foundOversize := false
	for _, d := range diff2 {
		if d.Action == "ignored" && strings.Contains(d.Path, "big.bin") {
			foundOversize = true
		}
		if d.Path == "big.bin" && d.Action != "ignored" {
			t.Fatalf("超限大文件不应正常进 diff：%+v", d)
		}
	}
	if !foundOversize {
		t.Fatal("超限文件应出现 ignored (oversize) 条目")
	}
}
