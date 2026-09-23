// Package ai —— ocr.go：图片 OCR（索引管道专用）。
//
// ExtractImageText 把存储中的一张图片交给配置的视觉模型提取文字：
//   - 配置来自 ai.ocr（settings 整体 JSON 块，每次调用热读取）：disabled
//     时安静返回空（调用方按「非文本」处理，索引仅名称）；
//   - Provider/Model 指向启用中 Provider 的「视觉图片」模型：openai_
//     compatible 走 chat/completions 多模态 content（image_url dataURL），
//     anthropic 走 /v1/messages 的 base64 source 块；均非流式、max_tokens
//     固定 2000；
//   - 失败降级语义：错误原样上抛（文本为空串），由调用方（search.Indexer）
//     记日志并回退仅名称索引——本方法不重试不打日志，保持纯服务；
//   - 图片经注入的 StorageReader 限流读取（max_image_bytes 上限，读超即
//     截断报错），base64 后内联进请求体（无外部 URL 回源，兼容私有部署）。
package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/settings"
)

// ocrPrompt OCR 提示词：只输出识别文字、保持原有顺序与换行；无文字
// 图片固定回「无文字」（响应侧将其归一为空串，见 ocrNoTextMarker）。
const ocrPrompt = "请提取图片中的全部文字内容（OCR）。只输出识别出的文字，保持原有顺序与换行结构；若图片中没有文字，只输出：无文字。"

// ocrNoTextMarker 模型对无文字图片的约定回复，视为「无内容」返回空串。
const ocrNoTextMarker = "无文字"

// ocrRequestTimeout 单次 OCR 请求上限（60s 非流式一次往返；上层索引管道
// 另有 90s 外层超时，这里先一步失败便于日志归因）。
const ocrRequestTimeout = 60 * time.Second

// ocrMaxTokens OCR 响应 token 上限：图片文字量有限，2000 足够且防跑飞。
const ocrMaxTokens = 2000

// StorageReader 抽象对象存储读取（upload.Storage 已满足）：OCR 需要读
// 图片 bytes 内联进视觉模型请求体，接口最小化避免 ai 直接依赖 upload。
type StorageReader interface {
	Read(storageKey string) (io.ReadCloser, error)
}

// SetStorageReader 注入存储读取器（main 装配时传 upload.Storage）；nil
// 忽略保持未配置（ExtractImageText 对未注入报错，调用方降级仅名称）。
func (s *Service) SetStorageReader(r StorageReader) {
	if r != nil {
		s.storage = r
	}
}

// ExtractImageText 读取 storageKey 处的图片并调视觉模型 OCR 提取文字
// （name 仅作接口透传/日志语义，不参与提示词）。行为契约：
//   - OCR 关闭（ai.ocr.enabled=false）→ ("", nil) 安静跳过；
//   - Provider 不存在/未启用、mock（无真实视觉能力）、存储读取器未注入、
//     size<=0 或超 max_image_bytes、限流读取发现实际超限 → 返回错误
//     （调用方记日志，索引回退仅名称）；
//   - 成功返回提取文本（trim 空白；模型回「无文字」归一为空串）。
//
// 热配置：每次调用读当前生效配置，管理端改 ai.ocr 即时生效无需重启。
func (s *Service) ExtractImageText(ctx context.Context, storageKey, mimeType, name string, size int64) (string, error) {
	cfg, err := s.Config()
	if err != nil {
		return "", err
	}
	if !cfg.OCR.Enabled {
		return "", nil
	}
	provider, ok := cfg.ProviderByID(cfg.OCR.ProviderID)
	if !ok || !provider.Enabled {
		return "", errors.New("ocr provider not available")
	}
	if provider.Kind == settings.AIKindMock {
		return "", errors.New("mock provider does not support vision ocr")
	}
	if s.storage == nil {
		return "", errors.New("ocr storage reader not configured")
	}
	maxBytes := cfg.OCR.MaxImageBytes
	if maxBytes <= 0 {
		maxBytes = settings.AIOCRMaxBytesDefault // 防御：直构配置（env 基线）未经钳制
	}
	if size <= 0 || size > maxBytes {
		return "", errors.New("image too large for ocr")
	}
	r, err := s.storage.Read(storageKey)
	if err != nil {
		return "", err
	}
	defer r.Close()
	// 限流读取 max+1 字节：多读 1 字节即可判定「声明大小与实际不符」的
	// 截断场景（存储层大小漂移时不把超大图塞进请求体）。
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxBytes {
		return "", errors.New("image too large for ocr")
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	// 60s 上限包一层（defer cancel，不覆盖调用方 ctx 的 cancel 语义）。
	reqCtx, cancel := context.WithTimeout(ctx, ocrRequestTimeout)
	defer cancel()
	start := s.now()
	var text string
	var promptTokens, completionTokens int
	switch provider.Kind {
	case settings.AIKindOpenAICompatible:
		text, promptTokens, completionTokens, err = s.ocrOpenAI(reqCtx, provider, cfg.OCR.ModelID, b64, mimeType)
	case settings.AIKindAnthropic:
		text, promptTokens, completionTokens, err = s.ocrAnthropic(reqCtx, provider, cfg.OCR.ModelID, b64, mimeType)
	default:
		return "", fmt.Errorf("ocr: unsupported provider kind %q", provider.Kind)
	}
	if err != nil {
		return "", err
	}
	// 用量记账复用对话链路的 recordUsage（按 provider+model 维度；无 usage
	// 回包时按字符近似折算），失败 best-effort 不影响结果。
	s.recordUsage(provider, cfg.OCR.ModelID, ChatRequest{Messages: []Message{{Role: "user", Content: ocrPrompt}}}, ChatResult{
		Content: text, PromptTokens: promptTokens, CompletionTokens: completionTokens, DurationMS: time.Since(start).Milliseconds(),
	})
	text = strings.TrimSpace(text)
	if text == "" || text == ocrNoTextMarker {
		return "", nil
	}
	return text, nil
}

// ---------- 请求体（本文件局部定义；响应复用 chat.go 的 openAIResponse
// / anthropicResponse 形状） ----------

// ocrImageURL openai 兼容 image_url 块（dataURL 内联图片）。
type ocrImageURL struct {
	URL string `json:"url"`
}

// ocrVisionPart openai 兼容多模态 content 元素（text 或 image_url）。
type ocrVisionPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *ocrImageURL `json:"image_url,omitempty"`
}

type ocrOpenAIMessage struct {
	Role    string          `json:"role"`
	Content []ocrVisionPart `json:"content"`
}

type ocrOpenAIRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []ocrOpenAIMessage `json:"messages"`
}

// ocrBase64Source anthropic 图片 source 块（base64 内联）。
type ocrBase64Source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ocrAnthropicPart anthropic 多模态 content 元素（image 或 text）。
type ocrAnthropicPart struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *ocrBase64Source `json:"source,omitempty"`
}

type ocrAnthropicMessage struct {
	Role    string             `json:"role"`
	Content []ocrAnthropicPart `json:"content"`
}

type ocrAnthropicRequest struct {
	Model     string                `json:"model"`
	MaxTokens int                   `json:"max_tokens"`
	Messages  []ocrAnthropicMessage `json:"messages"`
}

// ocrOpenAI 调 {base}/chat/completions（非流式）：messages[0].content 为
// [提示词, image_url dataURL] 多模态数组；返回文本与 usage token 数。
func (s *Service) ocrOpenAI(ctx context.Context, p settings.AIProvider, model, b64, mimeType string) (string, int, int, error) {
	body := ocrOpenAIRequest{
		Model:     model,
		MaxTokens: ocrMaxTokens,
		Messages: []ocrOpenAIMessage{{
			Role: "user",
			Content: []ocrVisionPart{
				{Type: "text", Text: ocrPrompt},
				{Type: "image_url", ImageURL: &ocrImageURL{URL: "data:" + mimeType + ";base64," + b64}},
			},
		}},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: marshal request: %v", ErrUpstreamChat, err)
	}
	url := strings.TrimSuffix(p.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: build request: %v", ErrUpstreamChat, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: %v", ErrUpstreamChat, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, 0, fmt.Errorf("%w: status %d: %s", ErrUpstreamChat, resp.StatusCode, truncateBytes(raw, 200))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: read response: %v", ErrUpstreamChat, err)
	}
	var out openAIResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, 0, fmt.Errorf("%w: decode response: %v", ErrUpstreamChat, err)
	}
	if out.Error != nil {
		return "", 0, 0, fmt.Errorf("%w: %s", ErrUpstreamChat, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", 0, 0, fmt.Errorf("%w: empty completion", ErrUpstreamChat)
	}
	var pt, ct int
	if out.Usage != nil {
		pt, ct = out.Usage.PromptTokens, out.Usage.CompletionTokens
	}
	return out.Choices[0].Message.Content, pt, ct, nil
}

// ocrAnthropic 调 {base}/v1/messages（非流式，anthropic-version 头与
// chat.go 一致）：content 为 [image(base64 source), text] 数组（anthropic
// 要求图片块在前）；返回拼接的 text 块与 usage token 数。
func (s *Service) ocrAnthropic(ctx context.Context, p settings.AIProvider, model, b64, mimeType string) (string, int, int, error) {
	body := ocrAnthropicRequest{
		Model:     model,
		MaxTokens: ocrMaxTokens,
		Messages: []ocrAnthropicMessage{{
			Role: "user",
			Content: []ocrAnthropicPart{
				{Type: "image", Source: &ocrBase64Source{Type: "base64", MediaType: mimeType, Data: b64}},
				{Type: "text", Text: ocrPrompt},
			},
		}},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: marshal request: %v", ErrUpstreamChat, err)
	}
	url := strings.TrimSuffix(p.BaseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: build request: %v", ErrUpstreamChat, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("x-api-key", p.APIKey)
	}
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: %v", ErrUpstreamChat, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, 0, fmt.Errorf("%w: status %d: %s", ErrUpstreamChat, resp.StatusCode, truncateBytes(raw, 200))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: read response: %v", ErrUpstreamChat, err)
	}
	var out anthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, 0, fmt.Errorf("%w: decode response: %v", ErrUpstreamChat, err)
	}
	if out.Error != nil {
		return "", 0, 0, fmt.Errorf("%w: %s", ErrUpstreamChat, out.Error.Message)
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), out.Usage.InputTokens, out.Usage.OutputTokens, nil
}
