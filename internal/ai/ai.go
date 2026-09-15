// Package ai 提供 AI 文件摘要（设计 v2 AI 能力的可落地子集）：
// OpenAI 兼容 /chat/completions 接口（AI_BASE_URL 可指向任意兼容网关），
// system 提示要求与文件相同语言、300 字内，max_tokens 500，超时 60s。
// 文本判定复用 search.IsTextIndexable 语义（调用方在读取内容前判定）。
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrDisabled AI 摘要未启用（AI_ENABLED=false，HTTP 503 AI_DISABLED）。
	ErrDisabled = errors.New("ai summary disabled")
	// ErrTooLarge 内容超过摘要输入上限（100KB，HTTP 413）。
	ErrTooLarge = errors.New("content too large for ai summary")
	// ErrUpstream 上游请求失败（非 2xx / 超时 / 网络/解析错误，HTTP 502）。
	ErrUpstream = errors.New("ai upstream request failed")
)

// MaxInputBytes 摘要输入的内容大小上限：超出拒绝（不截断——截断会静默
// 丢失文件后半内容，摘要误导性更强）。
const MaxInputBytes = 100 << 10 // 100 KiB

// DefaultTimeout 单次摘要请求超时。
const DefaultTimeout = 60 * time.Second

// summarizeSystemPrompt 摘要系统提示（要求跟随文件语言、300 字内）。
const summarizeSystemPrompt = "用与文件相同的语言总结以下文件内容，300 字内"

// Client 是 OpenAI 兼容摘要客户端。
type Client struct {
	enabled bool
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// New 构造客户端（enabled=false 时 Summarize 恒 ErrDisabled）。
// baseURL 形如 https://api.openai.com/v1（不含尾斜杠）；model 缺省
// gpt-4o-mini（config 层已保证）。
func New(enabled bool, baseURL, apiKey, model string) *Client {
	return newClient(enabled, baseURL, apiKey, model, DefaultTimeout)
}

func newClient(enabled bool, baseURL, apiKey, model string, timeout time.Duration) *Client {
	if model == "" {
		model = "gpt-4o-mini"
	}
	return &Client{
		enabled: enabled, baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey: apiKey, model: model,
		client: &http.Client{Timeout: timeout},
	}
}

// Enabled 返回是否启用（HTTP 层据此返回 503 AI_DISABLED）。
func (c *Client) Enabled() bool { return c.enabled }

// chatMessage 为 /chat/completions 的消息条目。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Summarize 生成 text（文件内容）的摘要；filename 提供给模型作上下文。
// 错误分类：ErrDisabled / ErrTooLarge（>100KB）/ ErrUpstream。
func (c *Client) Summarize(ctx context.Context, text, filename string) (string, error) {
	if !c.enabled {
		return "", ErrDisabled
	}
	if len(text) > MaxInputBytes {
		return "", ErrTooLarge
	}
	userContent := "文件名：" + filename + "\n\n" + text
	payload, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: summarizeSystemPrompt},
			{Role: "user", Content: userContent},
		},
		MaxTokens: 500,
	})
	if err != nil {
		return "", fmt.Errorf("%w: marshal request: %v", ErrUpstream, err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: read response: %v", ErrUpstream, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, truncate(body, 200))
	}
	var out chatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("%w: decode response: %v", ErrUpstream, err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("%w: %s", ErrUpstream, out.Error.Message)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("%w: empty completion", ErrUpstream)
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
