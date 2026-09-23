// 设置页「AI 个人配置」面板（双轨制：个人自备 Provider + 平台 Provider，
// 不依赖 admin）：「优先使用我的模型」开关 / 个人 Provider 卡片列表（新建、
// 编辑、删除弹窗：名称/类型 OpenAI 兼容或 Anthropic/base_url/api_key 留空
// 保持/多模型+能力勾选——交互风格对齐 AISettingsPanel 但简化）/ 个人场景
// 默认模型（chat/summary/edit，只列个人池模型，留空则用平台默认）/ 人设
// 管理（名称 + system 提示 CRUD）。保存 = 整块 PUT（掩码回读，api_key
// 任何读路径不回显）。加载失败与未配置状态清晰提示。
import { useEffect, useState } from 'react'
import { Button, Input, Select, Switch } from 'antd'
import { Plus } from 'lucide-react'
import {
  AIPersonalModelRef,
  AIPersonalPrefsView,
  AIPersonalProviderInput,
  getAIPersonalSettings,
  putAIPersonalSettings,
} from '../../api'
import type { AIModelCapabilities } from '../../api'
import { Modal } from '../FileBrowser'

/** 个人 Provider 类型选项（无 mock：个人池无演示 Provider 语义）。 */
const KIND_OPTIONS = [
  { value: 'openai_compatible', label: 'OpenAI 兼容（OpenAI/DeepSeek/Qwen/Ollama/vLLM）' },
  { value: 'anthropic', label: 'Anthropic（Claude）' },
]

/** 类型 → base_url 预设（选中即回填建议值）。 */
const KIND_BASE_PRESET: Record<string, string> = {
  openai_compatible: 'https://api.openai.com/v1',
  anthropic: 'https://www.anthropic.com',
}

/** 能力勾选项（与平台同构，含 reasoning）。 */
const CAPABILITY_FIELDS: { key: keyof AIModelCapabilities; label: string }[] = [
  { key: 'chat', label: '对话' },
  { key: 'embedding', label: '向量' },
  { key: 'vision', label: '视觉图片' },
  { key: 'rerank', label: '重排序' },
  { key: 'reasoning', label: '推理思考' },
]

const emptyCaps = (): AIModelCapabilities => ({ chat: false, embedding: false, vision: false, rerank: false, reasoning: false })

/** 编辑中的模型行。 */
interface ModelForm {
  id: string
  label: string
  caps: AIModelCapabilities
}

/** 编辑中的 Provider 表单态（api_key 草稿：空 = 保持现值）。 */
interface ProviderForm {
  id: string
  name: string
  kind: 'openai_compatible' | 'anthropic'
  base_url: string
  api_key: string
  models: ModelForm[]
  /** key_configured 仅展示用（掩码视图回读）。 */
  key_configured: boolean
}

/** 编辑中的人设表单态。 */
interface PersonaForm {
  id: string
  name: string
  system_prompt: string
}

/** 生成不与现有集合冲突的短 id（provider/persona 共用）。 */
function freshID(prefix: string, existing: Set<string>): string {
  for (let i = 1; i < 1000; i++) {
    const id = `${prefix}${i}`
    if (!existing.has(id)) return id
  }
  return `${prefix}${Date.now().toString(36)}`
}

/** 场景默认模型下拉的场景定义（仅个人池参与；留空 = 平台默认）。 */
const SCENARIOS: { key: 'chat' | 'summary' | 'edit'; label: string; hint: string }[] = [
  { key: 'chat', label: '对话默认', hint: '通用对话与「问 AI」；留空则用平台默认' },
  { key: 'summary', label: '摘要默认', hint: '文件 AI 摘要；留空则用平台默认（无效时回落对话）' },
  { key: 'edit', label: '编辑器默认', hint: '编辑器 AI（润色/续写等）；留空则用平台默认' },
]

/** 模型下拉选项值编码：provider_id::model_id。 */
const refValue = (ref: AIPersonalModelRef) => `${ref.provider_id}::${ref.model_id}`

export default function AIPersonalPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [saving, setSaving] = useState(false)
  // 本地草稿（保存时整块 PUT；保存成功以掩码响应重建）。
  const [providers, setProviders] = useState<ProviderForm[]>([])
  const [defaultModels, setDefaultModels] = useState<Partial<Record<'chat' | 'summary' | 'edit', AIPersonalModelRef>>>({})
  const [personas, setPersonas] = useState<PersonaForm[]>([])
  const [preferPersonal, setPreferPersonal] = useState(false)
  // 自动记忆（prefs.memory_auto；缺省 false=关）：AI 自动从对话中提取长期偏好。
  const [memoryAuto, setMemoryAuto] = useState(false)
  // Provider 编辑弹窗。
  const [editing, setEditing] = useState<ProviderForm | null>(null)
  // 人设编辑弹窗。
  const [personaEditing, setPersonaEditing] = useState<PersonaForm | null>(null)

  const rebuild = (v: AIPersonalPrefsView) => {
    setProviders((v.providers ?? []).map((p) => ({
      id: p.id, name: p.name, kind: p.kind, base_url: p.base_url, api_key: '',
      models: (p.models ?? []).map((m) => ({ id: m.id, label: m.label ?? '', caps: { ...emptyCaps(), ...(m.capabilities ?? emptyCaps()) } })),
      key_configured: p.api_key_configured,
    })))
    setDefaultModels({
      chat: v.default_models?.chat,
      summary: v.default_models?.summary,
      edit: v.default_models?.edit,
    })
    setPersonas((v.personas ?? []).map((p) => ({ ...p })))
    setPreferPersonal(Boolean(v.prefer_personal))
    setMemoryAuto(Boolean(v.memory_auto))
  }

  const load = async () => {
    setLoading(true)
    setLoadError('')
    try {
      rebuild(await getAIPersonalSettings())
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : '个人 AI 配置加载失败')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  /** 保存（整块 PUT）：api_key 留空 = 继承现值；引用已删模型/Provider 的
   * 场景默认联动清理；成功后以掩码响应重建（api_key 草稿清空）。 */
  const save = async () => {
    if (saving) return
    // 客户端预校验（后端为权威校验，消息更细）。
    const ids = new Set(providers.map((p) => p.id))
    if (ids.size !== providers.length) {
      onError('个人 Provider 的 ID 重复，请修改后重试')
      return
    }
    for (const p of providers) {
      if (!p.id.trim() || !p.name.trim() || !p.base_url.trim() || p.models.filter((m) => m.id.trim()).length === 0) {
        onError(`Provider「${p.name || p.id}」须填写 ID/名称/base_url 且至少一个模型`)
        return
      }
    }
    setSaving(true)
    onError('')
    try {
      const payloadProviders: AIPersonalProviderInput[] = providers.map((p) => ({
        id: p.id.trim(), name: p.name.trim(), kind: p.kind, base_url: p.base_url.trim(),
        api_key: p.api_key === '' ? undefined : p.api_key,
        models: p.models.filter((m) => m.id.trim()).map((m) => ({ id: m.id.trim(), label: m.label.trim() || undefined, capabilities: { ...m.caps } })),
      }))
      // 清理指向已删 Provider/模型的场景默认。
      const known = new Set(payloadProviders.flatMap((p) => p.models.map((m) => `${p.id}::${m.id}`)))
      const dm: Record<string, AIPersonalModelRef> = {}
      for (const key of ['chat', 'summary', 'edit'] as const) {
        const ref = defaultModels[key]
        if (ref && known.has(refValue(ref))) dm[key] = ref
      }
      const saved = await putAIPersonalSettings({
        providers: payloadProviders,
        default_models: dm,
        personas: personas.map((p) => ({ id: p.id.trim(), name: p.name.trim(), system_prompt: p.system_prompt })),
        prefer_personal: preferPersonal,
        memory_auto: memoryAuto,
      })
      rebuild(saved)
      onNotice('AI 个人配置已保存（即时生效）')
    } catch (err) {
      onError(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  /** 打开新建 Provider 弹窗。 */
  const newProvider = () => {
    setEditing({
      id: freshID('my-', new Set(providers.map((p) => p.id))),
      name: '', kind: 'openai_compatible', base_url: KIND_BASE_PRESET.openai_compatible,
      api_key: '', models: [{ id: '', label: '', caps: { ...emptyCaps(), chat: true } }], key_configured: false,
    })
  }

  /** 打开编辑 Provider 弹窗（api_key 草稿清空 = 留空保持现值）。 */
  const editProvider = (p: ProviderForm) => setEditing({ ...p, models: p.models.map((m) => ({ ...m, caps: { ...m.caps } })), api_key: '' })

  /** 弹窗确认：写回列表（新增或按原 id 替换）。 */
  const commitProvider = () => {
    if (!editing) return
    setProviders((prev) => {
      const idx = prev.findIndex((p) => p.id === editing.id)
      const next = { ...editing, name: editing.name.trim(), base_url: editing.base_url.trim(), id: editing.id.trim(), models: editing.models.filter((m) => m.id.trim()) }
      if (idx >= 0) { const copy = [...prev]; copy[idx] = next; return copy }
      return [...prev, next]
    })
    setEditing(null)
  }

  const removeProvider = (id: string) => {
    setProviders((prev) => prev.filter((p) => p.id !== id))
    setDefaultModels((prev) => {
      const next = { ...prev }
      for (const key of ['chat', 'summary', 'edit'] as const) {
        if (next[key]?.provider_id === id) delete next[key]
      }
      return next
    })
  }

  const commitPersona = () => {
    if (!personaEditing) return
    setPersonas((prev) => {
      const idx = prev.findIndex((p) => p.id === personaEditing.id)
      const next = { ...personaEditing, id: personaEditing.id.trim(), name: personaEditing.name.trim() }
      if (idx >= 0) { const copy = [...prev]; copy[idx] = next; return copy }
      return [...prev, next]
    })
    setPersonaEditing(null)
  }

  /** 个人池模型下拉选项（按 Provider 分组；仅 chat 能力可选为场景默认）。 */
  const modelOptions = () => {
    const opts: { value: string; label: string }[] = [{ value: '', label: '（留空：用平台默认）' }]
    for (const p of providers) {
      for (const m of p.models) {
        if (m.id.trim() && m.caps.chat) opts.push({ value: `${p.id}::${m.id}`, label: `${p.name || p.id} / ${m.label || m.id}` })
      }
    }
    return opts
  }

  if (loading) {
    return (
      <div className="panel setting-group">
        <h3>AI 个人配置</h3>
        <div className="hint">加载中…</div>
      </div>
    )
  }

  if (loadError) {
    return (
      <div className="panel setting-group">
        <h3>AI 个人配置</h3>
        <div className="banner error">{loadError}</div>
        <div style={{ marginTop: 12 }}>
          <Button onClick={() => void load()}>重试</Button>
        </div>
      </div>
    )
  }

  return (
    <div className="panel setting-group">
      <h3>AI 个人配置</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        双轨制：除平台统一配置的模型外，可自备 AI 服务（OpenAI 兼容网关 / Anthropic）。
        个人模型仅本人可用，对话与摘要经你的 Key 直连上游（额度自担）；密钥只写不读，
        保存后任何路径均不回显。
      </div>

      {/* prefer_personal 开关：未显式选择模型时个人默认 > 平台默认。 */}
      <div className="setting-row" style={{ borderBottom: 'none', paddingBottom: 0 }}>
        <div className="setting-main">
          <div className="setting-key">优先使用我的模型</div>
          <div className="setting-desc muted">
            开启后，未显式选择模型的对话 / 摘要优先用下方「个人场景默认模型」（个人 &gt; 平台）；
            关闭或个人默认缺失时回落平台默认。显式选择的模型不受此开关影响（个人池始终先于平台池解析）。
          </div>
        </div>
        <div className="setting-control">
          <span className="setting-bool">
            <Switch size="small" checked={preferPersonal} onChange={(v) => setPreferPersonal(v)} />
            <span>{preferPersonal ? '个人优先' : '平台默认'}</span>
          </span>
        </div>
      </div>

      {/* 自动记忆开关：AI 自动从对话中提取长期偏好（prefs.memory_auto）。 */}
      <div className="setting-row" style={{ borderBottom: 'none', paddingBottom: 0 }}>
        <div className="setting-main">
          <div className="setting-key">自动记忆</div>
          <div className="setting-desc muted">
            AI 自动从对话中提取长期偏好：开启后对话产生的偏好要点会以「自动」记忆保存（在 AI 助手的记忆面板查看），
            与手动记忆一同注入后续对话；关闭后仅保留手动维护的记忆。
          </div>
        </div>
        <div className="setting-control">
          <span className="setting-bool">
            <Switch size="small" checked={memoryAuto} onChange={(v) => setMemoryAuto(v)} />
            <span>{memoryAuto ? '已开启' : '已关闭'}</span>
          </span>
        </div>
      </div>

      {/* 个人 Provider 卡片列表。 */}
      <div className="setting-key" style={{ marginTop: 16 }}>个人 Provider（{providers.length}/8）</div>
      {providers.length === 0 ? (
        <div className="empty">尚未配置个人 Provider——添加后可在对话模型选择器中看到带「（个人）」标注的模型。</div>
      ) : (
        providers.map((p) => (
          <div key={p.id} className="setting-row ai-personal-provider">
            <div className="setting-main">
              <div className="setting-key">
                {p.name || p.id}
                <span className="badge" style={{ marginLeft: 8 }}>{p.kind === 'anthropic' ? 'Anthropic' : 'OpenAI 兼容'}</span>
                <span className={`badge ${p.key_configured ? 'available' : 'failed'}`} style={{ marginLeft: 4 }}>
                  {p.key_configured ? '密钥已配置' : '未配置密钥'}
                </span>
              </div>
              <div className="setting-meta muted" title={p.base_url}>{p.id} · {p.base_url}</div>
              <div className="event-badges">
                {p.models.map((m) => (
                  <span key={m.id} className="badge" title={CAPABILITY_FIELDS.filter((c) => m.caps[c.key]).map((c) => c.label).join(' / ') || '无能力勾选'}>
                    {m.label || m.id}
                    {m.caps.reasoning ? ' · 推理' : ''}
                  </span>
                ))}
              </div>
            </div>
            <div className="setting-control">
              <Button size="small" onClick={() => editProvider(p)}>编辑</Button>
              <Button size="small" danger onClick={() => removeProvider(p.id)}>删除</Button>
            </div>
          </div>
        ))
      )}
      <div style={{ marginTop: 8 }}>
        <Button type="primary" disabled={providers.length >= 8} onClick={newProvider}>
          <Plus size={14} style={{ marginRight: 4, verticalAlign: -2 }} />添加 Provider
        </Button>
      </div>

      {/* 个人场景默认模型（chat/summary/edit；留空 = 平台默认）。 */}
      <div className="setting-key" style={{ marginTop: 16 }}>个人场景默认模型</div>
      <div className="setting-desc muted" style={{ marginBottom: 8 }}>
        只列个人池中具备对话能力的模型；「优先使用我的模型」开启时生效（embedding 场景经平台 RAG 配置，不在此选择）。
      </div>
      <div className="smtp-grid">
        {SCENARIOS.map((s) => (
          <label key={s.key} className="field" title={s.hint}>
            <span>{s.label}（留空则用平台默认）</span>
            <Select
              value={defaultModels[s.key] ? refValue(defaultModels[s.key]!) : ''}
              onChange={(v) => {
                setDefaultModels((prev) => {
                  const next = { ...prev }
                  if (!v) delete next[s.key]
                  else {
                    const [provider_id, model_id] = v.split('::')
                    next[s.key] = { provider_id, model_id }
                  }
                  return next
                })
              }}
              options={modelOptions()}
              style={{ width: '100%' }}
            />
          </label>
        ))}
      </div>

      {/* 人设管理（名称 + system 提示）。 */}
      <div className="setting-key" style={{ marginTop: 16 }}>AI 人设（{personas.length}/20）</div>
      <div className="setting-desc muted" style={{ marginBottom: 8 }}>
        自定义 system 提示模板（≤4000 字符），供对话入口按需选用；仅保存在你的个人配置中。
      </div>
      {personas.length === 0 ? (
        <div className="empty">尚未创建人设。</div>
      ) : (
        personas.map((p) => (
          <div key={p.id} className="setting-row" >
            <div className="setting-main">
              <div className="setting-key">{p.name || p.id}</div>
              <div className="setting-desc muted" title={p.system_prompt}>
                {p.system_prompt.length > 80 ? `${p.system_prompt.slice(0, 80)}…` : p.system_prompt || '（空提示）'}
              </div>
            </div>
            <div className="setting-control">
              <Button size="small" onClick={() => setPersonaEditing({ ...p })}>编辑</Button>
              <Button size="small" danger onClick={() => setPersonas((prev) => prev.filter((x) => x.id !== p.id))}>删除</Button>
            </div>
          </div>
        ))
      )}
      <div style={{ marginTop: 8 }}>
        <Button disabled={personas.length >= 20} onClick={() => setPersonaEditing({ id: freshID('persona-', new Set(personas.map((p) => p.id))), name: '', system_prompt: '' })}>
          <Plus size={14} style={{ marginRight: 4, verticalAlign: -2 }} />添加人设
        </Button>
      </div>

      {/* 保存：整块 PUT（掩码回读）。 */}
      <div className="modal-actions" style={{ marginTop: 16 }}>
        <Button disabled={saving} onClick={() => void load()}>重置</Button>
        <Button type="primary" loading={saving} onClick={() => void save()}>
          {saving ? '保存中…' : '保存（即时生效）'}
        </Button>
      </div>

      {/* Provider 编辑弹窗（简化版：类型/base_url/api_key 留空保持/多模型+能力勾选）。 */}
      {editing && (
        <Modal title={providers.some((p) => p.id === editing.id) ? `编辑 Provider：${editing.name || editing.id}` : '添加个人 Provider'} onClose={() => setEditing(null)}>
          <form
            className="team-create-row ai-personal-form"
            onSubmit={(e) => { e.preventDefault(); commitProvider() }}
          >
            <label className="field">
              <span>名称（1..100 字符）</span>
              <Input autoFocus required maxLength={100} value={editing.name} onChange={(e) => setEditing({ ...editing, name: e.target.value })} placeholder="如：我的 DeepSeek" />
            </label>
            <label className="field">
              <span>ID（1..64 位字母/数字/_-.:，保存后不可与他人混淆）</span>
              <Input required maxLength={64} value={editing.id} onChange={(e) => setEditing({ ...editing, id: e.target.value })} placeholder="如 my-deepseek" />
            </label>
            <label className="field">
              <span>类型</span>
              <Select
                value={editing.kind}
                onChange={(v) => setEditing({ ...editing, kind: v as ProviderForm['kind'], base_url: editing.base_url.trim() === '' || Object.values(KIND_BASE_PRESET).includes(editing.base_url) ? KIND_BASE_PRESET[v] : editing.base_url })}
                options={KIND_OPTIONS}
              />
            </label>
            <label className="field">
              <span>Base URL（绝对 http/https）</span>
              <Input required value={editing.base_url} onChange={(e) => setEditing({ ...editing, base_url: e.target.value })} placeholder={KIND_BASE_PRESET[editing.kind]} />
            </label>
            <label className="field">
              <span>API Key（{editing.key_configured ? '已配置，留空保持不变' : '未配置'}）</span>
              <Input.Password
                value={editing.api_key}
                onChange={(e) => setEditing({ ...editing, api_key: e.target.value })}
                placeholder={editing.key_configured ? '留空保持现值' : '如 sk-…'}
                autoComplete="new-password"
              />
            </label>
            <div className="field">
              <span>模型（≤20 个；勾选能力，对话场景须勾「对话」）</span>
              {editing.models.map((m, i) => (
                <div key={i} className="ai-personal-model-row">
                  <Input
                    required
                    maxLength={128}
                    value={m.id}
                    onChange={(e) => setEditing({ ...editing, models: editing.models.map((x, j) => (j === i ? { ...x, id: e.target.value } : x)) })}
                    placeholder="模型 ID，如 deepseek-chat"
                    style={{ flex: 2, minWidth: 140 }}
                  />
                  <Input
                    maxLength={100}
                    value={m.label}
                    onChange={(e) => setEditing({ ...editing, models: editing.models.map((x, j) => (j === i ? { ...x, label: e.target.value } : x)) })}
                    placeholder="显示名（可选）"
                    style={{ flex: 1, minWidth: 100 }}
                  />
                  <span className="check-list ai-personal-caps">
                    {CAPABILITY_FIELDS.map((c) => (
                      <label key={c.key} className="check-item" title={c.label}>
                        <input
                          type="checkbox"
                          checked={m.caps[c.key]}
                          onChange={(e) => setEditing({ ...editing, models: editing.models.map((x, j) => (j === i ? { ...x, caps: { ...x.caps, [c.key]: e.target.checked } } : x)) })}
                        />
                        {c.label}
                      </label>
                    ))}
                  </span>
                  <Button size="small" danger disabled={editing.models.length <= 1} onClick={() => setEditing({ ...editing, models: editing.models.filter((_, j) => j !== i) })}>删除</Button>
                </div>
              ))}
              <Button size="small" disabled={editing.models.length >= 20} onClick={() => setEditing({ ...editing, models: [...editing.models, { id: '', label: '', caps: { ...emptyCaps(), chat: true } }] })}>
                添加模型
              </Button>
            </div>
            <div className="setting-control" style={{ marginTop: 8, justifyContent: 'flex-end' }}>
              <Button onClick={() => setEditing(null)}>取消</Button>
              <Button type="primary" htmlType="submit">确定</Button>
            </div>
          </form>
        </Modal>
      )}

      {/* 人设编辑弹窗。 */}
      {personaEditing && (
        <Modal title={personas.some((p) => p.id === personaEditing.id) ? `编辑人设：${personaEditing.name || personaEditing.id}` : '添加人设'} onClose={() => setPersonaEditing(null)}>
          <form className="team-create-row ai-personal-form" onSubmit={(e) => { e.preventDefault(); commitPersona() }}>
            <label className="field">
              <span>名称（1..100 字符）</span>
              <Input autoFocus required maxLength={100} value={personaEditing.name} onChange={(e) => setPersonaEditing({ ...personaEditing, name: e.target.value })} placeholder="如：写作助手" />
            </label>
            <label className="field">
              <span>ID（1..64 位字母/数字/_-.:）</span>
              <Input required maxLength={64} value={personaEditing.id} onChange={(e) => setPersonaEditing({ ...personaEditing, id: e.target.value })} placeholder="如 writer" />
            </label>
            <label className="field">
              <span>System 提示（≤4000 字符）</span>
              <Input.TextArea
                rows={5}
                maxLength={4000}
                value={personaEditing.system_prompt}
                onChange={(e) => setPersonaEditing({ ...personaEditing, system_prompt: e.target.value })}
                placeholder="你是……，请以……风格回答。"
              />
            </label>
            <div className="setting-control" style={{ marginTop: 8, justifyContent: 'flex-end' }}>
              <Button onClick={() => setPersonaEditing(null)}>取消</Button>
              <Button type="primary" htmlType="submit" disabled={!personaEditing.name.trim() || !personaEditing.id.trim()}>确定</Button>
            </div>
          </form>
        </Modal>
      )}
    </div>
  )
}
