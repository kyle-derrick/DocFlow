// 管理后台「AI 智能体（Docker Agent 创作舱）」面板（v2.8 自 SettingsPage
// 拆出独立模块，AdminPage 懒加载）：
// - Agent 总开关（原字段照旧：GET/PUT /admin/settings/agent 通道，关闭时
//   任务 API 不可用且不启动容器）；
// - Agent AI 能力：agent.allow_ai —— 容器保持断网，经 IPC socket 调用平台
//   AI（一次性令牌 + 每任务调用上限）；agent.ai_max_calls（1-500）；
// - 产物同步模式：agent.sync_mode —— git（按 git 提交集同步，推荐，
//   .gitignore 即过滤规则）或 scan（全量扫描，内置忽略 node_modules 等）。
// 新键为 system_settings（agent.*），经面板既有 PUT /admin/settings/agent
// 通道保存；后端尚未收录定义时读取按默认值展示，保存报错经 onError 提示。
import { useEffect, useState } from 'react'
import { Button, InputNumber, Radio, Switch } from 'antd'
import { adminGetSettings, getAgentSettings, putAgentSettings } from '../api'
import { useLocale } from '../i18n'

/** 三个新键的后端默认值（键缺失 / 后端未升级时按此展示）。 */
const DEFAULT_ALLOW_AI = true
const DEFAULT_AI_MAX_CALLS = 40
const DEFAULT_SYNC_MODE: 'git' | 'scan' = 'git'

/** 需要从 system_settings 列表兜底读取的新键（agent 端点键清单未含时）。 */
const NEW_SETTING_KEYS = ['agent.allow_ai', 'agent.ai_max_calls', 'agent.sync_mode'] as const

export default function AgentPanel({ onError, onNotice }: { onError: (m: string) => void; onNotice: (m: string) => void }) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const [cfg, setCfg] = useState<Record<string, unknown> | null>(null)
  const [busy, setBusy] = useState(false)
  /** 正在保存的键（agent 端点无前缀形态，如 allow_ai）。 */
  const [savingKey, setSavingKey] = useState<string | null>(null)
  /** 每任务 AI 调用上限草稿（保存按钮提交）。 */
  const [maxCallsDraft, setMaxCallsDraft] = useState<number | null>(null)

  const load = async () => {
    try {
      const agent = await getAgentSettings()
      const merged: Record<string, unknown> = { ...agent }
      // 兜底：agent 配置端点键清单未含新键（后端并行升级中）时，从
      // /admin/settings 定义列表读当前值；两处都缺省按默认展示。
      try {
        const sys = await adminGetSettings()
        for (const key of NEW_SETTING_KEYS) {
          if (merged[key.slice('agent.'.length)] === undefined) {
            const hit = (sys.settings ?? []).find((item) => item.key === key)
            if (hit) merged[key.slice('agent.'.length)] = hit.value
          }
        }
      } catch {
        // 系统设置列表读取失败不影响面板（新键按默认值展示）。
      }
      setCfg(merged)
      const raw = Number(merged.ai_max_calls)
      setMaxCallsDraft(Number.isFinite(raw) && raw > 0 ? Math.round(raw) : DEFAULT_AI_MAX_CALLS)
    } catch (e) {
      onError(e instanceof Error ? e.message : (zh ? 'Agent 配置加载失败' : 'Failed to load agent settings'))
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  /** 经面板既有 PUT /admin/settings/agent 通道保存（部分字段 PATCH 语义：
   *  响应与本地状态合并，agent 关闭时后端仅回 {enabled} 也不丢其余键）。 */
  const saveKeys = async (patch: Record<string, unknown>, okMessage: string) => {
    if (busy) return
    const key = Object.keys(patch)[0]
    setBusy(true)
    setSavingKey(key)
    try {
      const next = await putAgentSettings(patch)
      setCfg((prev) => ({ ...(prev ?? {}), ...next }))
      onNotice(okMessage)
    } catch (e) {
      onError(e instanceof Error ? e.message : (zh ? '保存失败' : 'Save failed'))
    } finally {
      setBusy(false)
      setSavingKey(null)
    }
  }

  if (!cfg) {
    return (
      <div className="panel setting-group">
        <h3>{zh ? 'AI 创作舱' : 'Agent studio'}</h3>
        <div className="hint">{zh ? '加载中…' : 'Loading…'}</div>
      </div>
    )
  }

  const enabled = cfg.enabled === true
  const allowAi = typeof cfg.allow_ai === 'boolean' ? cfg.allow_ai : DEFAULT_ALLOW_AI
  const rawCalls = Number(cfg.ai_max_calls)
  const maxCalls = Number.isFinite(rawCalls) && rawCalls > 0 ? Math.round(rawCalls) : DEFAULT_AI_MAX_CALLS
  const syncMode: 'git' | 'scan' = cfg.sync_mode === 'scan' ? 'scan' : DEFAULT_SYNC_MODE
  const harness: 'auto' | 'claude-code' | 'pi' | 'builtin' =
    cfg.harness === 'claude-code' || cfg.harness === 'pi' || cfg.harness === 'builtin' ? cfg.harness : 'auto'
  const maxCallsDirty = maxCallsDraft !== null && Number.isInteger(maxCallsDraft) && maxCallsDraft !== maxCalls

  return (
    <div className="panel setting-group">
      <h3>{zh ? 'Docker Agent 创作舱' : 'Docker Agent studio'}</h3>
      <div className="setting-desc muted">
        {zh
          ? '安全边界：关闭时 API 不可用且不启动容器；任务会先创建目录快照；Agent 产物必须经过差异预览和用户确认后才会写回平台，Docker runtime 按需启用。'
          : 'Security boundary: API disabled and no containers when off; tasks snapshot the directory first; agent outputs are written back only after diff preview and user confirmation.'}
      </div>
      {/* Agent 总开关（原有字段，读写通道照旧）。 */}
      <div className="setting-row">
        <div className="setting-main">
          <div className="setting-key">{zh ? 'Agent 开关' : 'Agent enabled'}</div>
          <div className="setting-desc muted">
            {zh ? '默认关闭；开启后仍需配置镜像白名单。' : 'Off by default; configure the image allowlist after enabling.'}
          </div>
        </div>
        <div className="setting-control">
          <Switch
            checked={enabled}
            loading={savingKey === 'enabled'}
            disabled={busy && savingKey !== 'enabled'}
            onChange={(v) => void saveKeys({ enabled: v }, v ? (zh ? 'Agent 已开启' : 'Agent enabled') : (zh ? 'Agent 已关闭' : 'Agent disabled'))}
          />
        </div>
      </div>
      {/* Agent AI 能力：断网容器经 IPC 调用平台 AI。 */}
      <div className="setting-row">
        <div className="setting-main">
          <div className="setting-key">
            {zh ? 'Agent AI 能力' : 'Agent AI capability'} <code className="setting-desc muted">agent.allow_ai</code>
          </div>
          <div className="setting-desc muted">
            {zh
              ? '容器保持断网，经 IPC socket 调用平台 AI（一次性令牌 + 每任务调用上限），断网容器仍有智能。'
              : 'Containers stay offline and call platform AI over an IPC socket (one-time token + per-task call cap).'}
          </div>
        </div>
        <div className="setting-control">
          <span className="setting-bool">
            <Switch
              size="small"
              checked={allowAi}
              loading={savingKey === 'allow_ai'}
              disabled={busy && savingKey !== 'allow_ai'}
              onChange={(v) => void saveKeys({ allow_ai: v }, zh ? `已保存 agent.allow_ai（当前值：${v ? '开启' : '关闭'}）` : `Saved agent.allow_ai (now ${v ? 'on' : 'off'})`)}
            />
            <span>{allowAi ? (zh ? '开启' : 'On') : (zh ? '关闭' : 'Off')}</span>
          </span>
        </div>
      </div>
      <div className="setting-row">
        <div className="setting-main">
          <div className="setting-key">
            {zh ? '每任务 AI 调用上限' : 'Per-task AI call cap'} <code className="setting-desc muted">agent.ai_max_calls</code>
          </div>
          <div className="setting-desc muted">
            {zh ? '单个 Agent 任务的 AI 调用次数上限（1-500），防止失控循环消耗配额。' : 'Max AI calls per agent task (1-500) to stop runaway loops.'}
          </div>
        </div>
        <div className="setting-control">
          <InputNumber
            min={1}
            max={500}
            step={1}
            precision={0}
            style={{ width: 110 }}
            value={maxCallsDraft}
            disabled={busy}
            onChange={(v) => setMaxCallsDraft(v === null || v === undefined ? null : v)}
            aria-label={zh ? '每任务 AI 调用上限' : 'Per-task AI call cap'}
          />
          <Button
            size="small"
            type="primary"
            disabled={!maxCallsDirty}
            loading={savingKey === 'ai_max_calls'}
            onClick={() => {
              if (!maxCallsDirty || maxCallsDraft === null) return
              void saveKeys({ ai_max_calls: maxCallsDraft }, zh ? `已保存 agent.ai_max_calls（当前值：${maxCallsDraft}）` : `Saved agent.ai_max_calls (value: ${maxCallsDraft})`)
            }}
          >
            {zh ? '保存' : 'Save'}
          </Button>
        </div>
      </div>
      {/* 产物同步模式：git 提交集（推荐）或全量扫描。 */}
      <div className="setting-row" style={{ alignItems: 'flex-start' }}>
        <div className="setting-main">
          <div className="setting-key">
            {zh ? '产物同步' : 'Artifact sync'} <code className="setting-desc muted">agent.sync_mode</code>
          </div>
          <div className="setting-desc muted">
            {zh ? 'Agent 产物写回平台时的同步方式，修改对后续任务生效。' : 'How agent artifacts sync back to the platform; applies to subsequent tasks.'}
          </div>
        </div>
        <div className="setting-control" style={{ flexDirection: 'column', alignItems: 'flex-start', gap: 6 }}>
          <Radio.Group
            value={syncMode}
            disabled={busy}
            onChange={(e) => {
              const next = e.target.value as 'git' | 'scan'
              if (next === syncMode) return
              void saveKeys({ sync_mode: next }, zh ? `已保存 agent.sync_mode（当前值：${next === 'git' ? 'git 提交集同步' : '全量扫描'}）` : `Saved agent.sync_mode (value: ${next})`)
            }}
          >
            <div className="agent-sync-options">
              <Radio value="git">
                <span>{zh ? '按 git 提交集同步（推荐）' : 'Sync by git commit set (recommended)'}</span>
                <span className="setting-desc muted">
                  {zh ? '以 git 提交集为准同步，.gitignore 即过滤规则' : 'Sync the git commit set; .gitignore acts as the filter'}
                </span>
              </Radio>
              <Radio value="scan">
                <span>{zh ? '全量扫描' : 'Full scan'}</span>
                <span className="setting-desc muted">
                  {zh ? '扫描工作区全部产物（内置忽略 node_modules 等）' : 'Scan the whole workspace (built-in ignores like node_modules)'}
                </span>
              </Radio>
            </div>
          </Radio.Group>
        </div>
      </div>

      <div className="setting-row" style={{ alignItems: 'flex-start' }}>
        <div className="setting-main">
          <div className="setting-key">
            {zh ? '执行引擎（Harness）' : 'Execution harness'} <code className="setting-desc muted">agent.harness</code>
          </div>
          <div className="setting-desc muted">
            {zh
              ? '沙箱内的 agent 执行引擎：auto 按平台默认模型协议自动选择（Anthropic → Claude Code，OpenAI 兼容 → pi）。'
              : 'Agent execution engine inside the sandbox: auto routes by the platform default provider protocol (Anthropic → Claude Code, OpenAI-compatible → pi).'}
          </div>
        </div>
        <div className="setting-control" style={{ flexDirection: 'column', alignItems: 'flex-start', gap: 6 }}>
          <Radio.Group
            value={harness}
            disabled={busy}
            onChange={(e) => {
              const next = e.target.value as 'auto' | 'claude-code' | 'pi' | 'builtin'
              if (next === harness) return
              void saveKeys({ harness: next }, zh ? `已保存 agent.harness（当前值：${next}）` : `Saved agent.harness (value: ${next})`)
            }}
          >
            <div className="agent-sync-options">
              <Radio value="auto">
                <span>{zh ? '自动（推荐）' : 'Auto (recommended)'}</span>
                <span className="setting-desc muted">{zh ? '按默认 Provider 协议路由' : 'Route by default provider protocol'}</span>
              </Radio>
              <Radio value="claude-code">
                <span>Claude Code</span>
                <span className="setting-desc muted">{zh ? 'Anthropic 协议（工具透传）' : 'Anthropic protocol (tool pass-through)'}</span>
              </Radio>
              <Radio value="pi">
                <span>pi</span>
                <span className="setting-desc muted">{zh ? 'OpenAI 兼容协议（badlogic/pi-mono）' : 'OpenAI-compatible protocol (badlogic/pi-mono)'}</span>
              </Radio>
              <Radio value="builtin">
                <span>{zh ? '内置 runner' : 'Builtin runner'}</span>
                <span className="setting-desc muted">{zh ? '轻量兜底（无外部依赖）' : 'Lightweight fallback (no external deps)'}</span>
              </Radio>
            </div>
          </Radio.Group>
        </div>
      </div>
    </div>
  )
}
