// 平台管理「配置总览」面板（v2.8，AdminPage 第六个设置 tag 懒加载）：
// a) 启动级环境变量（只读）：GET /admin/settings/env 分组渲染（key+value+
//    note；sensitive 值显示掩码 +「已脱敏」提示）；此类配置在部署文件
//    （.env / docker-compose.yml）中修改并重启生效。端点由后端并行任务
//    提供——未就绪（404 等）时显示占位提示，不阻塞页面。
// b) 运行时设置（system_settings）：基于 SETTING_KEY_META 的静态分类索引
//    卡片（AI/Agent/邮件/TLS/系统/安全/协作/上传），读一次 /admin/settings
//    取各键当前值（前 60 字符），各组标注所属修改面板。
// c) AI 能力状态：/ai/status 五能力徽章（总开关/Agent/联网搜索/MCP/RAG）。
import { useEffect, useMemo, useState } from 'react'
import { Collapse, Tag, Tooltip } from 'antd'
import type { CollapseProps } from 'antd'
import { EnvGroup, adminGetSettings, getAdminEnv } from '../api'
import { SETTING_KEY_META } from '../pages/SettingsPage'
import { useAIFeatures } from '../aiFeature'
import { useLocale } from '../i18n'

/** 运行时设置静态分类：分组顺序与展示名固定，键清单自 SETTING_KEY_META
 * 按前缀提取；panel = 该组键的修改入口面板名。mail/tls 无 system_settings
 * 运行时键（env-only / 面板内运行时状态），仅给指引。 */
const RUNTIME_GROUPS: Array<{
  id: string
  title: { zh: string; en: string }
  panel: { zh: string; en: string }
  prefixes: string[]
  envOnlyNote?: { zh: string; en: string }
}> = [
  {
    id: 'ai', title: { zh: 'AI', en: 'AI' }, panel: { zh: 'AI 设置', en: 'AI settings' },
    prefixes: ['ai'],
  },
  {
    id: 'agent', title: { zh: 'Agent', en: 'Agent' }, panel: { zh: 'AI 智能体', en: 'AI agent' },
    prefixes: ['agent'],
  },
  {
    id: 'mail', title: { zh: '邮件', en: 'Mail' }, panel: { zh: '邮件', en: 'Mail' },
    prefixes: [],
    envOnlyNote: {
      zh: '邮件通道（SMTP）为启动级 env 配置，发件人与连通性状态在「邮件」面板查看。',
      en: 'The mail channel (SMTP) is startup-level env config; sender and connectivity are shown in the Mail panel.',
    },
  },
  {
    id: 'tls', title: { zh: 'TLS', en: 'TLS' }, panel: { zh: 'TLS', en: 'TLS' },
    prefixes: [],
    envOnlyNote: {
      zh: '证书与 HTTPS 模式经「TLS」面板管理（ACME 自动证书 / 自定义证书上传）。',
      en: 'Certificates and HTTPS mode are managed in the TLS panel (ACME auto certs / custom upload).',
    },
  },
  {
    id: 'system', title: { zh: '系统', en: 'System' }, panel: { zh: '系统设置', en: 'System settings' },
    prefixes: ['site', 'share', 'retention', 'batch', 'folder', 'backup', 'audit', 'space', 'webdav'],
  },
  {
    id: 'security', title: { zh: '安全', en: 'Security' }, panel: { zh: '系统设置', en: 'System settings' },
    prefixes: ['security'],
  },
  {
    id: 'collab', title: { zh: '协作', en: 'Collaboration' }, panel: { zh: '系统设置', en: 'System settings' },
    prefixes: ['collab'],
  },
  {
    id: 'upload', title: { zh: '上传', en: 'Upload' }, panel: { zh: '系统设置', en: 'System settings' },
    prefixes: ['upload'],
  },
]

/** AI 能力徽章（键与 /ai/status 能力标志一一对应）。 */
const AI_CAP_BADGES: Array<{ key: 'enabled' | 'agent' | 'web_search' | 'mcp' | 'rag'; zh: string; en: string }> = [
  { key: 'enabled', zh: 'AI 总开关', en: 'AI enabled' },
  { key: 'agent', zh: 'Agent 创作舱', en: 'Agent studio' },
  { key: 'web_search', zh: '联网搜索', en: 'Web search' },
  { key: 'mcp', zh: 'MCP 外部工具', en: 'MCP tools' },
  { key: 'rag', zh: '知识库问答（RAG）', en: 'RAG Q&A' },
]

export default function ConfigOverviewPanel({ onError }: { onError: (m: string) => void }) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  // a) 启动级环境变量：groups = null 加载中；unavailable = 端点未就绪（404 等）。
  const [envGroups, setEnvGroups] = useState<EnvGroup[] | null>(null)
  const [envUnavailable, setEnvUnavailable] = useState(false)
  // b) 运行时设置：system_settings 当前值快照（key → value）。
  const [values, setValues] = useState<Record<string, unknown> | null>(null)
  // c) AI 能力状态（aiFeature 缓存，挂载时自动拉取 /ai/status）。
  const ai = useAIFeatures()

  useEffect(() => {
    let alive = true
    getAdminEnv()
      .then((groups) => { if (alive) setEnvGroups(groups) })
      .catch(() => { if (alive) setEnvUnavailable(true) })
    adminGetSettings()
      .then((result) => {
        if (!alive) return
        const map: Record<string, unknown> = {}
        for (const item of result.settings ?? []) map[item.key] = item.value
        setValues(map)
      })
      .catch((e) => { if (alive) onError(e instanceof Error ? e.message : (zh ? '系统设置加载失败' : 'Failed to load system settings')) })
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  /** 分组键清单（保持 SETTING_KEY_META 定义顺序）。 */
  const groups = useMemo(
    () => RUNTIME_GROUPS.map((g) => ({
      ...g,
      keys: Object.keys(SETTING_KEY_META).filter((key) => g.prefixes.some((p) => key.startsWith(`${p}.`))),
    })),
    [],
  )

  /** 键当前值预览：bool → true/false，其余 String；超 60 字符截断。 */
  const valuePreview = (key: string): string => {
    const v = values?.[key]
    if (v === undefined || v === null) return zh ? '未配置（默认）' : 'not set (default)'
    const s = typeof v === 'boolean' ? (v ? 'true' : 'false') : String(v)
    return s.length > 60 ? `${s.slice(0, 60)}…` : s
  }

  const envContent = (
    <div>
      <div className="setting-desc muted" style={{ marginBottom: 8 }}>
        {zh
          ? '此类配置在部署文件（.env / docker-compose.yml）中修改并重启生效，不提供运行时修改；文档见 docs/configuration.md。'
          : 'These are configured in deployment files (.env / docker-compose.yml) and applied on restart; see docs/configuration.md.'}
      </div>
      {envUnavailable ? (
        <div className="empty">{zh ? '环境配置端点不可用（需更新后端）' : 'Environment endpoint unavailable (backend update required)'}</div>
      ) : envGroups === null ? (
        <div className="hint">{zh ? '加载中…' : 'Loading…'}</div>
      ) : envGroups.length === 0 ? (
        <div className="empty">{zh ? '暂无环境变量分组' : 'No environment groups'}</div>
      ) : (
        envGroups.map((group) => (
          <div key={group.name} className="config-env-group">
            <h4>{group.name}</h4>
            {group.items.map((item) => (
              <div key={item.key} className="config-env-row">
                <code className="setting-value-mono" title={item.key}>{item.key}</code>
                {item.sensitive ? (
                  <Tooltip title={zh ? '已脱敏' : 'Masked'}>
                    <code className="setting-value-mono">••••</code>
                  </Tooltip>
                ) : (
                  <span className="config-env-value" title={item.value}>{item.value}</span>
                )}
                {item.note && <span className="config-env-note">{item.note}</span>}
              </div>
            ))}
          </div>
        ))
      )}
    </div>
  )

  const runtimeContent = (
    <div>
      <div className="setting-desc muted" style={{ marginBottom: 4 }}>
        {zh
          ? 'system_settings 运行时键的分类索引（只读快照，值截断展示）；修改请前往对应面板，保存即时生效。'
          : 'Category index of runtime system_settings keys (read-only snapshot, values truncated); edit in the corresponding panel, changes apply immediately.'}
      </div>
      {values === null && <div className="hint">{zh ? '加载中…' : 'Loading…'}</div>}
      <div className="config-runtime-grid">
        {groups.map((g) => {
          const panelName = zh ? g.panel.zh : g.panel.en
          return (
            <div key={g.id} className="config-runtime-card">
              <div className="config-runtime-head">
                <b>{zh ? g.title.zh : g.title.en}</b>
                <Tag className="config-runtime-panel">{panelName}</Tag>
              </div>
              {g.envOnlyNote ? (
                <div className="setting-desc muted">{zh ? g.envOnlyNote.zh : g.envOnlyNote.en}</div>
              ) : (
                g.keys.map((key) => {
                  const meta = SETTING_KEY_META[key]
                  return (
                    <div key={key} className="config-runtime-row">
                      <span title={`${key} · ${meta.dzh}`}>{zh ? meta.zh : meta.en}</span>
                      <code className="setting-value-mono" title={`${key} = ${valuePreview(key)}`}>{valuePreview(key)}</code>
                    </div>
                  )
                })
              )}
              <div className="setting-desc muted">
                {zh ? `去「${panelName}」面板修改` : `Edit in the "${panelName}" panel`}
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )

  const aiContent = (
    <div>
      <div className="setting-desc muted" style={{ marginBottom: 8 }}>
        {zh
          ? '平台 AI 能力开关状态（GET /ai/status）：总开关关闭时全站 AI 入口隐藏，各子能力驱动对应入口显隐。'
          : 'Platform AI capability flags (GET /ai/status): entries are hidden when the master switch is off; each flag drives its own entry.'}
      </div>
      <div className="config-ai-badges">
        {AI_CAP_BADGES.map(({ key, zh: labelZh, en }) => {
          const on = ai[key]
          return (
            <Tag key={key} color={on ? 'green' : 'default'} title={key}>
              {zh ? labelZh : en}：{on ? (zh ? '开启' : 'on') : (zh ? '关闭' : 'off')}
            </Tag>
          )
        })}
      </div>
    </div>
  )

  const items: CollapseProps['items'] = [
    { key: 'env', label: zh ? '启动级环境变量（只读）' : 'Startup environment variables (read-only)', children: envContent },
    { key: 'runtime', label: zh ? '运行时设置（system_settings）' : 'Runtime settings (system_settings)', children: runtimeContent },
    { key: 'ai', label: zh ? 'AI 能力状态' : 'AI capability status', children: aiContent },
  ]

  return (
    <div className="panel setting-group config-overview">
      <h3>{zh ? '配置总览' : 'Configuration overview'}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        {zh
          ? '集中查看平台两类配置与 AI 能力状态：启动级环境变量（部署文件修改、重启生效）与运行时设置（各管理面板即时生效）。'
          : 'A central view of platform configuration and AI capability flags: startup environment variables (deployment files, restart to apply) and runtime settings (admin panels, immediate).'}
      </div>
      <Collapse defaultActiveKey={['env']} items={items} />
    </div>
  )
}
