// Package settings 提供运行时可调的系统设置（system_settings 表）：
// 内置键定义（类型/默认值/范围/描述）、类型校验、热读取接口与审计写入。
// 非密钥原则：密钥类配置（JWT/S3 凭据等）一律走环境变量，不入库、不暴露
// 于管理 API。例外：SMTP 投递参数（含密码，只写不读）允许经管理端入库
// 覆盖 env——邮件通道是管理员日常运维项，且 DB 值经专用端点读写、密码
// 永不回显（见 SMTPSettings 与 /admin/settings/smtp）。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// value_type 取值，与 system_settings.value_type CHECK 约束一致。
const (
	TypeBool   = "bool"
	TypeInt    = "int"
	TypeString = "string"
)

// effect 取值：设置变更的生效方式元数据（管理端展示用）。
const (
	// EffectImmediate 变更即时生效（消费方每次请求热读取 system_settings）。
	EffectImmediate = "immediate"
	// EffectNewSession 变更对新会话/新登录生效。
	EffectNewSession = "new_session"
	// EffectRestart 变更须重启服务进程生效（启动时装配读取的配置）。
	EffectRestart = "restart"
)

var (
	// ErrUnknownKey 表示键不在内置定义中（不可设置/读取）。
	ErrUnknownKey = errors.New("unknown settings key")
	// ErrInvalidType 表示值类型与键定义不符（如 bool 键传入字符串）。
	ErrInvalidType = errors.New("invalid settings value type")
	// ErrInvalidValue 表示值类型正确但超出允许范围（如 int 下限/上限）。
	ErrInvalidValue = errors.New("invalid settings value")
	// ErrNotSet 表示键在表中没有行（读取时回退默认值）。
	ErrNotSet = errors.New("settings key not set")
)

// 内置键（唯一可设置的键集合；新增键须在此登记定义）。
// site.name 已移除：无任何消费方（前端标题等为静态配置），保留即死设置。
const (
	KeyUploadMaxVersionsPerFile  = "upload.max_versions_per_file"
	KeyUploadMaxFileSize         = "upload.max_file_size"
	KeyUploadDefaultQuota        = "upload.default_quota"
	KeyShareDefaultExpiryHours   = "share.default_expiry_hours"
	KeyShareDefaultWatermark     = "share.default_watermark"
	KeyShareWatermarkText        = "share.watermark_text"
	KeyRetentionTrashDays        = "retention.trash_days"
	KeyRetentionAccessEventsDays = "retention.access_events_days"
	KeyRateLimitPerMinute        = "security.rate_limit_per_minute"
	KeyLoginMaxRetries           = "security.login_max_retries"
	KeyLoginLockMinutes          = "security.login_lock_minutes"
	KeyMaxConcurrentUploads      = "upload.max_concurrent_uploads_per_user"
	KeyBatchMaxItems             = "batch.max_items"
	KeyFolderMaxDepth            = "folder.max_depth"
	KeySharePublicEnabled        = "share.public_enabled"
	KeyScanQuarantinePolicy      = "security.scan_quarantine_policy"
	KeyBackupEnabled             = "backup.enabled"
	KeyBackupRetentionDays       = "backup.retention_days"
	KeyBackupEncryptionRequired  = "backup.encryption_required"
	KeyBackupLastVerify          = "backup.last_verify"
	KeyAuditRetentionDays        = "audit.retention_days"
	// KeyUploadVersionRetentionDays 版本保留时间窗（天）：与
	// upload.max_versions_per_file 组合——时间窗内的版本不因数量裁剪删除；
	// 0 = 不启用时间窗（仅按数量裁剪，默认，避免行为突变）。
	KeyUploadVersionRetentionDays = "upload.version_retention_days"
	// KeyUploadBlockedExtensions 上传扩展名黑名单（逗号分隔，如
	// "exe,bat,sh"）；默认空 = 不拦截。建会话与 Complete 双侧校验。
	KeyUploadBlockedExtensions = "upload.blocked_extensions"
	// 空间模型（migration 040，统一空间）运行时配置：新空间默认配额 /
	// 空间配额上限 / 每用户空间数上限（admin 可改，热读取）。
	KeySpaceDefaultQuota = "space.default_quota"
	KeySpaceMaxQuota     = "space.max_quota"
	KeySpaceMaxPerUser   = "space.max_per_user"
	KeyWebDAVEnabled     = "webdav.enabled"
	// KeyCollabEnabled 富文本实时协作（ProseMirror 协作房间）总开关：
	// 关闭时 /api/v1/collab/:fileId/ws 返回 404 且不创建房间。
	KeyCollabEnabled       = "collab.enabled"
	KeyAgentEnabled        = "agent.enabled"
	KeyAgentRuntime        = "agent.runtime"
	KeyAgentAllowedImages  = "agent.allowed_images"
	KeyAgentMaxConcurrent  = "agent.max_concurrent"
	KeyAgentDefaultTimeout = "agent.default_timeout_seconds"
	KeyAgentMaxCPU         = "agent.max_cpu"
	KeyAgentMaxMemory      = "agent.max_memory_bytes"
	KeyAgentNetworkMode    = "agent.network_mode"
	KeyAgentCallbackURL    = "agent.mcp_callback_base_url"
	// Agent 容器经 IPC socket 调用平台 AI（agentsock）与产物同步模式。
	KeyAgentAllowAI    = "agent.allow_ai"
	KeyAgentAIMaxCalls = "agent.ai_max_calls"
	KeyAgentSyncMode   = "agent.sync_mode"
	// KeyAgentHarness 为 Agent 执行引擎选择（键名冻结）：auto 按平台默认
	// 模型协议自动路由，claude-code/pi 为显式指定，builtin 为内置轻量
	// runner；终值经 RuntimeRequest 注入容器 env DOCFLOW_HARNESS。
	KeyAgentHarness = "agent.harness"
)

// SMTP 运行时配置键（system_settings 存储，邮件发送处优先读库回退 env）。
// 刻意不进 Definitions：通用设置列表（GET /admin/settings）不展示、通用
// 写入口（PUT /admin/settings/:key）不可写，统一走专用 /admin/settings/smtp
// 端点（密码只写不读、审计打码）。
const (
	KeySMTPEnabled = "smtp.enabled"
	KeySMTPHost    = "smtp.host"
	KeySMTPPort    = "smtp.port"
	KeySMTPUser    = "smtp.user"
	KeySMTPPass    = "smtp.pass"
	KeySMTPFrom    = "smtp.from"
	KeySMTPTLSMode = "smtp.tls_mode"
)

// SMTP TLS 模式（KeySMTPTLSMode 取值）。
const (
	SMTPTLSModeAuto = "auto" // STARTTLS（服务器通告时自动协商，默认）
	SMTPTLSModeSSL  = "ssl"  // 隐式 TLS（SMTPS，465 端口常见）
	SMTPTLSModeNone = "none" // 不协商 TLS（仅内网中继场景）
)

// AI 运行时配置键（system_settings 存储，AI 对话/摘要处每次热读取）。
// 与 SMTP 同模式：不进 Definitions（通用设置列表不展示、通用写入口不可写），
// 统一走专用 /admin/settings/ai 端点；Provider 的 api_key 同 SMTP 密码——
// 只写不读（留空保持现值），任何读路径均以掩码回显。
const (
	KeyAIEnabled              = "ai.enabled"
	KeyAIProviders            = "ai.providers"
	KeyAIDefaultProvider      = "ai.default_provider"
	KeyAIDefaultModels        = "ai.default_models"
	KeyAITemperature          = "ai.temperature"
	KeyAIMaxTokens            = "ai.max_tokens"
	KeyAIPerUserPerMin        = "ai.per_user_per_min"
	KeyAIRAGMode              = "ai.rag.mode"
	KeyAIRAGVectorEnabled     = "ai.rag.vector_enabled"
	KeyAIRAGQdrantURL         = "ai.rag.qdrant_url"
	KeyAIRAGCollectionPrefix  = "ai.rag.collection_prefix"
	KeyAIRAGEmbeddingProvider = "ai.rag.embedding_provider"
	KeyAIRAGEmbeddingModel    = "ai.rag.embedding_model"
	KeyAIRAGRerankProvider    = "ai.rag.rerank_provider"
	KeyAIRAGRerankModel       = "ai.rag.rerank_model"
	KeyAIRAGTopK              = "ai.rag.top_k"
	KeyAIRAGChunkSize         = "ai.rag.chunk_size"
	KeyAIRAGChunkOverlap      = "ai.rag.chunk_overlap"
	// AI 联网搜索（chat 的 web_search 增强）：provider 为 "" = 禁用。
	// tavily_api_key 为密钥（同 provider api_key：只写不读、掩码回显），
	// 刻意不进 Definitions（通用设置列表不展示、通用写入口不可写）。
	KeyAISearchProvider   = "ai.search.provider"
	KeyAISearchSearxngURL = "ai.search.searxng_url"
	KeyAISearchTavilyKey  = "ai.search.tavily_api_key"
	KeyAISearchMaxResults = "ai.search.max_results"
	// KeyAIOCR 为图片 OCR 配置（整体 JSON 块：enabled/provider_id/model_id/
	// max_image_bytes）：开启后索引管道对图片文件调具备「视觉图片」能力的
	// 模型提取文字入索引。同 ai.personas 模式——不进 Definitions（通用设置
	// 列表不展示、通用写入口不可写），统一走 /admin/settings/ai 整块读写。
	KeyAIOCR = "ai.ocr"
	// 平台人设/技能提示词模板（ai.personas / ai.skills，JSON 数组整体
	// 读替）：管理员维护（专用 /admin/settings/ai/personas|skills 端点）、
	// 登录用户经 GET /ai/personas|/ai/skills 读取；EffectImmediate——前端
	// 下拉/弹层每次拉取热读取。同 SMTP/AI 专用键：不进 Definitions
	//（通用设置列表不展示、通用写入口不可写）。
	KeyAIPersonas = "ai.personas"
	KeyAISkills   = "ai.skills"
	// KeyAIMCPServices 为外部 MCP 服务器配置（ai.mcp，JSON 数组整体读替）：
	// 管理员维护（专用 /admin/settings/ai/mcp 端点）、对话入口 use_mcp 开启
	// 时热读取消费；auth_header 同 api_key——只写不读（留空 = 按 ID 继承
	// 现值，读路径永不回显）。同 ai.personas 模式：不进 Definitions
	//（通用设置列表不展示、通用写入口不可写）。EffectImmediate。
	KeyAIMCPServices = "ai.mcp"
)

// 平台人设/技能校验边界（settings 键 ai.personas / ai.skills）。
const (
	// AIPlatformMaxEntries 人设/技能条数上限。
	AIPlatformMaxEntries = 50
	// AIPlatformMaxIDRunes 条目 id 长度上限（rune 计）。
	AIPlatformMaxIDRunes = 64
	// AIPlatformMaxNameRunes 条目 name 长度上限（人设/技能一致，≤64）。
	AIPlatformMaxNameRunes = 64
	// AIPlatformMaxSkillDescRunes 技能 description 长度上限。
	AIPlatformMaxSkillDescRunes = 200
	// AIPlatformMaxPromptRunes 提示词长度上限（人设 system_prompt 与技能 prompt 一致）。
	AIPlatformMaxPromptRunes = 4000
)

// AIPersonaDef 为平台人设条目（ai.personas JSON 数组元素）：管理员维护的
// 通用 system 提示模板，对话入口按 id 选用后作为 system 语义注入。
type AIPersonaDef struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SystemPrompt string `json:"system_prompt"`
}

// AISkillDef 为平台技能/快捷指令模板条目（ai.skills JSON 数组元素）：
// prompt 支持占位符 {selection}（编辑器选区）/ {file}（当前文件名），由
// 前端在填入输入框时替换。
type AISkillDef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Prompt      string `json:"prompt"`
	// Enabled 控制技能对登录用户是否可见（false = 停用：不出现在技能
	// 模板按钮；管理端始终可见可编辑）。缺省视为启用，兼容旧数据。
	Enabled *bool `json:"enabled,omitempty"`
}

// skillEnabled 判定技能是否启用（nil 视为启用，兼容旧数据）。
func skillEnabled(s AISkillDef) bool {
	return s.Enabled == nil || *s.Enabled
}

// ValidateAIPersonas 校验平台人设列表：≤50 条、id 去空白后 1..64 且唯一、
// name 1..64、system_prompt ≤4000 字符。
func ValidateAIPersonas(list []AIPersonaDef) error {
	if len(list) > AIPlatformMaxEntries {
		return fmt.Errorf("%w: ai.personas 最多 %d 条", ErrInvalidValue, AIPlatformMaxEntries)
	}
	seen := make(map[string]bool, len(list))
	for i, p := range list {
		if id := strings.TrimSpace(p.ID); id == "" || len([]rune(id)) > AIPlatformMaxIDRunes {
			return fmt.Errorf("%w: personas[%d].id 须为 1..%d 个字符", ErrInvalidValue, i, AIPlatformMaxIDRunes)
		} else if seen[id] {
			return fmt.Errorf("%w: personas[%d].id %q 重复", ErrInvalidValue, i, id)
		} else {
			seen[id] = true
		}
		if name := strings.TrimSpace(p.Name); name == "" || len([]rune(name)) > AIPlatformMaxNameRunes {
			return fmt.Errorf("%w: personas[%d].name 须为 1..%d 个字符", ErrInvalidValue, i, AIPlatformMaxNameRunes)
		}
		if len([]rune(p.SystemPrompt)) > AIPlatformMaxPromptRunes {
			return fmt.Errorf("%w: personas[%d].system_prompt 过长（≤%d 字符）", ErrInvalidValue, i, AIPlatformMaxPromptRunes)
		}
	}
	return nil
}

// ValidateAISkills 校验平台技能列表：≤50 条、id 去空白后 1..64 且唯一、
// name 1..64、description ≤200、prompt ≤4000 字符。
func ValidateAISkills(list []AISkillDef) error {
	if len(list) > AIPlatformMaxEntries {
		return fmt.Errorf("%w: ai.skills 最多 %d 条", ErrInvalidValue, AIPlatformMaxEntries)
	}
	seen := make(map[string]bool, len(list))
	for i, s := range list {
		if id := strings.TrimSpace(s.ID); id == "" || len([]rune(id)) > AIPlatformMaxIDRunes {
			return fmt.Errorf("%w: skills[%d].id 须为 1..%d 个字符", ErrInvalidValue, i, AIPlatformMaxIDRunes)
		} else if seen[id] {
			return fmt.Errorf("%w: skills[%d].id %q 重复", ErrInvalidValue, i, id)
		} else {
			seen[id] = true
		}
		if name := strings.TrimSpace(s.Name); name == "" || len([]rune(name)) > AIPlatformMaxNameRunes {
			return fmt.Errorf("%w: skills[%d].name 须为 1..%d 个字符", ErrInvalidValue, i, AIPlatformMaxNameRunes)
		}
		if len([]rune(s.Description)) > AIPlatformMaxSkillDescRunes {
			return fmt.Errorf("%w: skills[%d].description 过长（≤%d 字符）", ErrInvalidValue, i, AIPlatformMaxSkillDescRunes)
		}
		if len([]rune(s.Prompt)) > AIPlatformMaxPromptRunes {
			return fmt.Errorf("%w: skills[%d].prompt 过长（≤%d 字符）", ErrInvalidValue, i, AIPlatformMaxPromptRunes)
		}
	}
	return nil
}

// AISearchProvider 取值（ai.search.provider）：空 = 禁用联网搜索。
const (
	AISearchProviderSearxng = "searxng"
	AISearchProviderTavily  = "tavily"
)

// 外部 MCP 服务器配置（settings 键 ai.mcp）校验边界。
const (
	// AIMCPMaxServices 服务条数上限（每轮对话逐服务拉取工具列表，过多
	// 会显著增加对话首包延迟）。
	AIMCPMaxServices = 8
	// AIMCPMaxURLRunes url 长度上限。
	AIMCPMaxURLRunes = 500
	// AIMCPMaxAuthHeaderRunes auth_header 长度上限（形如
	// "Authorization: Bearer <token>"）。
	AIMCPMaxAuthHeaderRunes = 1000
)

// AIMCPServiceDef 为一个外部 MCP 服务器条目（ai.mcp JSON 数组元素）：
// URL 为 Streamable HTTP 端点；AuthHeader 形如 "Authorization: Bearer x"
// （写入时留空 = 按 ID 继承现值，读路径永不回显）。对话入口 use_mcp 开启
// 时逐服务 initialize → tools/list 聚合工具供上游模型调用。
type AIMCPServiceDef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	AuthHeader string `json:"auth_header,omitempty"`
	Enabled    bool   `json:"enabled"`
}

// ValidateAIMCPServices 校验 MCP 服务列表：≤8 条、id 1..64 且唯一、
// name 1..64、url 为绝对 http(s) 地址且 ≤500、auth_header ≤1000。
func ValidateAIMCPServices(list []AIMCPServiceDef) error {
	if len(list) > AIMCPMaxServices {
		return fmt.Errorf("%w: ai.mcp 最多 %d 条", ErrInvalidValue, AIMCPMaxServices)
	}
	seen := make(map[string]bool, len(list))
	for i, s := range list {
		if id := strings.TrimSpace(s.ID); id == "" || len([]rune(id)) > AIPlatformMaxIDRunes {
			return fmt.Errorf("%w: mcp[%d].id 须为 1..%d 个字符", ErrInvalidValue, i, AIPlatformMaxIDRunes)
		} else if seen[id] {
			return fmt.Errorf("%w: mcp[%d].id %q 重复", ErrInvalidValue, i, id)
		} else {
			seen[id] = true
		}
		if name := strings.TrimSpace(s.Name); name == "" || len([]rune(name)) > AIPlatformMaxNameRunes {
			return fmt.Errorf("%w: mcp[%d].name 须为 1..%d 个字符", ErrInvalidValue, i, AIPlatformMaxNameRunes)
		}
		u, err := url.Parse(strings.TrimSpace(s.URL))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%w: mcp[%d].url 须为绝对 http(s) 地址", ErrInvalidValue, i)
		}
		if len([]rune(s.URL)) > AIMCPMaxURLRunes {
			return fmt.Errorf("%w: mcp[%d].url 过长（≤%d 字符）", ErrInvalidValue, i, AIMCPMaxURLRunes)
		}
		if len([]rune(s.AuthHeader)) > AIMCPMaxAuthHeaderRunes {
			return fmt.Errorf("%w: mcp[%d].auth_header 过长（≤%d 字符）", ErrInvalidValue, i, AIMCPMaxAuthHeaderRunes)
		}
	}
	return nil
}

// AI 场景键（ai.default_models JSON 的合法键）：chat 对话、summary 文件
// 摘要、edit 编辑器 AI；embedding 为 RAG 向量场景（不参与对话补全）。
const (
	AIScenarioChat      = "chat"
	AIScenarioSummary   = "summary"
	AIScenarioEdit      = "edit"
	AIScenarioEmbedding = "embedding"
)

// AIScenarios 全部合法场景键（校验与读取遍历用）。
var AIScenarios = []string{AIScenarioChat, AIScenarioSummary, AIScenarioEdit, AIScenarioEmbedding}

// AI Provider 类型（AIProvider.Kind 取值）：openai_compatible 一个实现
// 通吃 OpenAI/DeepSeek/Qwen/Ollama/vLLM 等兼容 /chat/completions 协议；
// anthropic 走 messages API；mock 为内置假实现（开发/测试/演示用）。
const (
	AIKindOpenAICompatible = "openai_compatible"
	AIKindAnthropic        = "anthropic"
	AIKindMock             = "mock"
)

// AIModelKind 为模型类型（互斥单选，Cherry Studio 语义）：一个模型只属
// 一类——chat 对话补全 / embedding 向量 / rerank 重排 / image 图像生成。
const (
	AIModelKindChat      = "chat"
	AIModelKindEmbedding = "embedding"
	AIModelKindRerank    = "rerank"
	AIModelKindImage     = "image"
)

// ValidAIModelKind 报告 s 是否为合法模型类型。
func ValidAIModelKind(s string) bool {
	switch s {
	case AIModelKindChat, AIModelKindEmbedding, AIModelKindRerank, AIModelKindImage:
		return true
	}
	return false
}

// AIModelCapabilities 为模型类型+能力并集（Cherry Studio 语义）：
//   - 类型互斥单选：Kind 取 chat/embedding/rerank/image（空/非法经
//     NormalizeAIModelKind 归一，默认 chat）；
//   - 能力可并集勾选：推理思考（reasoning）/视觉（vision）/音频
//     （audio）/视频（video）。项目未发布，无旧形态兼容；类型判定统一
//     走 IsChat/IsEmbedding/IsRerank 方法。
type AIModelCapabilities struct {
	Kind      string `json:"kind"`
	Reasoning bool   `json:"reasoning"`
	Vision    bool   `json:"vision"`
	Audio     bool   `json:"audio"`
	Video     bool   `json:"video"`
}

func (c AIModelCapabilities) IsChat() bool      { return c.Kind == AIModelKindChat }
func (c AIModelCapabilities) IsEmbedding() bool { return c.Kind == AIModelKindEmbedding }
func (c AIModelCapabilities) IsRerank() bool    { return c.Kind == AIModelKindRerank }

// NormalizeAIModelKind 归一模型类型：Kind 空/非法一律归 chat（默认类型）。
func (c *AIModelCapabilities) NormalizeAIModelKind() {
	if kind := strings.TrimSpace(c.Kind); ValidAIModelKind(kind) {
		c.Kind = kind
	} else {
		c.Kind = AIModelKindChat
	}
}

// AIModel 为 Provider 下的一个模型条目（ai.providers[].models 元素）。
type AIModel struct {
	ID           string              `json:"id"`
	Label        string              `json:"label,omitempty"`
	Capabilities AIModelCapabilities `json:"capabilities"`
}

// AIModelRef 为场景默认模型引用（ai.default_models 的值）。
type AIModelRef struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

// AIProvider 为一个 AI 服务提供方条目（ai.providers JSON 数组元素）。
// APIKey 仅在 PUT 请求中携带（空 = 保持现值不覆盖）；读路径永不回显明文。
// Model 为旧字段（主模型），Models 非空时按「首个 chat 能力模型」自动同步；
// 限流（RequestsPerMin/DailyQuota）为 Provider 级每用户限额，0 = 用全局
// ai.per_user_per_min / 不限。
type AIProvider struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Kind    string    `json:"kind"`
	BaseURL string    `json:"base_url"`
	APIKey  string    `json:"api_key,omitempty"`
	Model   string    `json:"model"`
	Models  []AIModel `json:"models,omitempty"`
	Enabled bool      `json:"enabled"`
	// RequestsPerMin 每用户每分钟请求上限（0 = 全局 ai.per_user_per_min 兜底）。
	RequestsPerMin int `json:"requests_per_min,omitempty"`
	// DailyQuota 每用户每日请求上限（0 = 不限）。
	DailyQuota int `json:"daily_quota,omitempty"`
}

// EffectiveModels 返回生效模型列表：models 非空原样返回（读入即归一
// capabilities.kind——空/非法归 chat；本方法为全部模型消费路径的唯一
// 归一落点，覆盖 jsonb 读入、请求载荷与个人池适配，归一幂等且能力
// 并集字段原样保留，写回不丢）；为空时按旧 base_url+model 单模型语义
// 合成（chat 类型）。
func (p AIProvider) EffectiveModels() []AIModel {
	if len(p.Models) > 0 {
		for i := range p.Models {
			p.Models[i].Capabilities.NormalizeAIModelKind()
		}
		return p.Models
	}
	if id := strings.TrimSpace(p.Model); id != "" {
		return []AIModel{{ID: id, Capabilities: AIModelCapabilities{Kind: AIModelKindChat}}}
	}
	return nil
}

// ModelWithID 从生效模型列表中查找模型。
func (p AIProvider) ModelWithID(id string) (AIModel, bool) {
	id = strings.TrimSpace(id)
	for _, m := range p.EffectiveModels() {
		if m.ID == id {
			return m, true
		}
	}
	return AIModel{}, false
}

// PrimaryModel 主模型：首个 chat 能力模型 → 首个模型 → 旧 Model 字段。
func (p AIProvider) PrimaryModel() string {
	models := p.EffectiveModels()
	for _, m := range models {
		if m.Capabilities.IsChat() {
			return m.ID
		}
	}
	if len(models) > 0 {
		return models[0].ID
	}
	return strings.TrimSpace(p.Model)
}

// AIConfig 为 AI 运行时配置全集（providers + 默认项 + 限流）。
// Enabled 为总开关（ai.enabled）：nil = 未入库（自动——存在启用中的
// Provider 即视为启用）；显式 false 时无论 Provider 如何全站关闭。
// DefaultModels 为场景默认模型（chat/summary/edit/embedding）：下拉只列
// 具备对应能力的模型；summary/edit 未配置或无效时运行时回落 chat。
type AIConfig struct {
	Enabled         *bool                 `json:"enabled,omitempty"`
	Providers       []AIProvider          `json:"providers"`
	DefaultProvider string                `json:"default_provider"`
	DefaultModels   map[string]AIModelRef `json:"default_models,omitempty"`
	Temperature     float64               `json:"temperature"`
	MaxTokens       int                   `json:"max_tokens"`
	PerUserPerMin   int                   `json:"per_user_per_min"`
	RAG             AIRAGConfig           `json:"rag"`
	Search          AISearchConfig        `json:"search"`
	// OCR 为图片 OCR 配置（ai.ocr 键）：整体块零值 = 载荷未带（SetAI 保持
	// 现值，同 Search 语义）；AIOverrides 整体读替并钳制 max_image_bytes。
	OCR AIOCRConfig `json:"ocr"`
	// Personas 为平台人设（ai.personas 键）：仅作 PUT /admin/settings/ai
	// 的载荷/回显传输字段——运行时热读取走 AIPersonas()，AIOverrides 不
	// 读该键；SetAI 中载荷 nil = 保持现值（参照 RAG 块合并语义），非 nil
	//（含空数组 = 清空）校验后整块写入。
	Personas []AIPersonaDef `json:"personas,omitempty"`
}

type AIRAGConfig struct {
	Mode              string `json:"mode"`
	VectorEnabled     bool   `json:"vector_enabled"`
	QdrantURL         string `json:"qdrant_url"`
	CollectionPrefix  string `json:"collection_prefix"`
	EmbeddingProvider string `json:"embedding_provider"`
	EmbeddingModel    string `json:"embedding_model"`
	// RerankProvider/RerankModel 重排序目标（Provider ID + 模型 ID，从具备
	// rerank 能力的模型中选择；空 = 不重排）。
	RerankProvider string `json:"rerank_provider"`
	RerankModel    string `json:"rerank_model"`
	TopK           int    `json:"top_k"`
	ChunkSize      int    `json:"chunk_size"`
	ChunkOverlap   int    `json:"chunk_overlap"`
}

// AISearchConfig 为联网搜索配置（chat 的 web_search 增强）。Provider 为 ""
// = 禁用；TavilyAPIKey 仅在 PUT 请求携带（空 = 保持现值），读路径永不回显。
type AISearchConfig struct {
	Provider     string `json:"provider"`
	SearxngURL   string `json:"searxng_url"`
	TavilyAPIKey string `json:"tavily_api_key,omitempty"`
	MaxResults   int    `json:"max_results"`
}

// AIOCRConfig 为图片 OCR 配置（ai.ocr 键整体 JSON 块）：开启后索引管道对
// 图片文件调用具备「视觉图片」能力的模型提取文字，结果入全文+向量索引。
// 可选增强——disabled 时无意义，故 ValidateAI 不做强制校验；ProviderID/
// ModelID 指向启用中 Provider 的视觉模型，指向不存在目标时运行时安静
// 跳过（索引回退仅名称）。
type AIOCRConfig struct {
	Enabled       bool   `json:"enabled"`
	ProviderID    string `json:"provider_id"`
	ModelID       string `json:"model_id"`
	MaxImageBytes int64  `json:"max_image_bytes"`
}

// ProviderByID 按 ID 查找 Provider。
func (c AIConfig) ProviderByID(id string) (AIProvider, bool) {
	for _, p := range c.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return AIProvider{}, false
}

// ResolveDefaultModel 解析对话场景（chat/summary/edit）的默认模型：
// DefaultModels[scenario] 未命中或指向未启用 Provider / 无 chat 能力时
// 回落 chat，再回落默认 Provider / 首个启用 Provider 的主模型。
// 未命中返回 ok=false（无任何可用 Provider）。
func (c AIConfig) ResolveDefaultModel(scenario string) (AIProvider, AIModel, bool) {
	if scenario == "" || scenario == AIScenarioEmbedding {
		scenario = AIScenarioChat
	}
	for _, s := range []string{scenario, AIScenarioChat} {
		ref, ok := c.DefaultModels[s]
		if !ok {
			continue
		}
		if p, ok2 := c.ProviderByID(ref.ProviderID); ok2 && p.Enabled {
			if m, ok3 := p.ModelWithID(ref.ModelID); ok3 && m.Capabilities.IsChat() {
				return p, m, true
			}
		}
	}
	if p, ok := c.ProviderByID(c.DefaultProvider); ok && p.Enabled {
		if id := p.PrimaryModel(); id != "" {
			return p, AIModel{ID: id, Capabilities: AIModelCapabilities{Kind: AIModelKindChat}}, true
		}
	}
	for _, p := range c.Providers {
		if p.Enabled {
			if id := p.PrimaryModel(); id != "" {
				return p, AIModel{ID: id, Capabilities: AIModelCapabilities{Kind: AIModelKindChat}}, true
			}
		}
	}
	return AIProvider{}, AIModel{}, false
}

// ResolveEmbeddingTarget 解析 RAG embedding 目标（Provider + 模型）：
// embedding_provider 为已配置 Provider ID 且 embedding_model 具备
// embedding 能力时命中；旧值 openai_compatible 回落默认 Provider（按模型
// 名直连，不校验能力——兼容旧配置无能力标注）。mock 由调用方自行处理。
func (c AIConfig) ResolveEmbeddingTarget() (AIProvider, string, bool) {
	switch c.RAG.EmbeddingProvider {
	case "", "mock", AIKindOpenAICompatible:
		// 旧值 openai_compatible / 空串：回落默认（或首个启用）的
		// openai_compatible Provider，沿用配置的 embedding_model。
		p, ok := c.ProviderByID(c.DefaultProvider)
		if !ok || !p.Enabled || p.Kind != AIKindOpenAICompatible {
			for _, cand := range c.Providers {
				if cand.Enabled && cand.Kind == AIKindOpenAICompatible {
					p, ok = cand, true
					break
				}
			}
		}
		if ok && p.Enabled && strings.TrimSpace(c.RAG.EmbeddingModel) != "" {
			return p, strings.TrimSpace(c.RAG.EmbeddingModel), true
		}
		return AIProvider{}, "", false
	default:
		p, ok := c.ProviderByID(c.RAG.EmbeddingProvider)
		if !ok {
			return AIProvider{}, "", false
		}
		m, ok := p.ModelWithID(c.RAG.EmbeddingModel)
		if !ok || !m.Capabilities.IsEmbedding() {
			return AIProvider{}, "", false
		}
		return p, m.ID, true
	}
}

// EffectiveEnabled 返回总开关的生效语义：显式 false → false；否则存在
// 启用中的 Provider → true。
func (c AIConfig) EffectiveEnabled() bool {
	if c.Enabled != nil && !*c.Enabled {
		return false
	}
	for _, p := range c.Providers {
		if p.Enabled {
			return true
		}
	}
	return false
}

// AI 配置默认值（env 未配置且库无覆盖时的回退基线）。
const (
	AITemperatureDefault   = 0.3
	AIMaxTokensDefault     = 2048
	AIPerUserPerMinDefault = 20
	// AISearchMaxResultsDefault 联网搜索默认召回条数。
	AISearchMaxResultsDefault = 5
	// AISearchMaxResultsLimit 联网搜索召回条数上限。
	AISearchMaxResultsLimit = 10
	// AIOCRMaxBytesDefault 单图 OCR 默认大小上限（8MiB）。
	AIOCRMaxBytesDefault int64 = 8 << 20
	// AIOCRMaxBytesLimit 单图 OCR 大小上限硬顶（32MiB）：防误配超大值
	// 拖垮请求体与视觉模型上下文，读取/写入两侧统一钳制。
	AIOCRMaxBytesLimit int64 = 32 << 20
)

// DefaultAIConfig 返回默认 AI 配置（无 Provider）。
func DefaultAIConfig() AIConfig {
	return AIConfig{Temperature: AITemperatureDefault, MaxTokens: AIMaxTokensDefault, PerUserPerMin: AIPerUserPerMinDefault, RAG: AIRAGConfig{Mode: "keyword", QdrantURL: "http://qdrant:6333", CollectionPrefix: "docflow_", EmbeddingProvider: "mock", EmbeddingModel: "text-embedding-3-small", TopK: 8, ChunkSize: 1000, ChunkOverlap: 100}, Search: AISearchConfig{MaxResults: AISearchMaxResultsDefault}}
}

// ValidateAIProvider 校验单个 Provider 条目（ID 唯一性由 ValidateAI 统一判）。
// mock 类型无需 baseURL/apiKey；其余类型 baseURL 须为绝对 http(s) URL、
// models 非空时逐条校验（ID 必填且不重复），models 为空时沿用旧 model
// 必填校验（旧配置单模型语义）。Provider 级限流范围：requests_per_min
// 0-10000、daily_quota 0-1000000。
func ValidateAIProvider(p AIProvider) error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("%w: provider id is required", ErrInvalidValue)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("%w: provider name is required", ErrInvalidValue)
	}
	if p.RequestsPerMin < 0 || p.RequestsPerMin > 10000 {
		return fmt.Errorf("%w: provider requests_per_min must be 0-10000", ErrInvalidValue)
	}
	if p.DailyQuota < 0 || p.DailyQuota > 1000000 {
		return fmt.Errorf("%w: provider daily_quota must be 0-1000000", ErrInvalidValue)
	}
	seenModel := make(map[string]bool, len(p.Models))
	for _, m := range p.Models {
		if strings.TrimSpace(m.ID) == "" {
			return fmt.Errorf("%w: provider %q model id is required", ErrInvalidValue, p.ID)
		}
		if k := strings.TrimSpace(m.Capabilities.Kind); k != "" && !ValidAIModelKind(k) {
			return fmt.Errorf("%w: provider %q model %q capabilities.kind must be chat/embedding/rerank/image", ErrInvalidValue, p.ID, m.ID)
		}
		if seenModel[m.ID] {
			return fmt.Errorf("%w: provider %q duplicate model id %q", ErrInvalidValue, p.ID, m.ID)
		}
		seenModel[m.ID] = true
	}
	switch p.Kind {
	case AIKindMock:
		return nil
	case AIKindOpenAICompatible, AIKindAnthropic:
		if p.BaseURL == "" {
			return fmt.Errorf("%w: provider base_url is required", ErrInvalidValue)
		}
		if u, err := url.Parse(p.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%w: provider base_url must be an absolute http(s) URL", ErrInvalidValue)
		}
		if len(p.Models) == 0 && strings.TrimSpace(p.Model) == "" {
			return fmt.Errorf("%w: provider model is required", ErrInvalidValue)
		}
		return nil
	default:
		return fmt.Errorf("%w: provider kind must be openai_compatible/anthropic/mock", ErrInvalidValue)
	}
}

// ValidateAI 校验 AI 配置整体：逐 Provider 校验 + ID 唯一 + 默认 Provider
// 存在性 + 场景默认模型引用（chat/summary/edit 须 chat 能力、embedding 须
// embedding 能力）+ 温度/token/限流范围 + RAG（embedding/rerank 须指向
// 已配置 Provider 的对应能力模型）。
func ValidateAI(c AIConfig) error {
	seen := make(map[string]bool, len(c.Providers))
	for _, p := range c.Providers {
		if err := ValidateAIProvider(p); err != nil {
			return err
		}
		if seen[p.ID] {
			return fmt.Errorf("%w: duplicate provider id %q", ErrInvalidValue, p.ID)
		}
		seen[p.ID] = true
	}
	if c.DefaultProvider != "" && !seen[c.DefaultProvider] {
		return fmt.Errorf("%w: default provider %q does not exist", ErrInvalidValue, c.DefaultProvider)
	}
	if err := validateDefaultModels(c); err != nil {
		return err
	}
	if c.Temperature < 0 || c.Temperature > 2 {
		return fmt.Errorf("%w: ai.temperature must be 0-2", ErrInvalidValue)
	}
	if c.MaxTokens < 1 || c.MaxTokens > 128000 {
		return fmt.Errorf("%w: ai.max_tokens must be 1-128000", ErrInvalidValue)
	}
	if c.PerUserPerMin < 0 || c.PerUserPerMin > 10000 {
		return fmt.Errorf("%w: ai.per_user_per_min must be 0-10000", ErrInvalidValue)
	}
	r := c.RAG
	if r.Mode == "" {
		r = DefaultAIConfig().RAG
	}
	if r.Mode != "keyword" && r.Mode != "hybrid" {
		return fmt.Errorf("%w: ai.rag.mode must be keyword or hybrid", ErrInvalidValue)
	}
	if r.TopK < 1 || r.TopK > 10 || r.ChunkSize < 100 || r.ChunkSize > 10000 || r.ChunkOverlap < 0 || r.ChunkOverlap >= r.ChunkSize {
		return fmt.Errorf("%w: invalid rag top_k/chunk_size/chunk_overlap", ErrInvalidValue)
	}
	if r.VectorEnabled && r.Mode == "hybrid" {
		u, err := url.Parse(r.QdrantURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%w: invalid qdrant_url", ErrInvalidValue)
		}
		if err := validateRAGModelRef(c, r.EmbeddingProvider, r.EmbeddingModel, "embedding", true); err != nil {
			return err
		}
		if r.RerankProvider != "" || r.RerankModel != "" {
			if r.RerankProvider == "" || r.RerankModel == "" {
				return fmt.Errorf("%w: rerank provider and model must be set together", ErrInvalidValue)
			}
			if err := validateRAGModelRef(c, r.RerankProvider, r.RerankModel, "rerank", false); err != nil {
				return err
			}
		}
		if r.CollectionPrefix == "" {
			return fmt.Errorf("%w: collection_prefix required", ErrInvalidValue)
		}
	}
	return validateAISearch(c.Search)
}

// validateAISearch 校验联网搜索配置：provider 取值合法；searxng 须配
// 绝对 http(s) 的 searxng_url；tavily 须配 api_key；max_results 范围
// 1-10（0 = 默认 5，由合并方回退后再校验）。
func validateAISearch(s AISearchConfig) error {
	switch s.Provider {
	case "":
		return nil
	case AISearchProviderSearxng:
		u, err := url.Parse(strings.TrimSpace(s.SearxngURL))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%w: ai.search.searxng_url must be an absolute http(s) URL when provider is searxng", ErrInvalidValue)
		}
	case AISearchProviderTavily:
		if strings.TrimSpace(s.TavilyAPIKey) == "" {
			return fmt.Errorf("%w: ai.search.tavily_api_key is required when provider is tavily", ErrInvalidValue)
		}
	default:
		return fmt.Errorf("%w: ai.search.provider must be empty, searxng or tavily", ErrInvalidValue)
	}
	if s.MaxResults < 0 || s.MaxResults > AISearchMaxResultsLimit {
		return fmt.Errorf("%w: ai.search.max_results must be 0-%d", ErrInvalidValue, AISearchMaxResultsLimit)
	}
	return nil
}

// validateDefaultModels 校验场景默认模型引用：键限 chat/summary/edit/
// embedding；provider 必须存在、模型必须在其生效列表且具备对应能力
// （对话类场景 → chat；embedding → embedding）。
func validateDefaultModels(c AIConfig) error {
	for scenario, ref := range c.DefaultModels {
		switch scenario {
		case AIScenarioChat, AIScenarioSummary, AIScenarioEdit, AIScenarioEmbedding:
		default:
			return fmt.Errorf("%w: unknown default model scenario %q", ErrInvalidValue, scenario)
		}
		p, ok := c.ProviderByID(ref.ProviderID)
		if !ok {
			return fmt.Errorf("%w: default %s model provider %q does not exist", ErrInvalidValue, scenario, ref.ProviderID)
		}
		m, ok := p.ModelWithID(ref.ModelID)
		if !ok {
			return fmt.Errorf("%w: default %s model %q not in provider %q", ErrInvalidValue, scenario, ref.ModelID, ref.ProviderID)
		}
		if scenario == AIScenarioEmbedding {
			if !m.Capabilities.IsEmbedding() {
				return fmt.Errorf("%w: default embedding model %q lacks embedding capability", ErrInvalidValue, ref.ModelID)
			}
			continue
		}
		if !m.Capabilities.IsChat() {
			return fmt.Errorf("%w: default %s model %q lacks chat capability", ErrInvalidValue, scenario, ref.ModelID)
		}
	}
	return nil
}

// validateRAGModelRef 校验 RAG 的 embedding/rerank 目标：mock 直接过、
// 旧值 openai_compatible（仅 embedding）沿用旧语义（模型名必填）、否则
// 须为已配置 Provider 的具备对应能力的模型。
func validateRAGModelRef(c AIConfig, providerID, modelID, capability string, allowLegacy bool) error {
	switch providerID {
	case "mock":
		return nil
	case AIKindOpenAICompatible:
		if !allowLegacy {
			return fmt.Errorf("%w: invalid %s provider", ErrInvalidValue, capability)
		}
		if strings.TrimSpace(modelID) == "" {
			return fmt.Errorf("%w: embedding_model required", ErrInvalidValue)
		}
		return nil
	}
	p, ok := c.ProviderByID(providerID)
	if !ok {
		return fmt.Errorf("%w: invalid %s provider %q", ErrInvalidValue, capability, providerID)
	}
	m, ok := p.ModelWithID(modelID)
	if !ok {
		return fmt.Errorf("%w: %s model %q not in provider %q", ErrInvalidValue, capability, modelID, providerID)
	}
	hasCap := capability == "embedding" && m.Capabilities.IsEmbedding() || capability == "rerank" && m.Capabilities.IsRerank()
	if !hasCap {
		return fmt.Errorf("%w: %s model %q lacks %s capability", ErrInvalidValue, capability, modelID, capability)
	}
	return nil
}

// SMTPSettings 为 SMTP 投递参数全集（env 基线 / 入库覆盖 / 请求载荷共用）。
// Pass 仅在 PUT 请求中携带（空 = 保持现值不覆盖）；任何读路径都不回显。
type SMTPSettings struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	Pass    string `json:"pass,omitempty"`
	From    string `json:"from"`
	TLSMode string `json:"tls_mode"`
}

// SMTPOverride 为 system_settings 中已入库的 SMTP 覆盖项（nil 指针 = 该键
// 未入库，沿用 env 基线；Pass 仅在入库为非空字符串时覆盖）。
type SMTPOverride struct {
	Enabled *bool
	Host    *string
	Port    *int
	User    *string
	Pass    *string
	From    *string
	TLSMode *string
}

// Apply 把入库覆盖合并到 env 基线，返回生效配置。
func (o SMTPOverride) Apply(env SMTPSettings) SMTPSettings {
	out := env
	if o.Enabled != nil {
		out.Enabled = *o.Enabled
	}
	if o.Host != nil {
		out.Host = *o.Host
	}
	if o.Port != nil {
		out.Port = *o.Port
	}
	if o.User != nil {
		out.User = *o.User
	}
	if o.Pass != nil && *o.Pass != "" {
		out.Pass = *o.Pass
	}
	if o.From != nil {
		out.From = *o.From
	}
	if o.TLSMode != nil && *o.TLSMode != "" {
		out.TLSMode = *o.TLSMode
	}
	return out
}

// smtpKeyTypes 为各 SMTP 键的 value_type（写入 system_settings 行用）。
var smtpKeyTypes = map[string]string{
	KeySMTPEnabled: TypeBool,
	KeySMTPHost:    TypeString,
	KeySMTPPort:    TypeInt,
	KeySMTPUser:    TypeString,
	KeySMTPPass:    TypeString,
	KeySMTPFrom:    TypeString,
	KeySMTPTLSMode: TypeString,
}

// ValidateSMTP 校验生效配置的完整性（enabled 时 host/from 必填、port 与
// TLS 模式合法；Pass 不参与校验）。返回 ErrInvalidValue 语义错误。
func ValidateSMTP(s SMTPSettings) error {
	if !s.Enabled {
		return nil
	}
	if s.Host == "" {
		return fmt.Errorf("%w: smtp.host is required when smtp.enabled", ErrInvalidValue)
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("%w: smtp.port must be 1-65535", ErrInvalidValue)
	}
	if s.From == "" {
		return fmt.Errorf("%w: smtp.from is required when smtp.enabled", ErrInvalidValue)
	}
	switch s.TLSMode {
	case "", SMTPTLSModeAuto, SMTPTLSModeSSL, SMTPTLSModeNone:
	default:
		return fmt.Errorf("%w: smtp.tls_mode must be auto/ssl/none", ErrInvalidValue)
	}
	return nil
}

// Definition 是一个内置键的元数据：类型、默认值、取值范围、描述与生效方式。
type Definition struct {
	Key         string
	Type        string
	Default     any
	Description string
	// Min/Max 仅对 int 类型生效（含边界）；nil 表示不限。
	Min *int64
	Max *int64
	// Effect 表示变更的生效方式（EffectImmediate/NewSession/Restart）：
	// immediate 为消费方每次请求热读取；restart 为启动装配读取（当前
	// security.* 限流与登录锁定参数均由 env 在启动时注入，须重启生效）。
	Effect string
}

func intPtr(v int64) *int64 { return &v }

// Definitions 是全部内置键定义；键顺序即 GetAll 输出顺序。
// int 类型键的默认值统一存 int64（与 normalizeValue 归一化结果一致）。
var Definitions = []Definition{
	{Key: KeyAIRAGMode, Type: TypeString, Default: "keyword", Effect: EffectImmediate, Description: "RAG mode: keyword or hybrid"},
	{Key: KeyAIRAGVectorEnabled, Type: TypeBool, Default: false, Effect: EffectImmediate, Description: "Enable optional vector RAG"},
	{Key: KeyAIRAGQdrantURL, Type: TypeString, Default: "http://qdrant:6333", Effect: EffectRestart, Description: "Qdrant URL"},
	{Key: KeyAIRAGCollectionPrefix, Type: TypeString, Default: "docflow_", Effect: EffectRestart, Description: "Qdrant collection prefix"},
	{Key: KeyAIRAGEmbeddingProvider, Type: TypeString, Default: "mock", Effect: EffectRestart, Description: "Embedding provider"},
	{Key: KeyAIRAGEmbeddingModel, Type: TypeString, Default: "text-embedding-3-small", Effect: EffectRestart, Description: "Embedding model"},
	{Key: KeyAIRAGTopK, Type: TypeInt, Default: int64(8), Min: intPtr(1), Max: intPtr(100), Effect: EffectImmediate, Description: "Vector retrieval top K"},
	{Key: KeyAIRAGChunkSize, Type: TypeInt, Default: int64(1000), Min: intPtr(100), Max: intPtr(10000), Effect: EffectRestart, Description: "RAG chunk size"},
	{Key: KeyAIRAGChunkOverlap, Type: TypeInt, Default: int64(100), Min: intPtr(0), Max: intPtr(5000), Effect: EffectRestart, Description: "RAG chunk overlap"},
	{Key: KeyAISearchProvider, Type: TypeString, Default: "", Effect: EffectImmediate, Description: "AI web search provider: empty (disabled), searxng or tavily"},
	{Key: KeyAISearchSearxngURL, Type: TypeString, Default: "", Effect: EffectImmediate, Description: "SearXNG base URL (JSON API enabled), used when ai.search.provider=searxng"},
	{Key: KeyAISearchMaxResults, Type: TypeInt, Default: int64(AISearchMaxResultsDefault), Min: intPtr(1), Max: intPtr(AISearchMaxResultsLimit), Effect: EffectImmediate, Description: "AI web search max results per query"},
	{Key: KeyUploadMaxVersionsPerFile, Type: TypeInt, Default: int64(5), Min: intPtr(1), Max: intPtr(1000), Effect: EffectImmediate, Description: "每文件保留的版本数上限（覆盖上传后按版本号裁剪历史版本；与 upload.version_retention_days 组合生效）"},
	{Key: KeyUploadVersionRetentionDays, Type: TypeInt, Default: int64(0), Min: intPtr(0), Max: intPtr(3650), Effect: EffectImmediate, Description: "版本保留时间窗（天）：创建时间在窗口内的版本不因数量裁剪删除；0 = 不启用时间窗（默认，仅按数量上限裁剪）"},
	{Key: KeyUploadBlockedExtensions, Type: TypeString, Default: "", Effect: EffectImmediate, Description: "上传扩展名黑名单（逗号分隔，如 exe,bat,sh；不含点、大小写不敏感）：命中的文件名在建会话与完成时拒绝（400）；默认空 = 不拦截"},
	{Key: KeyUploadMaxFileSize, Type: TypeInt, Default: int64(1 << 30), Min: intPtr(1), Max: intPtr(1 << 40), Effect: EffectImmediate, Description: "单文件上传大小上限（字节）"},
	{Key: KeyUploadDefaultQuota, Type: TypeInt, Default: int64(10 << 30), Min: intPtr(1), Max: intPtr(1 << 50), Effect: EffectImmediate, Description: "新用户开户默认存储配额（字节，默认 10GiB）；仅对新创建用户生效，存量用户经管理端单独调整"},
	{Key: KeyShareDefaultExpiryHours, Type: TypeInt, Default: int64(168), Min: intPtr(1), Max: intPtr(8760), Effect: EffectImmediate, Description: "公开分享默认有效期（小时）"},
	{Key: KeyShareDefaultWatermark, Type: TypeBool, Default: true, Effect: EffectImmediate, Description: "新分享默认启用水印（创建请求未显式指定 watermark_enabled 时采用）"},
	{Key: KeyShareWatermarkText, Type: TypeString, Default: "{date} {name}", Effect: EffectImmediate, Description: "水印默认模板，支持 {email}/{date}/{name} 占位符（公开访问无登录身份，{email} 渲染为脱敏 IP 前缀）"},
	{Key: KeyRetentionTrashDays, Type: TypeInt, Default: int64(30), Min: intPtr(1), Max: intPtr(3650), Effect: EffectImmediate, Description: "回收站保留天数：软删除超过该天数后由后台清理任务彻底删除"},
	{Key: KeyRetentionAccessEventsDays, Type: TypeInt, Default: int64(90), Min: intPtr(1), Max: intPtr(3650), Effect: EffectImmediate, Description: "文件访问事件保留天数：超过该天数后由后台清理任务删除"},
	{Key: KeyRateLimitPerMinute, Type: TypeInt, Default: int64(120), Min: intPtr(0), Max: intPtr(100000), Effect: EffectRestart, Description: "认证 API 每分钟请求上限（须重启生效：限流器在启动时按 env 装配，本键当前无热读取消费方）"},
	{Key: KeyLoginMaxRetries, Type: TypeInt, Default: int64(5), Min: intPtr(1), Max: intPtr(100), Effect: EffectImmediate, Description: "登录/WebDAV 失败锁定阈值（同一用户名+IP）；即时生效，env LOGIN_MAX_RETRIES 仅为引导默认（键未入库时沿用 env 值）"},
	{Key: KeyLoginLockMinutes, Type: TypeInt, Default: int64(15), Min: intPtr(1), Max: intPtr(10080), Effect: EffectImmediate, Description: "防爆破锁定时长（分钟）；即时生效，env LOGIN_LOCK_MINUTES 仅为引导默认（键未入库时沿用 env 值）"},
	{Key: KeyMaxConcurrentUploads, Type: TypeInt, Default: int64(3), Min: intPtr(1), Max: intPtr(100), Effect: EffectImmediate, Description: "每用户并发上传会话上限：非终态会话（uploading/verifying/scanning）达到上限时新建会话返回 429"},
	{Key: KeyBatchMaxItems, Type: TypeInt, Default: int64(100), Min: intPtr(1), Max: intPtr(1000), Effect: EffectImmediate, Description: "批量操作单次最大项目数"},
	{Key: KeyFolderMaxDepth, Type: TypeInt, Default: int64(32), Min: intPtr(1), Max: intPtr(1000), Effect: EffectImmediate, Description: "目录最大深度（根为 1）：创建子目录与目录移动超过上限拒绝"},
	{Key: KeySharePublicEnabled, Type: TypeBool, Default: true, Effect: EffectImmediate, Description: "是否允许创建公开分享"},
	{Key: KeyScanQuarantinePolicy, Type: TypeString, Default: "quarantine", Effect: EffectImmediate, Description: "扫描失败处理策略：quarantine 或 reject（当前版本未接线：扫描失败一律隔离，隔离区可经管理端处置）"},
	{Key: KeyBackupEnabled, Type: TypeBool, Default: false, Effect: EffectRestart, Description: "是否启用备份任务"},
	{Key: KeyBackupRetentionDays, Type: TypeInt, Default: int64(30), Min: intPtr(1), Max: intPtr(3650), Effect: EffectRestart, Description: "备份保留天数"},
	{Key: KeyBackupEncryptionRequired, Type: TypeBool, Default: true, Effect: EffectRestart, Description: "是否要求备份加密"},
	{Key: KeyBackupLastVerify, Type: TypeString, Default: "", Effect: EffectRestart, Description: "最近一次备份验证时间"},
	{Key: KeyAuditRetentionDays, Type: TypeInt, Default: int64(90), Min: intPtr(0), Max: intPtr(3650), Effect: EffectImmediate, Description: "审计日志保留天数（0 = 永久保留）：后台清理任务每日删除超过保留期的审计记录"},
	{Key: KeySpaceDefaultQuota, Type: TypeInt, Default: int64(10 << 30), Min: intPtr(0), Max: intPtr(1 << 50), Effect: EffectImmediate, Description: "新空间默认存储配额（字节，默认 10GiB；0 = 不限）：新建空间与注册默认空间的初始配额"},
	{Key: KeySpaceMaxQuota, Type: TypeInt, Default: int64(1 << 40), Min: intPtr(0), Max: intPtr(1 << 50), Effect: EffectImmediate, Description: "空间配额上限（字节，默认 1TiB；0 = 不限）：空间 owner/admin 调整配额时不得超过，系统 admin 不受限"},
	{Key: KeySpaceMaxPerUser, Type: TypeInt, Default: int64(20), Min: intPtr(1), Max: intPtr(1000), Effect: EffectImmediate, Description: "每用户空间数上限（owner 维度计数，含默认空间）：超出后创建空间返回 413"},
	{Key: KeyWebDAVEnabled, Type: TypeBool, Default: false, Effect: EffectImmediate, Description: "启用 WebDAV 文件访问"},
	{Key: KeyCollabEnabled, Type: TypeBool, Default: true, Effect: EffectImmediate, Description: "启用富文本实时协作（/api/v1/collab/{fileId}/ws 协作房间；关闭时端点返回 404 且不创建房间）"},
	{Key: KeyAgentEnabled, Type: TypeBool, Default: false, Effect: EffectImmediate, Description: "Docker 沙箱（进阶）：默认关闭，需管理员显式开启（开启后 AI 创作空间可选 Docker 沙箱引擎）"},
	{Key: KeyAgentRuntime, Type: TypeString, Default: "docker", Effect: EffectRestart, Description: "Agent runtime（仅 docker）"},
	{Key: KeyAgentAllowedImages, Type: TypeString, Default: "", Effect: EffectImmediate, Description: "允许的 Agent 镜像，逗号分隔"},
	{Key: KeyAgentMaxConcurrent, Type: TypeInt, Default: int64(1), Min: intPtr(1), Max: intPtr(100), Effect: EffectImmediate, Description: "Agent 最大并发任务数"},
	{Key: KeyAgentDefaultTimeout, Type: TypeInt, Default: int64(900), Min: intPtr(1), Max: intPtr(86400), Effect: EffectImmediate, Description: "Agent 默认超时秒数"},
	{Key: KeyAgentMaxCPU, Type: TypeInt, Default: int64(1), Min: intPtr(1), Max: intPtr(64), Effect: EffectImmediate, Description: "Agent 最大 CPU 数"},
	{Key: KeyAgentMaxMemory, Type: TypeInt, Default: int64(512 << 20), Min: intPtr(1 << 20), Max: intPtr(1 << 40), Effect: EffectImmediate, Description: "Agent 最大内存字节数"},
	{Key: KeyAgentNetworkMode, Type: TypeString, Default: "none", Effect: EffectImmediate, Description: "Agent 网络模式 none/restricted"},
	{Key: KeyAgentCallbackURL, Type: TypeString, Default: "", Effect: EffectImmediate, Description: "受限 MCP 回调基地址（不含凭据）"},
	{Key: KeyAgentAllowAI, Type: TypeBool, Default: true, Effect: EffectImmediate, Description: "允许 Agent 容器经 IPC socket（unix domain，NetworkMode=none 下仍可用）调用平台默认对话模型；关闭时创建任务不签发 AI 令牌、容器不注入 DOCFLOW_AI_TOKEN"},
	{Key: KeyAgentAIMaxCalls, Type: TypeInt, Default: int64(40), Min: intPtr(1), Max: intPtr(10000), Effect: EffectImmediate, Description: "单个 Agent 任务经 IPC socket 调用平台 AI 的次数上限（超出返回 429，令牌随任务终态注销）"},
	{Key: KeyAgentSyncMode, Type: TypeString, Default: "git", Effect: EffectImmediate, Description: "Agent 产物同步模式：git 优先读取容器内 runner 产出的 .docflow-changes.json（A/M/D 清单）构造 diff，缺失/非法时回退全量扫描；scan 恒走全量扫描"},
	{Key: KeyAgentHarness, Type: TypeString, Default: "auto", Effect: EffectImmediate, Description: "Agent 执行引擎：auto 按平台默认模型协议自动选择（Anthropic→Claude Code，OpenAI 兼容→pi）；builtin=内置轻量 runner"},
}

// DefinitionByKey 返回键定义；未知键返回 ErrUnknownKey。
func DefinitionByKey(key string) (Definition, error) {
	for _, d := range Definitions {
		if d.Key == key {
			return d, nil
		}
	}
	return Definition{}, ErrUnknownKey
}

// normalizeValue 按定义校验并归一化请求值（JSON 反序列化后 bool→bool、
// 数字→float64、字符串→string），返回按定义类型的强类型值。
func normalizeValue(d Definition, value any) (any, error) {
	switch d.Type {
	case TypeBool:
		b, ok := value.(bool)
		if !ok {
			return nil, ErrInvalidType
		}
		return b, nil
	case TypeInt:
		var n int64
		switch v := value.(type) {
		case float64: // encoding/json 的数字类型
			if v != float64(int64(v)) {
				return nil, ErrInvalidValue
			}
			n = int64(v)
		case int:
			n = int64(v)
		case int64:
			n = v
		default:
			return nil, ErrInvalidType
		}
		if (d.Min != nil && n < *d.Min) || (d.Max != nil && n > *d.Max) {
			return nil, ErrInvalidValue
		}
		return n, nil
	case TypeString:
		s, ok := value.(string)
		if !ok {
			return nil, ErrInvalidType
		}
		return s, nil
	default:
		return nil, ErrInvalidType
	}
}

// Setting 对应 system_settings 表一行。
type Setting struct {
	Key         string     `gorm:"column:key;primaryKey"`
	ValueJSON   string     `gorm:"column:value_json;type:jsonb;not null"`
	ValueType   string     `gorm:"column:value_type;size:16;not null"`
	Description string     `gorm:"column:description;not null"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;not null"`
	UpdatedBy   *uuid.UUID `gorm:"column:updated_by;type:uuid"`
}

// TableName 显式映射到 system_settings（gorm 默认复数化为 settings）。
func (Setting) TableName() string { return "system_settings" }

// SettingView 是管理 API 的响应视图：解析后的值 + 类型、描述与生效方式。
type SettingView struct {
	Key         string     `json:"key"`
	Value       any        `json:"value"`
	Type        string     `json:"type"`
	Description string     `json:"description"`
	Default     any        `json:"default"`
	Effect      string     `json:"effect"`
	UpdatedAt   time.Time  `json:"updated_at,omitempty"`
	UpdatedBy   *uuid.UUID `json:"updated_by,omitempty"`
}

// repository 抽象 system_settings 的数据访问；生产实现为 gormRepository，
// 测试可用内存实现（模式同 files 包的 repo 抽象）。
type repository interface {
	// GetRow 返回键行；无行返回 ErrNotSet。
	GetRow(key string) (Setting, error)
	// ListRows 返回全部已存储行。
	ListRows() ([]Setting, error)
	// UpsertRow 按主键 upsert。
	UpsertRow(s Setting) error
}

type gormRepository struct{ db *gorm.DB }

func (g *gormRepository) GetRow(key string) (Setting, error) {
	var s Setting
	err := g.db.First(&s, "key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Setting{}, ErrNotSet
	}
	return s, err
}

func (g *gormRepository) ListRows() ([]Setting, error) {
	var out []Setting
	err := g.db.Order("key").Find(&out).Error
	return out, err
}

func (g *gormRepository) UpsertRow(s Setting) error {
	return g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value_json", "value_type", "description", "updated_by", "updated_at"}),
	}).Create(&s).Error
}

// Store 提供设置的读取与写入；每次 Get 直读数据库（热读取，变更即时生效）。
type Store struct {
	repo  repository
	audit audit.Recorder
	now   func() time.Time
}

func NewStore(db *gorm.DB) *Store {
	return &Store{repo: &gormRepository{db: db}, audit: audit.NopRecorder{}, now: time.Now}
}

// SetAuditRecorder 注入审计写入器；nil 保持 Nop（模式同 http.Handler）。
func (s *Store) SetAuditRecorder(recorder audit.Recorder) {
	if recorder != nil {
		s.audit = recorder
	}
}

// decode 按定义类型把 value_json 解析为强类型值。
func decode(d Definition, raw string) (any, error) {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, err
	}
	return normalizeValue(d, value)
}

// Get 返回键的当前值；未设置或解析失败时回退默认值（热读取的容错默认）。
func (s *Store) Get(key string) (any, error) {
	d, err := DefinitionByKey(key)
	if err != nil {
		return nil, err
	}
	row, err := s.repo.GetRow(key)
	if err != nil {
		if errors.Is(err, ErrNotSet) {
			return d.Default, nil
		}
		return nil, err
	}
	value, err := decode(d, row.ValueJSON)
	if err != nil {
		return d.Default, nil
	}
	return value, nil
}

// GetAll 返回全部内置键的视图：已设置的取存储值，未设置的取默认值。
func (s *Store) GetAll() ([]SettingView, error) {
	rows, err := s.repo.ListRows()
	if err != nil {
		return nil, err
	}
	stored := make(map[string]Setting, len(rows))
	for _, r := range rows {
		stored[r.Key] = r
	}
	out := make([]SettingView, 0, len(Definitions))
	for _, d := range Definitions {
		view := SettingView{Key: d.Key, Type: d.Type, Description: d.Description, Default: d.Default, Value: d.Default, Effect: d.Effect}
		if row, ok := stored[d.Key]; ok {
			if value, err := decode(d, row.ValueJSON); err == nil {
				view.Value = value
			}
			view.UpdatedAt = row.UpdatedAt
			view.UpdatedBy = row.UpdatedBy
		}
		out = append(out, view)
	}
	return out, nil
}

// Set 校验并写入键值（actor 记入 updated_by），成功后写 settings.update 审计。
// 未知键返回 ErrUnknownKey；类型/范围不符返回 ErrInvalidType/ErrInvalidValue。
// 返回归一化后的值（供响应回显）。
func (s *Store) Set(key string, value any, actor uuid.UUID) (any, error) {
	d, err := DefinitionByKey(key)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeValue(d, value)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	if err := s.repo.UpsertRow(Setting{Key: key, ValueJSON: string(raw), ValueType: d.Type, Description: d.Description, UpdatedBy: &actor, UpdatedAt: s.now().UTC()}); err != nil {
		return nil, err
	}
	metadata, _ := json.Marshal(map[string]any{"key": key, "value": normalized})
	_ = s.audit.Record(audit.Entry{UserID: &actor, Action: audit.ActionSettingsUpdate, ResourceType: audit.ResourceSettings, ResourceID: key, Status: audit.StatusSuccess, Metadata: string(metadata)})
	return normalized, nil
}

// GetInt 返回 int 类型键的当前值（热读取）；未知键或值非法返回错误，
// 由调用方决定回退（如回退 config 默认值）。
func (s *Store) GetInt(key string) (int, error) {
	value, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	n, ok := value.(int64)
	if !ok {
		return 0, fmt.Errorf("settings key %s is not int", key)
	}
	return int(n), nil
}

// GetIntDefined 返回 int 类型键的入库值（热读取）：键在 system_settings
// 有行且值可解析时返回 (值, true)；未入库（ErrNotSet）、行损坏或键未知
// 返回 (0, false)。与 Get 的「回退定义默认值」不同，本方法区分「未配置」，
// 供消费方回落自身基线（如 env 注入值——env 为引导默认、运行时键优先，
// 典型消费方：登录/WebDAV 防爆破锁定参数）。
func (s *Store) GetIntDefined(key string) (int, bool) {
	d, err := DefinitionByKey(key)
	if err != nil {
		return 0, false
	}
	row, err := s.repo.GetRow(key)
	if err != nil {
		return 0, false
	}
	value, err := decode(d, row.ValueJSON)
	if err != nil {
		return 0, false
	}
	n, ok := value.(int64)
	if !ok {
		return 0, false
	}
	return int(n), true
}

// GetBool 返回 bool 类型键的当前值（热读取）；未知键或值非法返回错误，
// 由调用方决定回退。
func (s *Store) GetBool(key string) (bool, error) {
	value, err := s.Get(key)
	if err != nil {
		return false, err
	}
	b, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("settings key %s is not bool", key)
	}
	return b, nil
}

// GetString 返回 string 类型键的当前值（热读取）；未知键或值非法返回错误，
// 由调用方决定回退。
func (s *Store) GetString(key string) (string, error) {
	value, err := s.Get(key)
	if err != nil {
		return "", err
	}
	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("settings key %s is not string", key)
	}
	return str, nil
}

// SMTPOverrides 读取全部 smtp.* 入库覆盖项（未入库的键保持 nil 指针，
// 与入库值恰为零值区分）。DB 异常返回错误，调用方回退 env 基线投递。
func (s *Store) SMTPOverrides() (SMTPOverride, error) {
	var out SMTPOverride
	// read：行存在且 JSON 可解析时置指针；行不存在（ErrNotSet）保持 nil。
	readBool := func(key string, dst **bool) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		var v bool
		if err := json.Unmarshal([]byte(row.ValueJSON), &v); err != nil {
			return err
		}
		*dst = &v
		return nil
	}
	readInt := func(key string, dst **int) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		var v int64
		if err := json.Unmarshal([]byte(row.ValueJSON), &v); err != nil {
			return err
		}
		n := int(v)
		*dst = &n
		return nil
	}
	readString := func(key string, dst **string) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		var v string
		if err := json.Unmarshal([]byte(row.ValueJSON), &v); err != nil {
			return err
		}
		*dst = &v
		return nil
	}
	if err := readBool(KeySMTPEnabled, &out.Enabled); err != nil {
		return out, err
	}
	if err := readString(KeySMTPHost, &out.Host); err != nil {
		return out, err
	}
	if err := readInt(KeySMTPPort, &out.Port); err != nil {
		return out, err
	}
	if err := readString(KeySMTPUser, &out.User); err != nil {
		return out, err
	}
	if err := readString(KeySMTPPass, &out.Pass); err != nil {
		return out, err
	}
	if err := readString(KeySMTPFrom, &out.From); err != nil {
		return out, err
	}
	if err := readString(KeySMTPTLSMode, &out.TLSMode); err != nil {
		return out, err
	}
	return out, nil
}

// SetSMTP 写入 SMTP 配置（与 env 基线合并校验：Pass 留空 = 保持现值），
// 成功后写 settings.update 审计（Pass 以 "******" 打码，绝不记录明文）。
func (s *Store) SetSMTP(in SMTPSettings, env SMTPSettings, actor uuid.UUID) (SMTPSettings, error) {
	// 与现值合并出完整生效配置再校验（避免「只改 host、库里无 port」被误拒）。
	current := env
	if ov, err := s.SMTPOverrides(); err == nil {
		current = ov.Apply(env)
	}
	merged := SMTPSettings{
		Enabled: in.Enabled,
		Host:    strings.TrimSpace(in.Host),
		Port:    in.Port,
		User:    strings.TrimSpace(in.User),
		Pass:    in.Pass,
		From:    strings.TrimSpace(in.From),
		TLSMode: in.TLSMode,
	}
	if merged.Host == "" {
		merged.Host = current.Host
	}
	if merged.Port == 0 {
		merged.Port = current.Port
	}
	if merged.User == "" {
		merged.User = current.User
	}
	if merged.From == "" {
		merged.From = current.From
	}
	if merged.TLSMode == "" {
		merged.TLSMode = current.TLSMode
	}
	if err := ValidateSMTP(merged); err != nil {
		return SMTPSettings{}, err
	}
	now := s.now().UTC()
	writes := []struct {
		key  string
		val  any
		skip bool
	}{
		{KeySMTPEnabled, merged.Enabled, false},
		{KeySMTPHost, merged.Host, false},
		{KeySMTPPort, int64(merged.Port), false},
		{KeySMTPUser, merged.User, false},
		// 密码只写不读：请求为空 = 保持现值（不覆盖为空串）。
		{KeySMTPPass, merged.Pass, merged.Pass == ""},
		{KeySMTPFrom, merged.From, false},
		{KeySMTPTLSMode, merged.TLSMode, false},
	}
	for _, w := range writes {
		if w.skip {
			continue
		}
		raw, err := json.Marshal(w.val)
		if err != nil {
			return SMTPSettings{}, err
		}
		row := Setting{Key: w.key, ValueJSON: string(raw), ValueType: smtpKeyTypes[w.key], Description: "SMTP runtime override", UpdatedBy: &actor, UpdatedAt: now}
		if err := s.repo.UpsertRow(row); err != nil {
			return SMTPSettings{}, err
		}
	}
	audited := SMTPSettings{
		Enabled: merged.Enabled, Host: merged.Host, Port: merged.Port,
		User: merged.User, Pass: "", From: merged.From, TLSMode: merged.TLSMode,
	}
	if merged.User != "" {
		audited.Pass = "******"
	}
	metadata, _ := json.Marshal(map[string]any{"key": "smtp", "value": audited})
	_ = s.audit.Record(audit.Entry{UserID: &actor, Action: audit.ActionSettingsUpdate, ResourceType: audit.ResourceSettings, ResourceID: "smtp", Status: audit.StatusSuccess, Metadata: string(metadata)})
	return merged, nil
}

// AIOverrides 读取全部 ai.* 入库覆盖项（无行 = 零值/nil，读取失败由调用方
// 回退 env 基线）。providers 行为 JSON 数组整体读替（不做元素级合并）；
// 第二返回值表示 providers 行是否已入库。
func (s *Store) AIOverrides() (AIConfig, bool, error) {
	out := DefaultAIConfig()
	set := false
	row, err := s.repo.GetRow(KeyAIProviders)
	if err != nil {
		if !errors.Is(err, ErrNotSet) {
			return out, false, err
		}
	} else if err := json.Unmarshal([]byte(row.ValueJSON), &out.Providers); err != nil {
		return DefaultAIConfig(), false, nil
	} else {
		set = true
	}
	readString := func(key string, dst *string) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(row.ValueJSON), dst)
	}
	readFloat := func(key string, dst *float64) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(row.ValueJSON), dst)
	}
	readInt := func(key string, dst *int) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		var n int64
		if err := json.Unmarshal([]byte(row.ValueJSON), &n); err != nil {
			return err
		}
		*dst = int(n)
		return nil
	}
	readBool := func(key string, dst *bool) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(row.ValueJSON), dst)
	}
	readBoolPtr := func(key string) (*bool, error) {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(row.ValueJSON) == "null" || strings.TrimSpace(row.ValueJSON) == "" {
			return nil, nil // 显式写回「自动」（未设置）语义
		}
		var v bool
		if err := json.Unmarshal([]byte(row.ValueJSON), &v); err != nil {
			return nil, err
		}
		return &v, nil
	}
	if out.Enabled, err = readBoolPtr(KeyAIEnabled); err != nil {
		return out, set, err
	}
	if err := readString(KeyAIDefaultProvider, &out.DefaultProvider); err != nil {
		return out, set, err
	}
	if row, err := s.repo.GetRow(KeyAIDefaultModels); err == nil {
		_ = json.Unmarshal([]byte(row.ValueJSON), &out.DefaultModels)
	}
	if err := readFloat(KeyAITemperature, &out.Temperature); err != nil {
		return out, set, err
	}
	if err := readInt(KeyAIMaxTokens, &out.MaxTokens); err != nil {
		return out, set, err
	}
	if err := readInt(KeyAIPerUserPerMin, &out.PerUserPerMin); err != nil {
		return out, set, err
	}
	if err := readString(KeyAIRAGMode, &out.RAG.Mode); err != nil {
		return out, set, err
	}
	if err := readString(KeyAIRAGQdrantURL, &out.RAG.QdrantURL); err != nil {
		return out, set, err
	}
	if err := readString(KeyAIRAGCollectionPrefix, &out.RAG.CollectionPrefix); err != nil {
		return out, set, err
	}
	if err := readString(KeyAIRAGEmbeddingProvider, &out.RAG.EmbeddingProvider); err != nil {
		return out, set, err
	}
	if err := readString(KeyAIRAGEmbeddingModel, &out.RAG.EmbeddingModel); err != nil {
		return out, set, err
	}
	if err := readString(KeyAIRAGRerankProvider, &out.RAG.RerankProvider); err != nil {
		return out, set, err
	}
	if err := readString(KeyAIRAGRerankModel, &out.RAG.RerankModel); err != nil {
		return out, set, err
	}
	if err := readBool(KeyAIRAGVectorEnabled, &out.RAG.VectorEnabled); err != nil {
		return out, set, err
	}
	if err := readInt(KeyAIRAGTopK, &out.RAG.TopK); err != nil {
		return out, set, err
	}
	if err := readInt(KeyAIRAGChunkSize, &out.RAG.ChunkSize); err != nil {
		return out, set, err
	}
	if err := readInt(KeyAIRAGChunkOverlap, &out.RAG.ChunkOverlap); err != nil {
		return out, set, err
	}
	if err := readString(KeyAISearchProvider, &out.Search.Provider); err != nil {
		return out, set, err
	}
	if err := readString(KeyAISearchSearxngURL, &out.Search.SearxngURL); err != nil {
		return out, set, err
	}
	// tavily_api_key 同 provider api_key：入库后读取明文供运行时使用，
	// 但管理端读路径永不回显（掩码由 HTTP 层处理）。
	if err := readString(KeyAISearchTavilyKey, &out.Search.TavilyAPIKey); err != nil {
		return out, set, err
	}
	if err := readInt(KeyAISearchMaxResults, &out.Search.MaxResults); err != nil {
		return out, set, err
	}
	if out.Search.MaxResults <= 0 {
		out.Search.MaxResults = AISearchMaxResultsDefault
	}
	// ocr 为整体 JSON 块读替（同 providers 语义，不做元素级合并）：未入库
	// 跳过；损坏返回 err（由调用方回退 env 基线）；max_image_bytes 读取后
	// 统一规整（<=0 默认、>limit 钳制）。
	if row, err := s.repo.GetRow(KeyAIOCR); err == nil {
		if err := json.Unmarshal([]byte(row.ValueJSON), &out.OCR); err != nil {
			return out, set, err
		}
	} else if !errors.Is(err, ErrNotSet) {
		return out, set, err
	}
	if out.OCR.MaxImageBytes <= 0 {
		out.OCR.MaxImageBytes = AIOCRMaxBytesDefault
	} else if out.OCR.MaxImageBytes > AIOCRMaxBytesLimit {
		out.OCR.MaxImageBytes = AIOCRMaxBytesLimit
	}
	return out, set, nil
}

// SetAI 写入 AI 配置（env 基线合并：Provider 的 api_key 留空 = 保持现值；
// 默认项缺省回退现值），成功后写 settings.update 审计（api_key 打码）。
// 返回合并后的生效配置。
func (s *Store) SetAI(in AIConfig, env AIConfig, actor uuid.UUID) (AIConfig, error) {
	current, _, err := s.AIOverrides()
	if err != nil {
		return AIConfig{}, err
	}
	// 当前生效基线：env Provider（如有）+ 入库 Provider（入库为准，元素级
	// 按 ID 合并以保留「留空 = 不覆盖」的既有 api_key）。
	byID := make(map[string]AIProvider, len(current.Providers)+len(env.Providers))
	mergedProviders := make([]AIProvider, 0, len(current.Providers)+len(env.Providers))
	for _, p := range env.Providers {
		byID[p.ID] = p
		mergedProviders = append(mergedProviders, p)
	}
	for _, p := range current.Providers {
		if _, ok := byID[p.ID]; ok {
			continue
		}
		byID[p.ID] = p
		mergedProviders = append(mergedProviders, p)
	}
	// 请求载荷覆盖：ID 命中既有项时 api_key 留空继承现值；models 规整
	//（去空白、丢空 ID 条目）且非空时同步旧 Model 字段为主模型。
	next := make([]AIProvider, 0, len(in.Providers))
	for _, p := range in.Providers {
		p.ID = strings.TrimSpace(p.ID)
		p.Name = strings.TrimSpace(p.Name)
		p.BaseURL = strings.TrimSpace(p.BaseURL)
		p.Model = strings.TrimSpace(p.Model)
		if len(p.Models) > 0 {
			models := p.Models[:0:0]
			for _, m := range p.Models {
				m.ID = strings.TrimSpace(m.ID)
				m.Label = strings.TrimSpace(m.Label)
				if m.ID == "" {
					continue
				}
				models = append(models, m)
			}
			p.Models = models
			if primary := p.PrimaryModel(); primary != "" {
				p.Model = primary
			}
		}
		if p.APIKey == "" {
			if prev, ok := byID[p.ID]; ok {
				p.APIKey = prev.APIKey
			}
		}
		next = append(next, p)
	}
	merged := AIConfig{
		Enabled:         in.Enabled,
		Providers:       next,
		DefaultProvider: strings.TrimSpace(in.DefaultProvider),
		DefaultModels:   in.DefaultModels,
		Temperature:     in.Temperature,
		MaxTokens:       in.MaxTokens,
		PerUserPerMin:   in.PerUserPerMin,
		RAG:             in.RAG,
		Search:          in.Search,
		OCR:             in.OCR,
	}
	if merged.DefaultModels == nil {
		merged.DefaultModels = current.DefaultModels // 载荷未带 = 保持现值
	}
	// 清理指向已不存在的 Provider/模型的场景默认（其余能力不匹配交给
	// ValidateAI 显式报错，由管理员修正）。
	if len(merged.DefaultModels) > 0 {
		kept := make(map[string]AIModelRef, len(merged.DefaultModels))
		for scenario, ref := range merged.DefaultModels {
			if p, ok := merged.ProviderByID(ref.ProviderID); ok {
				if _, ok2 := p.ModelWithID(ref.ModelID); ok2 {
					kept[scenario] = ref
				}
			}
		}
		merged.DefaultModels = kept
	}
	if merged.RAG.Mode == "" {
		merged.RAG = current.RAG
	}
	if merged.RAG.Mode == "" {
		merged.RAG = DefaultAIConfig().RAG
	}
	// search 合并：载荷整体为零值块（旧客户端不带 search）= 保持现值；
	// 带载荷时 provider/url/max_results 直接采用（provider="" 为合法禁用值），
	// tavily_api_key 留空 = 继承现值（只写不读语义），max_results<=0 回退默认。
	if merged.Search == (AISearchConfig{}) {
		merged.Search = current.Search
	} else {
		merged.Search.Provider = strings.TrimSpace(merged.Search.Provider)
		merged.Search.SearxngURL = strings.TrimSpace(merged.Search.SearxngURL)
		if merged.Search.TavilyAPIKey == "" {
			merged.Search.TavilyAPIKey = current.Search.TavilyAPIKey
		}
		if merged.Search.MaxResults <= 0 {
			merged.Search.MaxResults = AISearchMaxResultsDefault
		}
	}
	if merged.Search.MaxResults <= 0 {
		merged.Search.MaxResults = AISearchMaxResultsDefault
	}
	// ocr 合并（同 search 整体块零值=保持现值语义）：载荷未带（零值块，
	// 旧客户端/其他入口不带 ocr）= 保持现值；带载荷 trim 规整 id/model，
	// max_image_bytes <=0 回默认、超限钳制。ValidateAI 不校验 OCR
	//（可选增强，disabled 时无意义）。
	if merged.OCR == (AIOCRConfig{}) {
		merged.OCR = current.OCR
	}
	merged.OCR.ProviderID = strings.TrimSpace(merged.OCR.ProviderID)
	merged.OCR.ModelID = strings.TrimSpace(merged.OCR.ModelID)
	if merged.OCR.MaxImageBytes <= 0 {
		merged.OCR.MaxImageBytes = AIOCRMaxBytesDefault
	} else if merged.OCR.MaxImageBytes > AIOCRMaxBytesLimit {
		merged.OCR.MaxImageBytes = AIOCRMaxBytesLimit
	}
	// personas 合并（参照 RAG 块语义）：载荷未带（nil）= 保持现值（热读取
	// 经 AIPersonas，AIOverrides 不读该键）；非 nil（含空数组 = 清空）trim
	// 规整后校验并整块写 KeyAIPersonas（独立审计见 writeAIListRow）。
	if in.Personas == nil {
		cur, err := s.AIPersonas()
		if err != nil {
			return AIConfig{}, err
		}
		merged.Personas = cur
	} else {
		normalized := make([]AIPersonaDef, 0, len(in.Personas))
		for _, p := range in.Personas {
			normalized = append(normalized, AIPersonaDef{ID: strings.TrimSpace(p.ID), Name: strings.TrimSpace(p.Name), SystemPrompt: p.SystemPrompt})
		}
		if err := ValidateAIPersonas(normalized); err != nil {
			return AIConfig{}, err
		}
		if err := s.writeAIListRow(KeyAIPersonas, normalized, actor); err != nil {
			return AIConfig{}, err
		}
		merged.Personas = normalized
	}
	if merged.Enabled == nil {
		merged.Enabled = current.Enabled // 载荷未带 = 保持现值（nil 保持自动）
	}
	if merged.DefaultProvider == "" {
		merged.DefaultProvider = current.DefaultProvider
	}
	// 回退后的默认项不在最终 Provider 集中（如整体清空）时置空——
	// resolveProvider 对空默认回退第一个启用项。
	known := make(map[string]bool, len(merged.Providers))
	for _, p := range merged.Providers {
		known[p.ID] = true
	}
	if merged.DefaultProvider != "" && !known[merged.DefaultProvider] {
		merged.DefaultProvider = ""
	}
	if merged.Temperature == 0 {
		merged.Temperature = current.Temperature
	}
	if merged.Temperature == 0 {
		merged.Temperature = AITemperatureDefault
	}
	if merged.MaxTokens == 0 {
		merged.MaxTokens = current.MaxTokens
	}
	if merged.MaxTokens == 0 {
		merged.MaxTokens = AIMaxTokensDefault
	}
	if merged.PerUserPerMin == 0 {
		merged.PerUserPerMin = current.PerUserPerMin
	}
	if merged.PerUserPerMin == 0 {
		merged.PerUserPerMin = AIPerUserPerMinDefault
	}
	if err := ValidateAI(merged); err != nil {
		return AIConfig{}, err
	}
	now := s.now().UTC()
	writes := []struct {
		key string
		val any
		typ string
	}{
		{KeyAIEnabled, merged.Enabled, TypeString},
		{KeyAIProviders, merged.Providers, TypeString},
		{KeyAIDefaultProvider, merged.DefaultProvider, TypeString},
		{KeyAIDefaultModels, merged.DefaultModels, TypeString},
		{KeyAITemperature, merged.Temperature, TypeString},
		{KeyAIMaxTokens, int64(merged.MaxTokens), TypeString},
		{KeyAIPerUserPerMin, int64(merged.PerUserPerMin), TypeString},
		{KeyAIRAGMode, merged.RAG.Mode, TypeString},
		{KeyAIRAGVectorEnabled, merged.RAG.VectorEnabled, TypeBool},
		{KeyAIRAGQdrantURL, merged.RAG.QdrantURL, TypeString},
		{KeyAIRAGCollectionPrefix, merged.RAG.CollectionPrefix, TypeString},
		{KeyAIRAGEmbeddingProvider, merged.RAG.EmbeddingProvider, TypeString},
		{KeyAIRAGEmbeddingModel, merged.RAG.EmbeddingModel, TypeString},
		{KeyAIRAGRerankProvider, merged.RAG.RerankProvider, TypeString},
		{KeyAIRAGRerankModel, merged.RAG.RerankModel, TypeString},
		{KeyAIRAGTopK, int64(merged.RAG.TopK), TypeInt},
		{KeyAIRAGChunkSize, int64(merged.RAG.ChunkSize), TypeInt},
		{KeyAIRAGChunkOverlap, int64(merged.RAG.ChunkOverlap), TypeInt},
		{KeyAISearchProvider, merged.Search.Provider, TypeString},
		{KeyAISearchSearxngURL, merged.Search.SearxngURL, TypeString},
		{KeyAISearchTavilyKey, merged.Search.TavilyAPIKey, TypeString},
		{KeyAISearchMaxResults, int64(merged.Search.MaxResults), TypeInt},
		{KeyAIOCR, merged.OCR, TypeString},
	}
	for _, w := range writes {
		raw, err := json.Marshal(w.val)
		if err != nil {
			return AIConfig{}, err
		}
		row := Setting{Key: w.key, ValueJSON: string(raw), ValueType: w.typ, Description: "AI runtime override", UpdatedBy: &actor, UpdatedAt: now}
		if err := s.repo.UpsertRow(row); err != nil {
			return AIConfig{}, err
		}
	}
	// 审计载荷：api_key 打码（已配置 = "******"）。
	auditedProviders := make([]AIProvider, len(merged.Providers))
	for i, p := range merged.Providers {
		if p.APIKey != "" {
			p.APIKey = "******"
		}
		auditedProviders[i] = p
	}
	auditedSearch := merged.Search
	if auditedSearch.TavilyAPIKey != "" {
		auditedSearch.TavilyAPIKey = "******"
	}
	metadata, _ := json.Marshal(map[string]any{"key": "ai", "value": AIConfig{
		Enabled: merged.Enabled, Providers: auditedProviders, DefaultProvider: merged.DefaultProvider,
		DefaultModels: merged.DefaultModels,
		Temperature:   merged.Temperature, MaxTokens: merged.MaxTokens, PerUserPerMin: merged.PerUserPerMin, RAG: merged.RAG,
		Search: auditedSearch,
		OCR:    merged.OCR,
	}})
	_ = s.audit.Record(audit.Entry{UserID: &actor, Action: audit.ActionSettingsUpdate, ResourceType: audit.ResourceSettings, ResourceID: "ai", Status: audit.StatusSuccess, Metadata: string(metadata)})
	return merged, nil
}

// ---------- 平台人设 / 技能模板（ai.personas / ai.skills） ----------

// AIPersonas 读取平台人设（热读取；未入库或 JSON 损坏返回空数组——容错
// 默认，与 Get 的回退语义一致，人设缺失不阻塞对话链路）。
func (s *Store) AIPersonas() ([]AIPersonaDef, error) {
	list, err := readAIList[AIPersonaDef](s, KeyAIPersonas)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// SetAIPersonas 校验并整块写入平台人设（trim 规整 id/name），成功后写
// settings.update 审计；返回归一化后的列表。
func (s *Store) SetAIPersonas(in []AIPersonaDef, actor uuid.UUID) ([]AIPersonaDef, error) {
	normalized := make([]AIPersonaDef, 0, len(in))
	for _, p := range in {
		normalized = append(normalized, AIPersonaDef{ID: strings.TrimSpace(p.ID), Name: strings.TrimSpace(p.Name), SystemPrompt: p.SystemPrompt})
	}
	if err := ValidateAIPersonas(normalized); err != nil {
		return nil, err
	}
	if err := s.writeAIListRow(KeyAIPersonas, normalized, actor); err != nil {
		return nil, err
	}
	return normalized, nil
}

// AISkills 读取平台技能模板（热读取；未入库或 JSON 损坏返回空数组）。
func (s *Store) AISkills() ([]AISkillDef, error) {
	list, err := readAIList[AISkillDef](s, KeyAISkills)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// SetAISkills 校验并整块写入平台技能（trim 规整 id/name/description），
// 成功后写 settings.update 审计；返回归一化后的列表（数组顺序即展示顺序）。
func (s *Store) SetAISkills(in []AISkillDef, actor uuid.UUID) ([]AISkillDef, error) {
	normalized := make([]AISkillDef, 0, len(in))
	for _, sk := range in {
		item := AISkillDef{ID: strings.TrimSpace(sk.ID), Name: strings.TrimSpace(sk.Name), Description: strings.TrimSpace(sk.Description), Prompt: sk.Prompt, Enabled: sk.Enabled}
		if item.Enabled == nil {
			enabled := true
			item.Enabled = &enabled
		}
		normalized = append(normalized, item)
	}
	if err := ValidateAISkills(normalized); err != nil {
		return nil, err
	}
	if err := s.writeAIListRow(KeyAISkills, normalized, actor); err != nil {
		return nil, err
	}
	return normalized, nil
}

// readAIList 读取 ai.personas / ai.skills 的 JSON 数组行；未入库（ErrNotSet）
// 返回空数组，JSON 损坏同样回退空数组（热读取容错）。
func readAIList[T any](s *Store, key string) ([]T, error) {
	row, err := s.repo.GetRow(key)
	if err != nil {
		if errors.Is(err, ErrNotSet) {
			return []T{}, nil
		}
		return nil, err
	}
	var list []T
	if err := json.Unmarshal([]byte(row.ValueJSON), &list); err != nil {
		return []T{}, nil
	}
	if list == nil {
		list = []T{}
	}
	return list, nil
}

// writeAIListRow 把归一化后的列表整块 upsert 到 system_settings 并写审计。
func (s *Store) writeAIListRow(key string, value any, actor uuid.UUID) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := s.repo.UpsertRow(Setting{Key: key, ValueJSON: string(raw), ValueType: TypeString, Description: "AI runtime override", UpdatedBy: &actor, UpdatedAt: s.now().UTC()}); err != nil {
		return err
	}
	metadata, _ := json.Marshal(map[string]any{"key": key, "value": value})
	_ = s.audit.Record(audit.Entry{UserID: &actor, Action: audit.ActionSettingsUpdate, ResourceType: audit.ResourceSettings, ResourceID: key, Status: audit.StatusSuccess, Metadata: string(metadata)})
	return nil
}

// ---------- 外部 MCP 服务器配置（ai.mcp） ----------

// AIMCPServices 读取 MCP 服务列表（热读取；未入库或 JSON 损坏返回空数组
// ——容错默认，配置缺失不阻塞对话链路的 use_mcp 降级）。
func (s *Store) AIMCPServices() ([]AIMCPServiceDef, error) {
	list, err := readAIList[AIMCPServiceDef](s, KeyAIMCPServices)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// SetAIMCPServices 校验并整块写入 MCP 服务列表（trim 规整 id/name/url/
// auth_header），成功后写 settings.update 审计（auth_header 打码 "******"，
// 绝不记录明文）。AuthHeader 留空 = 从现值按 ID 继承（密钥只写不读语义，
// 参照 providers api_key 模式——管理端回显掩码由 HTTP 层处理）。返回
// 归一化后的列表。
func (s *Store) SetAIMCPServices(in []AIMCPServiceDef, actor uuid.UUID) ([]AIMCPServiceDef, error) {
	current, err := s.AIMCPServices()
	if err != nil {
		return nil, err
	}
	prevAuth := make(map[string]string, len(current))
	for _, c := range current {
		prevAuth[c.ID] = c.AuthHeader
	}
	normalized := make([]AIMCPServiceDef, 0, len(in))
	for _, svc := range in {
		svc.ID = strings.TrimSpace(svc.ID)
		svc.Name = strings.TrimSpace(svc.Name)
		svc.URL = strings.TrimSpace(svc.URL)
		svc.AuthHeader = strings.TrimSpace(svc.AuthHeader)
		if svc.AuthHeader == "" {
			svc.AuthHeader = prevAuth[svc.ID] // 留空 = 继承现值
		}
		normalized = append(normalized, svc)
	}
	if err := ValidateAIMCPServices(normalized); err != nil {
		return nil, err
	}
	if err := s.writeAIMCRow(normalized, actor); err != nil {
		return nil, err
	}
	return normalized, nil
}

// writeAIMCRow 把 MCP 服务列表整块 upsert 并写审计（审计载荷中
// auth_header 打码，区别于 writeAIListRow 的明文——密钥不入审计日志）。
func (s *Store) writeAIMCRow(list []AIMCPServiceDef, actor uuid.UUID) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := s.repo.UpsertRow(Setting{Key: KeyAIMCPServices, ValueJSON: string(raw), ValueType: TypeString, Description: "AI runtime override", UpdatedBy: &actor, UpdatedAt: s.now().UTC()}); err != nil {
		return err
	}
	audited := make([]AIMCPServiceDef, len(list))
	for i, svc := range list {
		if svc.AuthHeader != "" {
			svc.AuthHeader = "******"
		}
		audited[i] = svc
	}
	metadata, _ := json.Marshal(map[string]any{"key": KeyAIMCPServices, "value": audited})
	_ = s.audit.Record(audit.Entry{UserID: &actor, Action: audit.ActionSettingsUpdate, ResourceType: audit.ResourceSettings, ResourceID: KeyAIMCPServices, Status: audit.StatusSuccess, Metadata: string(metadata)})
	return nil
}
