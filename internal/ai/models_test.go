// Package ai —— 多模型 / 场景默认 / 校验兼容单测（settings 模型解析、
// resolveChatTarget 场景回落与越权拒绝、ValidateAI 场景默认校验、
// ResolveEmbeddingTarget 旧值兼容）。
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/docflow/docflow/internal/settings"
)

// TestEffectiveModelsLegacyCompat 旧 settings JSON（providers 无 models
// 数组）解析后按 base_url+model 合成单模型列表（chat 能力）。
func TestEffectiveModelsLegacyCompat(t *testing.T) {
	raw := `[{"id":"legacy","name":"Legacy","kind":"openai_compatible","base_url":"https://api.openai.com/v1","api_key":"sk-x","model":"gpt-4o-mini","enabled":true}]`
	var providers []settings.AIProvider
	if err := json.Unmarshal([]byte(raw), &providers); err != nil {
		t.Fatalf("unmarshal legacy providers: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers = %d", len(providers))
	}
	models := providers[0].EffectiveModels()
	if len(models) != 1 || models[0].ID != "gpt-4o-mini" || !models[0].Capabilities.IsChat() {
		t.Fatalf("legacy EffectiveModels = %+v", models)
	}
	if models[0].Capabilities.Kind != settings.AIModelKindChat || models[0].Capabilities.Vision {
		t.Fatalf("legacy 合成模型不应带其他能力: %+v", models[0].Capabilities)
	}
	if providers[0].PrimaryModel() != "gpt-4o-mini" {
		t.Fatalf("PrimaryModel = %q", providers[0].PrimaryModel())
	}
	// models 非空时原样返回、PrimaryModel 取首个 chat 能力模型。
	multi := settings.AIProvider{Model: "legacy-primary", Models: []settings.AIModel{
		{ID: "emb-only", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}},
		{ID: "chat-a", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
	}}
	if got := multi.PrimaryModel(); got != "chat-a" {
		t.Fatalf("multi PrimaryModel = %q, want chat-a", got)
	}
	if len(multi.EffectiveModels()) != 2 {
		t.Fatalf("multi EffectiveModels = %+v", multi.EffectiveModels())
	}
}

// TestResolveChatTargetScenarioDefaults 场景默认解析：summary 命中专属
// 模型；无效场景（edit 未配置）回落 chat；均未配置回落默认 Provider 主模型。
func TestResolveChatTargetScenarioDefaults(t *testing.T) {
	cfg := settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "m1", Name: "Mock", Kind: settings.AIKindMock, Enabled: true,
			Models: []settings.AIModel{
				{ID: "chat-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
				{ID: "sum-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
			},
		}},
		DefaultProvider: "m1",
		DefaultModels: map[string]settings.AIModelRef{
			"chat":    {ProviderID: "m1", ModelID: "chat-m"},
			"summary": {ProviderID: "m1", ModelID: "sum-m"},
		},
	}
	svc := NewService(func() (settings.AIConfig, error) { return cfg, nil })
	p, m, _, err := svc.ResolveChatTarget("", "", settings.AIScenarioSummary)
	if err != nil || p.ID != "m1" || m != "sum-m" {
		t.Fatalf("summary: %v %s %s", err, p.ID, m)
	}
	_, m, _, err = svc.ResolveChatTarget("", "", settings.AIScenarioEdit)
	if err != nil || m != "chat-m" {
		t.Fatalf("edit 应回落 chat: %v %s", err, m)
	}
	// 显式 provider 无 model：场景默认命中同 Provider 时优先。
	_, m, _, err = svc.ResolveChatTarget("m1", "", settings.AIScenarioSummary)
	if err != nil || m != "sum-m" {
		t.Fatalf("explicit provider summary: %v %s", err, m)
	}
	// default_models 清空后回落默认 Provider 主模型（首个 chat 能力模型）。
	cfg2 := cfg
	cfg2.DefaultModels = nil
	svc2 := NewService(func() (settings.AIConfig, error) { return cfg2, nil })
	_, m, _, err = svc2.ResolveChatTarget("", "", "")
	if err != nil || m != "chat-m" {
		t.Fatalf("fallback primary: %v %s", err, m)
	}
}

// TestResolveChatTargetModelNotAllowed 显式 model 越权拒绝：不属于任何
// 启用 Provider / 无 chat 能力 → ErrModelNotAllowed。
func TestResolveChatTargetModelNotAllowed(t *testing.T) {
	cfg := settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "m1", Name: "Mock", Kind: settings.AIKindMock, Enabled: true,
			Models: []settings.AIModel{
				{ID: "chat-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
				{ID: "emb-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}},
			},
		}},
		DefaultProvider: "m1",
	}
	svc := NewService(func() (settings.AIConfig, error) { return cfg, nil })
	if _, _, _, err := svc.ResolveChatTarget("", "no-such", ""); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("unknown model: %v", err)
	}
	if _, _, _, err := svc.ResolveChatTarget("m1", "emb-m", ""); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("embedding-only model: %v", err)
	}
	// 未指定 Provider 时按模型归属反查：合法模型命中。
	p, m, _, err := svc.ResolveChatTarget("", "chat-m", "")
	if err != nil || p.ID != "m1" || m != "chat-m" {
		t.Fatalf("model reverse lookup: %v %s %s", err, p.ID, m)
	}
}

// TestChatScenarioDefaultUsedBySummarize SummarizeFile 走 summary 场景
// 默认模型（mock 全链路）。
func TestChatScenarioDefaultUsedBySummarize(t *testing.T) {
	cfg := settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "m1", Name: "Mock", Kind: settings.AIKindMock, Enabled: true,
			Models: []settings.AIModel{
				{ID: "chat-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
				{ID: "sum-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
			},
		}},
		DefaultProvider: "m1",
		DefaultModels:   map[string]settings.AIModelRef{"summary": {ProviderID: "m1", ModelID: "sum-m"}},
	}
	svc := NewService(func() (settings.AIConfig, error) { return cfg, nil })
	res, err := svc.Chat(context.Background(), ChatRequest{Scenario: settings.AIScenarioSummary, Messages: []Message{{Role: "user", Content: "文件名：a.md\n\n内容"}}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Model != "sum-m" {
		t.Fatalf("summary 场景模型 = %q, want sum-m", res.Model)
	}
}

// TestValidateAIDefaultModels 场景默认校验：未知场景键 / Provider 不存在 /
// 模型不存在 / 能力不匹配 → ErrInvalidValue 系错误；合法配置通过。
func TestValidateAIDefaultModels(t *testing.T) {
	base := settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "p1", Name: "P1", Kind: settings.AIKindOpenAICompatible, BaseURL: "https://api.example.com/v1", Enabled: true,
			Models: []settings.AIModel{
				{ID: "chat-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
				{ID: "emb-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}},
			},
		}},
		Temperature: 0.3, MaxTokens: 1024, PerUserPerMin: 20,
	}
	good := base
	good.DefaultModels = map[string]settings.AIModelRef{
		"chat":      {ProviderID: "p1", ModelID: "chat-m"},
		"summary":   {ProviderID: "p1", ModelID: "chat-m"},
		"edit":      {ProviderID: "p1", ModelID: "chat-m"},
		"embedding": {ProviderID: "p1", ModelID: "emb-m"},
	}
	if err := settings.ValidateAI(good); err != nil {
		t.Fatalf("good default_models: %v", err)
	}
	badScene := good
	badScene.DefaultModels = map[string]settings.AIModelRef{"unknown": {ProviderID: "p1", ModelID: "chat-m"}}
	if err := settings.ValidateAI(badScene); err == nil {
		t.Fatal("未知场景键应报错")
	}
	badProvider := good
	badProvider.DefaultModels = map[string]settings.AIModelRef{"chat": {ProviderID: "missing", ModelID: "chat-m"}}
	if err := settings.ValidateAI(badProvider); err == nil {
		t.Fatal("Provider 不存在应报错")
	}
	badModel := good
	badModel.DefaultModels = map[string]settings.AIModelRef{"chat": {ProviderID: "p1", ModelID: "nope"}}
	if err := settings.ValidateAI(badModel); err == nil {
		t.Fatal("模型不存在应报错")
	}
	badCap := good
	badCap.DefaultModels = map[string]settings.AIModelRef{"chat": {ProviderID: "p1", ModelID: "emb-m"}}
	if err := settings.ValidateAI(badCap); err == nil {
		t.Fatal("chat 场景指向 embedding 模型应报错")
	}
	badEmb := good
	badEmb.DefaultModels = map[string]settings.AIModelRef{"embedding": {ProviderID: "p1", ModelID: "chat-m"}}
	if err := settings.ValidateAI(badEmb); err == nil {
		t.Fatal("embedding 场景指向无 embedding 能力模型应报错")
	}
}

// TestValidateAIRAGEmbeddingFromProviders RAG embedding/rerank 校验：须为
// 已配置 Provider 的对应能力模型（mock 与旧值 openai_compatible 兼容）。
func TestValidateAIRAGEmbeddingFromProviders(t *testing.T) {
	base := settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "p1", Name: "P1", Kind: settings.AIKindOpenAICompatible, BaseURL: "https://api.example.com/v1", Enabled: true,
			Models: []settings.AIModel{
				{ID: "chat-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
				{ID: "emb-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}},
				{ID: "rr-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindRerank}},
			},
		}},
		Temperature: 0.3, MaxTokens: 1024, PerUserPerMin: 20,
		RAG: settings.AIRAGConfig{
			Mode: "hybrid", VectorEnabled: true, QdrantURL: "http://qdrant:6333", CollectionPrefix: "docflow_",
			TopK: 8, ChunkSize: 1000, ChunkOverlap: 100,
		},
	}
	good := base
	good.RAG.EmbeddingProvider = "p1"
	good.RAG.EmbeddingModel = "emb-m"
	good.RAG.RerankProvider = "p1"
	good.RAG.RerankModel = "rr-m"
	if err := settings.ValidateAI(good); err != nil {
		t.Fatalf("good rag: %v", err)
	}
	badEmbCap := base
	badEmbCap.RAG.EmbeddingProvider = "p1"
	badEmbCap.RAG.EmbeddingModel = "chat-m" // 无 embedding 能力
	if err := settings.ValidateAI(badEmbCap); err == nil {
		t.Fatal("embedding 指向无能力模型应报错")
	}
	badRerank := base
	badRerank.RAG.EmbeddingProvider = "mock"
	badRerank.RAG.RerankProvider = "p1"
	badRerank.RAG.RerankModel = "emb-m" // 无 rerank 能力
	if err := settings.ValidateAI(badRerank); err == nil {
		t.Fatal("rerank 指向无能力模型应报错")
	}
	legacy := base
	legacy.RAG.EmbeddingProvider = settings.AIKindOpenAICompatible
	legacy.RAG.EmbeddingModel = "text-embedding-3-small"
	if err := settings.ValidateAI(legacy); err != nil {
		t.Fatalf("旧值 openai_compatible 应兼容: %v", err)
	}
}

// TestResolveEmbeddingTargetLegacyFallback embedding 解析：Provider ID +
// embedding 能力模型命中；旧值 openai_compatible 回落默认 Provider。
func TestResolveEmbeddingTargetLegacyFallback(t *testing.T) {
	cfg := settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "p1", Name: "P1", Kind: settings.AIKindOpenAICompatible, BaseURL: "https://api.example.com/v1", Enabled: true,
			Models: []settings.AIModel{
				{ID: "chat-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
				{ID: "emb-m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}},
			},
		}},
		DefaultProvider: "p1",
	}
	cfg.RAG.EmbeddingProvider = "p1"
	cfg.RAG.EmbeddingModel = "emb-m"
	if p, m, ok := cfg.ResolveEmbeddingTarget(); !ok || p.ID != "p1" || m != "emb-m" {
		t.Fatalf("providerId 命中: %v %s %s", ok, p.ID, m)
	}
	// 能力不匹配 → 未命中。
	cfg.RAG.EmbeddingModel = "chat-m"
	if _, _, ok := cfg.ResolveEmbeddingTarget(); ok {
		t.Fatal("无 embedding 能力不应命中")
	}
	// 旧值 openai_compatible → 回落默认 Provider（模型名不校验能力）。
	cfg.RAG.EmbeddingProvider = settings.AIKindOpenAICompatible
	cfg.RAG.EmbeddingModel = "text-embedding-3-small"
	if p, m, ok := cfg.ResolveEmbeddingTarget(); !ok || p.ID != "p1" || m != "text-embedding-3-small" {
		t.Fatalf("旧值回落: %v %s %s", ok, p.ID, m)
	}
}

// TestValidateAIProviderModelsAndLimits Provider 级校验：模型 ID 必填且
// 不重复；requests_per_min/daily_quota 范围。
func TestValidateAIProviderModelsAndLimits(t *testing.T) {
	if err := settings.ValidateAIProvider(settings.AIProvider{
		ID: "p", Name: "P", Kind: settings.AIKindOpenAICompatible, BaseURL: "https://api.example.com/v1",
		Models: []settings.AIModel{{ID: " "}},
	}); err == nil {
		t.Fatal("空模型 ID 应报错")
	}
	if err := settings.ValidateAIProvider(settings.AIProvider{
		ID: "p", Name: "P", Kind: settings.AIKindOpenAICompatible, BaseURL: "https://api.example.com/v1",
		Models: []settings.AIModel{{ID: "a"}, {ID: "a"}},
	}); err == nil {
		t.Fatal("重复模型 ID 应报错")
	}
	if err := settings.ValidateAIProvider(settings.AIProvider{
		ID: "p", Name: "P", Kind: settings.AIKindMock, RequestsPerMin: 99999,
	}); err == nil {
		t.Fatal("requests_per_min 超范围应报错")
	}
	if err := settings.ValidateAIProvider(settings.AIProvider{
		ID: "p", Name: "P", Kind: settings.AIKindMock, DailyQuota: -1,
	}); err == nil {
		t.Fatal("daily_quota 为负应报错")
	}
	// models 非空时旧 model 字段可缺省。
	if err := settings.ValidateAIProvider(settings.AIProvider{
		ID: "p", Name: "P", Kind: settings.AIKindOpenAICompatible, BaseURL: "https://api.example.com/v1",
		Models: []settings.AIModel{{ID: "a", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}}},
	}); err != nil {
		t.Fatalf("models 非空时 model 可缺省: %v", err)
	}
}
