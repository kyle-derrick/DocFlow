// Package ai —— platformtools_test.go：内置平台文件工具声明完整性单测
// （5 工具、schema 必填参数、白名单判定、名称集合）。
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/settings"
)

// requiredOf 提取 schema 的 required 数组（断言用）。
func requiredOf(t *testing.T, params map[string]any) []string {
	t.Helper()
	raw, ok := params["required"]
	if !ok {
		return nil
	}
	list, _ := raw.([]string)
	if list == nil {
		// 手写声明里 required 为 []string，但经 map 断言可能成 []any 兜底。
		anyList, _ := raw.([]any)
		for _, v := range anyList {
			if s, ok := v.(string); ok {
				list = append(list, s)
			}
		}
	}
	return list
}

// TestPlatformToolsDeclarations 验证 5 个内置工具声明齐全、名称/描述/
// schema 结构合法且必填参数正确。
func TestPlatformToolsDeclarations(t *testing.T) {
	tools := PlatformTools()
	want := []string{"df_list_dir", "df_read_file", "df_write_file", "df_mkdir", "df_search"}
	if len(tools) != len(want) {
		t.Fatalf("工具数 = %d, want %d（%v）", len(tools), len(want), tools)
	}
	byName := map[string]PlatformTool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	for _, name := range want {
		tl, ok := byName[name]
		if !ok {
			t.Fatalf("缺少工具 %q（现有 %v）", name, byName)
		}
		if strings.TrimSpace(tl.Description) == "" {
			t.Fatalf("工具 %s 缺少 description", name)
		}
		if tl.Params["type"] != "object" {
			t.Fatalf("工具 %s params.type = %v, want object", name, tl.Params["type"])
		}
		if _, ok := tl.Params["properties"].(map[string]any); !ok {
			t.Fatalf("工具 %s 缺少 properties 对象", name)
		}
	}
	// 必填参数：path（read/write/mkdir）、content（write）、query（search）；
	// df_list_dir 的 path 可选。
	if got := requiredOf(t, byName["df_list_dir"].Params); got != nil {
		t.Fatalf("df_list_dir required = %v, want 无（path 可选）", got)
	}
	for name, wantReq := range map[string][]string{
		"df_read_file":  {"path"},
		"df_write_file": {"path", "content"},
		"df_mkdir":      {"path"},
		"df_search":     {"query"},
	} {
		got := requiredOf(t, byName[name].Params)
		if len(got) != len(wantReq) {
			t.Fatalf("%s required = %v, want %v", name, got, wantReq)
		}
		for i := range wantReq {
			if got[i] != wantReq[i] {
				t.Fatalf("%s required = %v, want %v", name, got, wantReq)
			}
		}
	}
	// schema 可序列化为合法 JSON（进上游请求体）。
	for _, tl := range tools {
		if _, err := json.Marshal(tl.Params); err != nil {
			t.Fatalf("工具 %s params 序列化失败: %v", tl.Name, err)
		}
	}
}

// TestPlatformToolNames 名称集合与声明一致。
func TestPlatformToolNames(t *testing.T) {
	names := PlatformToolNames()
	want := []string{"df_list_dir", "df_read_file", "df_write_file", "df_mkdir", "df_search"}
	if len(names) != len(want) {
		t.Fatalf("名称集合大小 = %d, want %d", len(names), len(want))
	}
	for _, n := range want {
		if !names[n] {
			t.Fatalf("名称集合缺少 %q", n)
		}
	}
	// 返回副本语义防御：内部声明不可经返回切片被外部篡改。
	first := PlatformTools()
	first[0].Description = "篡改"
	if PlatformTools()[0].Description == "篡改" {
		t.Fatalf("PlatformTools() 应返回副本，不受外部篡改影响")
	}
}

// TestPlatformToolWriteExtAllowed 写入扩展名白名单：文本类放行、二进制/
// 无扩展名拒绝、大小写不敏感。
func TestPlatformToolWriteExtAllowed(t *testing.T) {
	allow := []string{"a.md", "notes.txt", "main.go", "data.json", "config.yaml", "page.html", "style.css", "app.js", "index.ts", "script.py", "Main.java", "query.sql", "run.sh", "logo.svg", "data.csv", "flow.mermaid", "diagram.drawio", "board.excalidraw", "doc.dfdoc", "A.MD", "Chart.SVG"}
	for _, name := range allow {
		if !PlatformToolWriteExtAllowed(name) {
			t.Fatalf("%q 应在白名单内", name)
		}
	}
	deny := []string{"evil.exe", "lib.bin", "photo.png", "doc.docx", "sheet.xlsx", "book.pdf", "archive.zip", "font.ttf", "noext", "trailing.", ".htaccess"}
	for _, name := range deny {
		if PlatformToolWriteExtAllowed(name) {
			t.Fatalf("%q 不应在白名单内", name)
		}
	}
}

// TestChatPlatformToolsLoop 内置工具循环：PlatformTools+ToolExecutor 注入
// ChatRequest 后，上游请求 tools 含 df_*；模型请求工具时经回调执行并把
// 结果 JSON 回喂（role:tool），最终文本返回（非流式路径）。
func TestChatPlatformToolsLoop(t *testing.T) {
	var bodies []map[string]any
	rounds := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		rounds++
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_p1","function":{"name":"df_list_dir","arguments":"{}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"共 3 个文件"}}]}`))
	}))
	defer up.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{
			Providers:       []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Model: "gpt-test", Enabled: true}},
			DefaultProvider: "o1", Temperature: 0.3, MaxTokens: 128,
		}, nil
	})
	var execName string
	var execArgs json.RawMessage
	res, err := svc.Chat(context.Background(), ChatRequest{
		Messages:      []Message{{Role: "user", Content: "列出我的文件"}},
		PlatformTools: PlatformTools(),
		ToolExecutor: func(name string, args json.RawMessage) (string, error) {
			execName, execArgs = name, args
			return `{"ok":true,"entries":[{"name":"a.md","type":"file","size":3}],"count":1}`, nil
		},
	}, nil)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if res.Content != "共 3 个文件" {
		t.Fatalf("最终回复 = %q", res.Content)
	}
	if execName != "df_list_dir" {
		t.Fatalf("执行器收到的工具名 = %q", execName)
	}
	if strings.TrimSpace(string(execArgs)) != "{}" {
		t.Fatalf("执行器收到的参数 = %s", execArgs)
	}
	// 上游首请求 tools 含全部 df_*。
	if len(bodies) != 2 {
		t.Fatalf("上游轮数 = %d, want 2", len(bodies))
	}
	raw, _ := json.Marshal(bodies[0]["tools"])
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("解析 tools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Function.Name] = true
	}
	for _, want := range []string{"df_list_dir", "df_read_file", "df_write_file", "df_mkdir", "df_search"} {
		if !names[want] {
			t.Fatalf("上游 tools 缺少 %s（实际 %v）", want, names)
		}
	}
	// 第二轮请求含 role:tool 回喂结果。
	rawMsgs, _ := json.Marshal(bodies[1]["messages"])
	var msgs []map[string]any
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		t.Fatalf("解析 messages: %v", err)
	}
	var fedTool bool
	for _, m := range msgs {
		if m["role"] == "tool" && strings.Contains(fmt.Sprint(m["content"]), `"ok":true`) {
			fedTool = true
		}
	}
	if !fedTool {
		t.Fatalf("第二轮未回喂工具结果: %s", rawMsgs)
	}
}
