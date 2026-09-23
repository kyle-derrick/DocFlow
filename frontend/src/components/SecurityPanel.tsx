// 平台管理「安全与访问」面板（v2.9 第七个平台设置 tag，AdminPage 懒加载）：
// - 登录防爆破：security.login_max_retries / security.login_lock_minutes ——
//   同一「用户名 + IP」连续失败 N 次锁定 M 分钟，覆盖 Web 登录与 WebDAV
//   Basic Auth，锁定到期自动解除；
// - 认证限流与扫描策略：security.rate_limit_per_minute /
//   security.scan_quarantine_policy（security.* 全部键自系统设置面板迁入，
//   保持 security.*/webdav.* 的单一编辑入口）；
// - WebDAV 文件挂载：webdav.enabled 平台开关 + 挂载路径/个人令牌指引
//  （展示型；令牌由各用户在「设置 → WebDAV」自行创建）。
// 全部键经 /admin/settings（system_settings 通道）读写；GET 缺键（后端
// 并行升级未收录）时按默认值展示，保存报错经行内提示「后端未就绪」不阻塞。
import { useEffect, useState } from 'react'
import { Button, InputNumber, Select, Switch } from 'antd'
import { ApiError, SettingItem, adminGetSettings, adminPutSetting } from '../api'
import { SETTING_KEY_META } from '../pages/SettingsPage'
import { useLocale } from '../i18n'

/** 本面板管理的键与后端默认值/范围（GET 缺键或后端未收录时按默认展示；
 * 范围与 internal/settings Definitions 对齐，键名冻结）。 */
const MANAGED: Record<string, { kind: 'int' | 'string' | 'bool'; def: number | string | boolean; min?: number; max?: number }> = {
  'security.login_max_retries': { kind: 'int', def: 5, min: 1, max: 100 },
  'security.login_lock_minutes': { kind: 'int', def: 15, min: 1, max: 10080 },
  'security.rate_limit_per_minute': { kind: 'int', def: 120, min: 0, max: 100000 },
  'security.scan_quarantine_policy': { kind: 'string', def: 'quarantine' },
  'webdav.enabled': { kind: 'bool', def: false },
}

/** 生效方式徽章文案（与系统设置面板同口径，值取 /admin/settings 的 effect）。 */
const EFFECT_TEXT: Record<string, { zh: string; en: string }> = {
  immediate: { zh: '立即生效', en: 'Immediate' },
  new_session: { zh: '新会话生效', en: 'New session' },
  restart: { zh: '需重启生效', en: 'Restart required' },
}

export default function SecurityPanel({ onError, onNotice }: { onError: (m: string) => void; onNotice: (m: string) => void }) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  /** 键 → /admin/settings 条目（仅本面板管理的键；null = 尚未加载）。 */
  const [items, setItems] = useState<Record<string, SettingItem> | null>(null)
  const [loading, setLoading] = useState(true)
  /** int/string 键的草稿（保存按钮提交）。 */
  const [drafts, setDrafts] = useState<Record<string, number | string>>({})
  const [savingKey, setSavingKey] = useState<string | null>(null)
  const [rowError, setRowError] = useState('')

  /** 应用一次 /admin/settings 结果：条目映射 + 重置草稿为服务端现值。 */
  const apply = (result: { settings?: SettingItem[] }) => {
    const map: Record<string, SettingItem> = {}
    for (const item of result.settings ?? []) {
      if (MANAGED[item.key]) map[item.key] = item
    }
    setItems(map)
    const next: Record<string, number | string> = {}
    for (const [key, spec] of Object.entries(MANAGED)) {
      if (spec.kind === 'bool') continue
      next[key] = (map[key]?.value as number | string | undefined) ?? (spec.def as number | string)
    }
    setDrafts(next)
  }

  const load = async () => {
    setLoading(true)
    try {
      apply(await adminGetSettings())
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : (zh ? '安全设置加载失败' : 'Failed to load security settings'))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const save = async (key: string, value: number | string | boolean) => {
    if (savingKey !== null) return
    setRowError('')
    setSavingKey(key)
    try {
      const normalized = await adminPutSetting(key, value)
      onNotice(zh ? `已保存 ${key}（当前值：${String(normalized)}）` : `Saved ${key} (value: ${String(normalized)})`)
      // 刷新取回服务端归一化值与生效方式徽章（不触发整面板 loading）。
      apply(await adminGetSettings())
    } catch (err) {
      if (err instanceof ApiError && (err.status === 400 || err.status === 404)) {
        setRowError(zh ? `保存 ${key} 失败：后端未就绪（键未收录或取值超范围）` : `Failed to save ${key}: backend not ready (unknown key or out of range)`)
      } else if (err instanceof ApiError && err.status === 403) {
        setRowError(zh ? '无权限' : 'Forbidden')
      } else {
        setRowError(err instanceof Error ? err.message : (zh ? '保存失败' : 'Save failed'))
      }
    } finally {
      setSavingKey(null)
    }
  }

  /** 主名称中文（SETTING_KEY_META）+ 小字 code 原 key，与系统设置面板一致。 */
  const label = (key: string) => {
    const m = SETTING_KEY_META[key]
    return m ? (zh ? m.zh : m.en) : key
  }
  const desc = (key: string) => {
    const m = SETTING_KEY_META[key]
    return m ? (zh ? m.dzh : m.den) : ''
  }
  const effectBadge = (key: string) => {
    const effect = items?.[key]?.effect
    if (!effect) return null
    const text = EFFECT_TEXT[effect]
    return (
      <span className="badge" style={{ marginLeft: 8 }} title={zh ? '变更生效方式' : 'How changes take effect'}>
        {text ? (zh ? text.zh : text.en) : effect}
      </span>
    )
  }

  /** int 行：InputNumber + 保存（草稿与当前值不同才可保存）。 */
  const intRow = (key: string, unit?: { zh: string; en: string }) => {
    const spec = MANAGED[key]
    const draft = drafts[key]
    const current = (items?.[key]?.value as number | undefined) ?? (spec.def as number)
    const dirty = draft !== undefined && Number(draft) !== current
    return (
      <div key={key} className="setting-row">
        <div className="setting-main">
          <div className="setting-key">
            <span>{label(key)}</span>
            <code className="setting-key-code" title={key}>{key}</code>
            {effectBadge(key)}
          </div>
          <div className="setting-desc muted">{desc(key)}</div>
        </div>
        <div className="setting-control">
          <InputNumber
            min={spec.min}
            max={spec.max}
            step={1}
            precision={0}
            style={{ width: 130 }}
            value={typeof draft === 'number' ? draft : Number(draft)}
            disabled={savingKey !== null}
            onChange={(v) => setDrafts((prev) => ({ ...prev, [key]: v === null || v === undefined ? (spec.def as number) : v }))}
            addonAfter={unit ? (zh ? unit.zh : unit.en) : undefined}
            aria-label={label(key)}
          />
          <Button
            size="small"
            type="primary"
            loading={savingKey === key}
            disabled={savingKey !== null || !dirty}
            onClick={() => void save(key, Number(drafts[key]))}
          >
            {zh ? '保存' : 'Save'}
          </Button>
        </div>
      </div>
    )
  }

  /** 扫描策略行：Select（quarantine/reject）+ 保存。 */
  const policyRow = () => {
    const key = 'security.scan_quarantine_policy'
    const draft = String(drafts[key] ?? MANAGED[key].def)
    const current = String((items?.[key]?.value as string | undefined) ?? MANAGED[key].def)
    return (
      <div key={key} className="setting-row">
        <div className="setting-main">
          <div className="setting-key">
            <span>{label(key)}</span>
            <code className="setting-key-code" title={key}>{key}</code>
            {effectBadge(key)}
          </div>
          <div className="setting-desc muted">
            {zh
              ? '扫描失败处理策略：quarantine（隔离，可在「威胁防护」处置）或 reject（上传时直接拒绝）'
              : 'Policy on scan failure: quarantine (handled in Threat protection) or reject (refuse at upload)'}
          </div>
        </div>
        <div className="setting-control">
          <Select
            style={{ width: 200 }}
            disabled={savingKey !== null}
            value={draft}
            onChange={(v) => setDrafts((prev) => ({ ...prev, [key]: v }))}
            options={[
              { value: 'quarantine', label: zh ? 'quarantine（隔离）' : 'quarantine' },
              { value: 'reject', label: zh ? 'reject（拒绝）' : 'reject' },
            ]}
          />
          <Button
            size="small"
            type="primary"
            loading={savingKey === key}
            disabled={savingKey !== null || draft === current}
            onClick={() => void save(key, draft)}
          >
            {zh ? '保存' : 'Save'}
          </Button>
        </div>
      </div>
    )
  }

  if (loading) {
    return (
      <div className="panel setting-group">
        <h3>{zh ? '安全与访问' : 'Security & access'}</h3>
        <div className="hint">{zh ? '加载中…' : 'Loading…'}</div>
      </div>
    )
  }

  const webdavOn = Boolean((items?.['webdav.enabled']?.value as boolean | undefined) ?? MANAGED['webdav.enabled'].def)

  return (
    <>
      {/* 登录防爆破（键名冻结：security.login_max_retries / login_lock_minutes）。 */}
      <div className="panel setting-group">
        <h3>{zh ? '登录防爆破' : 'Login anti-bruteforce'}</h3>
        <div className="setting-desc muted" style={{ marginBottom: 12 }}>
          {zh
            ? '同一「用户名 + IP」连续登录失败达到阈值后锁定一段时间，覆盖 Web 登录与 WebDAV Basic Auth；锁定到期自动解除，管理员也可在「人员与组」启用用户提前解锁。'
            : 'The same username+IP is locked out after N consecutive failures, covering web login and WebDAV basic auth; the lock lifts automatically on expiry, or an admin can re-enable the user early under People & groups.'}
        </div>
        {intRow('security.login_max_retries', { zh: '次', en: 'times' })}
        {intRow('security.login_lock_minutes', { zh: '分钟', en: 'min' })}
      </div>

      {/* 认证限流与扫描策略（security.* 其余两键，随本面板统一管理）。 */}
      <div className="panel setting-group">
        <h3>{zh ? '认证限流与扫描策略' : 'Auth rate limit & scan policy'}</h3>
        <div className="setting-desc muted" style={{ marginBottom: 12 }}>
          {zh
            ? '认证类 API 的每分钟请求上限与安全扫描失败处理策略；security.* 键全部在「安全与访问」面板管理，不再出现在「系统设置」。'
            : 'Per-minute cap for auth APIs and the policy on scan failures; all security.* keys are managed here, not in System settings.'}
        </div>
        {intRow('security.rate_limit_per_minute', { zh: '次/分', en: 'req/min' })}
        {policyRow()}
      </div>

      {/* WebDAV 文件挂载：平台开关 + 展示型接入指引。 */}
      <div className="panel setting-group">
        <h3>{zh ? 'WebDAV 文件挂载' : 'WebDAV file mount'}</h3>
        <div className="setting-row">
          <div className="setting-main">
            <div className="setting-key">
              <span>{label('webdav.enabled')}</span>
              <code className="setting-key-code" title="webdav.enabled">webdav.enabled</code>
              {effectBadge('webdav.enabled')}
            </div>
            <div className="setting-desc muted">{desc('webdav.enabled')}</div>
          </div>
          <div className="setting-control">
            <span className="setting-bool" title={zh ? 'bool 行直开直关：切换后立即保存' : 'Boolean rows save immediately on toggle'}>
              <Switch
                size="small"
                checked={webdavOn}
                loading={savingKey === 'webdav.enabled'}
                onChange={(v) => void save('webdav.enabled', v)}
              />
              <span>{savingKey === 'webdav.enabled' ? (zh ? '保存中…' : 'Saving…') : webdavOn ? (zh ? '开启' : 'On') : (zh ? '关闭' : 'Off')}</span>
            </span>
          </div>
        </div>
        <div className="panel-inner">
          <div className="setting-key" style={{ marginBottom: 4 }}>{zh ? '接入指引' : 'How to connect'}</div>
          <div className="setting-desc muted">
            {zh ? '挂载路径格式 ' : 'Mount path format '}
            <code className="setting-key-code">{'/webdav/{空间名}/{路径}'}</code>
            {zh
              ? '（根路径列出该账号有权限的全部空间）。鉴权为 Basic Auth：用户名为平台邮箱/用户名，密码为个人 WebDAV 令牌——由各用户在「设置 → WebDAV」创建与吊销；登录防爆破与上方一致（同一用户名+IP 连续失败锁定）。客户端：Windows「映射网络驱动器」、macOS Finder「连接服务器」、Linux davfs2；详见 docs/configuration.md。'
              : ' (the root lists every space the account can access). Auth is basic: username is the platform email/username, password is a personal WebDAV token created under Settings → WebDAV; the anti-bruteforce rules above apply here too (per username+IP). Clients: Windows map network drive, macOS Finder Connect to Server, Linux davfs2; see docs/configuration.md.'}
          </div>
        </div>
      </div>

      {rowError && <div className="banner error">{rowError}</div>}
    </>
  )
}
