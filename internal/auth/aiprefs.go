// aiprefs.go：用户个人 AI 配置（user_ai_prefs，migration 049，双轨制：
// 个人自备 Provider + 平台 Provider）。
//
// prefs 为整块 JSON（providers/default_models/personas/prefer_personal），
// 结构校验与密钥掩码在本文件完成；存储接口挂在 UserStore（模式同
// openwith.go 的 user_open_with）。api_key 语义同平台 Provider 与 SMTP
// 密码：入库明文供运行时直连使用，但只在 PUT 请求携带（空 = 保持现值，
// 合并在 HTTP 层完成），任何读路径均以掩码视图回显（api_key_configured），
// 绝不返回明文。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/settings"
)

// 个人 Provider 类型（AIPersonalProvider.Kind 取值）：与平台一致的
// openai_compatible / anthropic 两种真实协议；不支持 mock（个人池无演示
// Provider 语义，避免绕过真实链路校验）。
const (
	AIPersonalKindOpenAICompatible = settings.AIKindOpenAICompatible
	AIPersonalKindAnthropic        = settings.AIKindAnthropic
)

// 个人结构上限（校验边界）。
const (
	AIPersonalMaxProviders   = 8
	AIPersonalMaxModels      = 20
	AIPersonalMaxPersonas    = 20
	AIPersonalMaxPromptRunes = 4000
	// AIPersonalMaxSkills 个人技能/快捷指令模板条数上限（与平台 ai.skills 一致）。
	AIPersonalMaxSkills = 50
	// AIPersonalMaxSkillDescRunes 个人技能 description 长度上限。
	AIPersonalMaxSkillDescRunes = 200
)

// ErrInvalidAIPrefs 表示个人 AI 配置非法（HTTP 400 语义；错误消息含具体
// 字段定位，供前端逐项提示）。
var ErrInvalidAIPrefs = errors.New("invalid personal ai settings")

// AIPersonalModel 为个人 Provider 下的一个模型条目（capabilities 与平台
// 同构，含 reasoning）。
type AIPersonalModel struct {
	ID           string                       `json:"id"`
	Label        string                       `json:"label,omitempty"`
	Capabilities settings.AIModelCapabilities `json:"capabilities"`
}

// AIPersonalProvider 为用户自备的 AI 服务提供方条目。APIKey 仅在 PUT
// 请求中携带（空 = 保持现值不覆盖）；读路径永不回显明文（掩码视图仅报
// api_key_configured）。
type AIPersonalProvider struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Kind    string            `json:"kind"`
	BaseURL string            `json:"base_url"`
	APIKey  string            `json:"api_key,omitempty"`
	Models  []AIPersonalModel `json:"models"`
}

// AIPersonalModelRef 为个人场景默认模型引用（default_models 的值；
// provider_id/model_id 须指向个人池）。
type AIPersonalModelRef struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

// AIPersonalPersona 为用户自定义 AI 人设（system 提示模板）。
type AIPersonalPersona struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SystemPrompt string `json:"system_prompt"`
}

// AIPersonalSkill 为用户自定义技能/快捷指令模板：prompt 支持占位符
// {selection}（编辑器选区）/ {file}（当前文件名），由前端在填入输入框时
// 替换（语义同平台 ai.skills）。
type AIPersonalSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Prompt      string `json:"prompt"`
}

// AIPersonalPrefs 为用户个人 AI 配置全集（user_ai_prefs.prefs 的 JSON 结构）。
// PreferPersonal=true 时未显式指定模型的对话/摘要请求优先使用个人池的
// 场景默认模型（个人 > 平台；显式指定 model 时个人池始终先于平台池解析）。
type AIPersonalPrefs struct {
	Providers      []AIPersonalProvider          `json:"providers"`
	DefaultModels  map[string]AIPersonalModelRef `json:"default_models,omitempty"`
	Personas       []AIPersonalPersona           `json:"personas,omitempty"`
	Skills         []AIPersonalSkill             `json:"skills,omitempty"`
	PreferPersonal bool                          `json:"prefer_personal"`
	// MemoryAuto 开启「AI 记忆自动提取」（默认零值 false=关闭）：每轮对话
	// 完成后由服务端后台提取长期偏好写入 ai_memory（kind=auto）。布尔非
	// 密钥，掩码视图原样回显；存量 JSON 无此字段反序列化为 false，无需
	// 迁移（校验无新增——布尔无非法值）。
	MemoryAuto bool `json:"memory_auto,omitempty"`
}

// ---------- 校验 ----------

// validAIPrefsID 校验标识符（provider/model/persona 的 id 与 name 共用）：
// 去空白后 1..max 字符，字符集 [A-Za-z0-9_\-.：]（支持中文名）。
func validAIPrefsID(s string, max int) bool {
	s = strings.TrimSpace(s)
	if s == "" || len([]rune(s)) > max {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == ':' || r == ' ':
		case r >= 0x4e00 && r <= 0x9fff: // CJK（名称）
		default:
			return false
		}
	}
	return true
}

// ValidateAIPersonalPrefs 校验个人 AI 配置整体：providers ≤8（id 唯一、
// kind 仅 openai_compatible/anthropic、base_url 绝对 http(s)、models ≤20
// 且 id 唯一）、default_models 键限 chat/summary/edit/embedding 且引用须
// 指向个人池具备对应能力的模型、personas ≤20（system_prompt ≤4000 字符）。
// 非法返回包裹 ErrInvalidAIPrefs 的定位错误。
func ValidateAIPersonalPrefs(p AIPersonalPrefs) error {
	if len(p.Providers) > AIPersonalMaxProviders {
		return fmt.Errorf("%w: providers 最多 %d 个", ErrInvalidAIPrefs, AIPersonalMaxProviders)
	}
	seenProvider := make(map[string]bool, len(p.Providers))
	for i, prov := range p.Providers {
		field := fmt.Sprintf("providers[%d]", i)
		if !validAIPrefsID(prov.ID, 64) {
			return fmt.Errorf("%w: %s.id 须为 1..64 位字母/数字/_-.: 字符", ErrInvalidAIPrefs, field)
		}
		if seenProvider[prov.ID] {
			return fmt.Errorf("%w: %s.id %q 重复", ErrInvalidAIPrefs, field, prov.ID)
		}
		seenProvider[prov.ID] = true
		if !validAIPrefsID(prov.Name, 100) {
			return fmt.Errorf("%w: %s.name 须为 1..100 个字符", ErrInvalidAIPrefs, field)
		}
		switch prov.Kind {
		case AIPersonalKindOpenAICompatible, AIPersonalKindAnthropic:
		default:
			return fmt.Errorf("%w: %s.kind 须为 openai_compatible 或 anthropic", ErrInvalidAIPrefs, field)
		}
		if u, err := url.Parse(strings.TrimSpace(prov.BaseURL)); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%w: %s.base_url 须为绝对 http(s) URL", ErrInvalidAIPrefs, field)
		}
		if len([]rune(prov.APIKey)) > 512 {
			return fmt.Errorf("%w: %s.api_key 过长（≤512 字符）", ErrInvalidAIPrefs, field)
		}
		if len(prov.Models) > AIPersonalMaxModels {
			return fmt.Errorf("%w: %s.models 最多 %d 个", ErrInvalidAIPrefs, field, AIPersonalMaxModels)
		}
		if len(prov.Models) == 0 {
			return fmt.Errorf("%w: %s.models 至少配置 1 个模型", ErrInvalidAIPrefs, field)
		}
		seenModel := make(map[string]bool, len(prov.Models))
		for j, m := range prov.Models {
			mfield := fmt.Sprintf("%s.models[%d]", field, j)
			if !validAIPrefsID(m.ID, 128) {
				return fmt.Errorf("%w: %s.id 须为 1..128 位字母/数字/_-.: 字符", ErrInvalidAIPrefs, mfield)
			}
			if seenModel[m.ID] {
				return fmt.Errorf("%w: %s.id %q 重复", ErrInvalidAIPrefs, mfield, m.ID)
			}
			seenModel[m.ID] = true
			if len([]rune(m.Label)) > 100 {
				return fmt.Errorf("%w: %s.label 过长（≤100 字符）", ErrInvalidAIPrefs, mfield)
			}
		}
	}
	// default_models：键白名单 + 引用存在性与能力匹配（对话类 → chat，
	// embedding → embedding；引用的是个人池）。
	for scenario, ref := range p.DefaultModels {
		switch scenario {
		case settings.AIScenarioChat, settings.AIScenarioSummary, settings.AIScenarioEdit, settings.AIScenarioEmbedding:
		default:
			return fmt.Errorf("%w: default_models 含未知场景 %q（仅 chat/summary/edit/embedding）", ErrInvalidAIPrefs, scenario)
		}
		prov, ok := p.PersonalProviderByID(ref.ProviderID)
		if !ok {
			return fmt.Errorf("%w: default_models.%s.provider_id %q 不在个人 Provider 中", ErrInvalidAIPrefs, scenario, ref.ProviderID)
		}
		m, ok := prov.PersonalModelWithID(ref.ModelID)
		if !ok {
			return fmt.Errorf("%w: default_models.%s.model_id %q 不在 Provider %q 中", ErrInvalidAIPrefs, scenario, ref.ModelID, prov.ID)
		}
		if scenario == settings.AIScenarioEmbedding && !m.Capabilities.Embedding {
			return fmt.Errorf("%w: default_models.%s 模型 %q 缺少 embedding 能力", ErrInvalidAIPrefs, scenario, ref.ModelID)
		}
		if scenario != settings.AIScenarioEmbedding && !m.Capabilities.Chat {
			return fmt.Errorf("%w: default_models.%s 模型 %q 缺少 chat 能力", ErrInvalidAIPrefs, scenario, ref.ModelID)
		}
	}
	if len(p.Personas) > AIPersonalMaxPersonas {
		return fmt.Errorf("%w: personas 最多 %d 个", ErrInvalidAIPrefs, AIPersonalMaxPersonas)
	}
	seenPersona := make(map[string]bool, len(p.Personas))
	for i, persona := range p.Personas {
		field := fmt.Sprintf("personas[%d]", i)
		if !validAIPrefsID(persona.ID, 64) {
			return fmt.Errorf("%w: %s.id 须为 1..64 位字母/数字/_-.: 字符", ErrInvalidAIPrefs, field)
		}
		if seenPersona[persona.ID] {
			return fmt.Errorf("%w: %s.id %q 重复", ErrInvalidAIPrefs, field, persona.ID)
		}
		seenPersona[persona.ID] = true
		if !validAIPrefsID(persona.Name, 100) {
			return fmt.Errorf("%w: %s.name 须为 1..100 个字符", ErrInvalidAIPrefs, field)
		}
		if len([]rune(persona.SystemPrompt)) > AIPersonalMaxPromptRunes {
			return fmt.Errorf("%w: %s.system_prompt 过长（≤%d 字符）", ErrInvalidAIPrefs, field, AIPersonalMaxPromptRunes)
		}
	}
	// skills：≤50（id 唯一、name/description/prompt 长度边界；prompt 支持
	// {selection}/{file} 占位符，由前端替换，此处不做内容校验）。
	if len(p.Skills) > AIPersonalMaxSkills {
		return fmt.Errorf("%w: skills 最多 %d 个", ErrInvalidAIPrefs, AIPersonalMaxSkills)
	}
	seenSkill := make(map[string]bool, len(p.Skills))
	for i, skill := range p.Skills {
		field := fmt.Sprintf("skills[%d]", i)
		if !validAIPrefsID(skill.ID, 64) {
			return fmt.Errorf("%w: %s.id 须为 1..64 位字母/数字/_-.: 字符", ErrInvalidAIPrefs, field)
		}
		if seenSkill[skill.ID] {
			return fmt.Errorf("%w: %s.id %q 重复", ErrInvalidAIPrefs, field, skill.ID)
		}
		seenSkill[skill.ID] = true
		if !validAIPrefsID(skill.Name, 100) {
			return fmt.Errorf("%w: %s.name 须为 1..100 个字符", ErrInvalidAIPrefs, field)
		}
		if len([]rune(skill.Description)) > AIPersonalMaxSkillDescRunes {
			return fmt.Errorf("%w: %s.description 过长（≤%d 字符）", ErrInvalidAIPrefs, field, AIPersonalMaxSkillDescRunes)
		}
		if len([]rune(skill.Prompt)) > AIPersonalMaxPromptRunes {
			return fmt.Errorf("%w: %s.prompt 过长（≤%d 字符）", ErrInvalidAIPrefs, field, AIPersonalMaxPromptRunes)
		}
	}
	return nil
}

// PersonalProviderByID 按 ID 查找个人 Provider。
func (p AIPersonalPrefs) PersonalProviderByID(id string) (AIPersonalProvider, bool) {
	id = strings.TrimSpace(id)
	for _, prov := range p.Providers {
		if prov.ID == id {
			return prov, true
		}
	}
	return AIPersonalProvider{}, false
}

// PersonalModelWithID 从 Provider 的模型列表中查找模型。
func (p AIPersonalProvider) PersonalModelWithID(id string) (AIPersonalModel, bool) {
	id = strings.TrimSpace(id)
	for _, m := range p.Models {
		if m.ID == id {
			return m, true
		}
	}
	return AIPersonalModel{}, false
}

// ---------- 掩码视图（读路径绝不回显 api_key） ----------

// AIPersonalProviderView 为个人 Provider 的掩码视图：api_key 以
// api_key_configured 布尔回显，其余字段深拷贝。
type AIPersonalProviderView struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Kind             string            `json:"kind"`
	BaseURL          string            `json:"base_url"`
	APIKeyConfigured bool              `json:"api_key_configured"`
	Models           []AIPersonalModel `json:"models"`
}

// AIPersonalPrefsView 为 GET /ai/personal-settings 的响应载荷（掩码视图）。
type AIPersonalPrefsView struct {
	Providers      []AIPersonalProviderView      `json:"providers"`
	DefaultModels  map[string]AIPersonalModelRef `json:"default_models,omitempty"`
	Personas       []AIPersonalPersona           `json:"personas,omitempty"`
	Skills         []AIPersonalSkill             `json:"skills,omitempty"`
	PreferPersonal bool                          `json:"prefer_personal"`
	// MemoryAuto 原样回显（非密钥，不受掩码逻辑触碰）。
	MemoryAuto bool `json:"memory_auto,omitempty"`
}

// Masked 返回深拷贝掩码视图：Provider 的 api_key 字段剔除，仅保留
// api_key_configured 布尔；其余字段（models/default_models/personas）逐层
// 拷贝，调用方可安全序列化。
func (p AIPersonalPrefs) Masked() AIPersonalPrefsView {
	out := AIPersonalPrefsView{PreferPersonal: p.PreferPersonal, MemoryAuto: p.MemoryAuto}
	out.Providers = make([]AIPersonalProviderView, 0, len(p.Providers))
	for _, prov := range p.Providers {
		view := AIPersonalProviderView{
			ID: prov.ID, Name: prov.Name, Kind: prov.Kind, BaseURL: prov.BaseURL,
			APIKeyConfigured: prov.APIKey != "",
		}
		view.Models = make([]AIPersonalModel, len(prov.Models))
		copy(view.Models, prov.Models)
		out.Providers = append(out.Providers, view)
	}
	if p.DefaultModels != nil {
		out.DefaultModels = make(map[string]AIPersonalModelRef, len(p.DefaultModels))
		for k, v := range p.DefaultModels {
			out.DefaultModels[k] = v
		}
	}
	out.Personas = append(out.Personas, p.Personas...)
	out.Skills = append(out.Skills, p.Skills...)
	return out
}

// ---------- 存储（UserStore，模式同 user_open_with） ----------

// GetAIPrefs 读取用户的个人 AI 配置原始 JSON；无记录返回 (nil, nil)（由
// 调用方归一为空配置）。解析失败（库中 JSON 损坏）返回错误。
func (s *UserStore) GetAIPrefs(userID uuid.UUID) (json.RawMessage, error) {
	var raw []byte
	err := s.db.Raw("SELECT prefs FROM user_ai_prefs WHERE user_id = ?", userID).Scan(&raw).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("user_ai_prefs: invalid json for user %s", userID)
	}
	return json.RawMessage(raw), nil
}

// SetAIPrefs 整块写入（UPSERT，更新 updated_at）；prefs 须先经
// ValidateAIPersonalPrefs 校验并完成 api_key 留空继承合并（HTTP 层）。
func (s *UserStore) SetAIPrefs(userID uuid.UUID, prefs json.RawMessage) error {
	now := time.Now().UTC()
	return s.db.Exec(`
		INSERT INTO user_ai_prefs (user_id, prefs, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (user_id)
		DO UPDATE SET prefs = EXCLUDED.prefs, updated_at = EXCLUDED.updated_at`,
		userID, []byte(prefs), now).Error
}
