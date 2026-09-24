// AI 模型选择器（共享组件）：从 AIAssistant 抽取，供全局助手 / 编辑页
// AI 助手（AIEditChat）等复用，统一「模型切换」体验：
// - 选项 = getAIModels() 列表（providerId / providerName + model +
//   capabilities 标签），默认模型追加「（默认）」标注；
// - 命中默认模型且用户未显式选择（modelExplicit=false）：不显示选中值，
//   以 placeholder「默认（Provider / 模型）」提示（modelKey 实际仍为默认键，
//   发送/思考开关评估照常生效）；
// - 选中回调 onSelect(id)：宿主负责 setModelKey + 显式记忆
//   （localStorage docflow.ai.model，由 AI_MODEL_STORAGE_KEY 统一）。
import { Select } from 'antd'
import { AI_MODEL_STORAGE_KEY, defaultAIModelKey } from './AIAssistant'
import type { AIModelOption } from './AIAssistant'

export default function AIModelSelect({
  models,
  modelKey,
  modelExplicit,
  onSelect,
  zh,
  size = 'small',
}: {
  models: AIModelOption[]
  modelKey: string
  /** 用户是否显式选过模型（localStorage 有记忆）。 */
  modelExplicit: boolean
  /** 选中模型 id（宿主持久化到 docflow.ai.model 并置 explicit）。 */
  onSelect: (id: string) => void
  zh: boolean
  size?: 'small' | 'middle'
}) {
  const dkey = defaultAIModelKey()
  const usingDefault = !modelExplicit && !!dkey && modelKey === dkey
  const defModel = dkey ? models.find((m) => m.id === dkey) : undefined
  const defLabel = defModel
    ? `${defModel.providerName || defModel.providerId} / ${defModel.model}`
    : (dkey ?? '')
  return (
    <Select
      className="ai-model-select"
      size={size}
      value={usingDefault ? undefined : (modelKey || undefined)}
      placeholder={usingDefault
        ? (zh ? `默认（${defLabel}）` : `Default (${defLabel})`)
        : (zh ? '默认模型' : 'Default model')}
      onChange={(v) => {
        try {
          window.localStorage.setItem(AI_MODEL_STORAGE_KEY, v)
        } catch {
          /* ignore */
        }
        onSelect(v)
      }}
      options={models.map((m) => ({
        value: m.id,
        label: (
          <span className="ai-model-option">
            <span className="ai-model-option-name">{m.providerName || m.providerId} / {m.model}{m.id === dkey ? (zh ? '（默认）' : ' (default)') : ''}</span>
            {m.capabilities.map((c) => (
              <span key={c} className="ai-model-cap">{c}</span>
            ))}
          </span>
        ),
      }))}
    />
  )
}
