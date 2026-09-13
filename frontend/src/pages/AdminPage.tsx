// 管理设置页（仅 admin 角色）：顶部五类计数概览 + 按前缀分组的设置卡片，
// 行内编辑保存（bool 开关 / int 数字 / string 文本，按 value type 渲染）。
// 非 admin（403）显示无权限页；保存成功后刷新设置列表并提示。
import { FormEvent, useEffect, useMemo, useState } from 'react'
import {
  AdminStats,
  ApiError,
  SettingItem,
  SettingType,
  SettingValue,
  adminGetSettings,
  adminGetStats,
  adminPutSetting,
} from '../api'
import { formatTime } from '../components/FileBrowser'

/** 设置键前缀 → 分组标题（未知前缀回退原样）。 */
const groupTitles: Record<string, string> = {
  site: '站点',
  upload: '上传',
  share: '分享',
  retention: '保留策略',
}

const typeText: Record<SettingType, string> = {
  bool: '布尔',
  int: '整数',
  string: '文本',
}

const statCards: Array<{ key: keyof AdminStats; label: string }> = [
  { key: 'users', label: '用户' },
  { key: 'files', label: '文件' },
  { key: 'uploads', label: '上传会话' },
  { key: 'sessions', label: '登录会话' },
  { key: 'shares', label: '分享' },
]

function formatSettingValue(value: SettingValue): string {
  return String(value)
}

export default function AdminPage() {
  const [stats, setStats] = useState<AdminStats | null>(null)
  const [settings, setSettings] = useState<SettingItem[]>([])
  const [loading, setLoading] = useState(true)
  const [forbidden, setForbidden] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  // 行内编辑状态：当前编辑键 + 草稿（bool 直接存布尔，int/string 存字符串）。
  const [editingKey, setEditingKey] = useState<string | null>(null)
  const [draft, setDraft] = useState<string | boolean>('')
  const [savingKey, setSavingKey] = useState<string | null>(null)
  const [rowError, setRowError] = useState('')

  const load = async () => {
    setLoading(true)
    setError('')
    try {
      const [list, st] = await Promise.all([adminGetSettings(), adminGetStats()])
      setSettings(list)
      setStats(st)
      setForbidden(false)
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) {
        setForbidden(true)
        setError('')
      } else {
        setError(err instanceof Error ? err.message : '加载失败')
      }
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [])

  /** 按键前缀分组（保持 Definitions 输出顺序）。 */
  const groups = useMemo(() => {
    const map = new Map<string, SettingItem[]>()
    for (const item of settings) {
      const prefix = item.key.split('.')[0]
      const list = map.get(prefix) ?? []
      list.push(item)
      map.set(prefix, list)
    }
    return Array.from(map.entries())
  }, [settings])

  const startEdit = (item: SettingItem) => {
    setEditingKey(item.key)
    setDraft(item.type === 'bool' ? Boolean(item.value) : String(item.value))
    setRowError('')
    setNotice('')
  }

  const handleSave = async (e: FormEvent, item: SettingItem) => {
    e.preventDefault()
    if (editingKey !== item.key) return
    setRowError('')
    let value: SettingValue
    if (item.type === 'bool') {
      value = Boolean(draft)
    } else if (item.type === 'int') {
      const parsed = Number(draft)
      if (draft === '' || !Number.isInteger(parsed)) {
        setRowError('请输入整数')
        return
      }
      value = parsed
    } else {
      value = String(draft).trim()
    }
    setSavingKey(item.key)
    try {
      const normalized = await adminPutSetting(item.key, value)
      setEditingKey(null)
      setNotice(`已保存 ${item.key}（当前值：${String(normalized)}）`)
      // 保存成功后刷新设置列表（含 updated_at/updated_by 与服务端归一化结果）。
      try {
        setSettings(await adminGetSettings())
      } catch {
        // 列表刷新失败不打断，保留本地已保存状态
      }
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) setRowError('无权限')
      else setRowError(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSavingKey(null)
    }
  }

  if (forbidden) {
    return (
      <div className="page">
        <div className="page-head">
          <h2>管理设置</h2>
        </div>
        <div className="admin-forbidden">
          <h3>无访问权限</h3>
          <p>该页面仅系统管理员（admin 角色）可访问。</p>
        </div>
      </div>
    )
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>管理设置</h2>
        <button className="btn ghost" onClick={() => { setNotice(''); void load() }}>刷新</button>
      </div>

      {notice && <div className="banner ok">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">加载中…</div>}

      {!loading && stats && (
        <div className="stats-grid">
          {statCards.map(({ key, label }) => (
            <div key={key} className="stat-card">
              <div className="stat-value">{stats[key]}</div>
              <div className="stat-label">{label}</div>
            </div>
          ))}
        </div>
      )}

      {!loading && groups.map(([prefix, items]) => (
        <div key={prefix} className="panel setting-group">
          <h3>{groupTitles[prefix] ?? prefix}</h3>
          {items.map((item) => {
            const editing = editingKey === item.key
            return (
              <div key={item.key} className="setting-row">
                <div className="setting-main">
                  <div className="setting-key">{item.key}</div>
                  <div className="setting-desc muted">{item.description}</div>
                  <div className="setting-meta muted">
                    类型 {typeText[item.type]} · 默认值 {formatSettingValue(item.default)}
                    {item.updated_at && ` · 更新于 ${formatTime(item.updated_at)}`}
                  </div>
                </div>
                <div className="setting-control">
                  {editing ? (
                    <form className="setting-edit" onSubmit={(e) => void handleSave(e, item)}>
                      {item.type === 'bool' ? (
                        <label className="setting-bool">
                          <input
                            type="checkbox"
                            checked={Boolean(draft)}
                            onChange={(e) => setDraft(e.target.checked)}
                          />
                          <span>{draft ? '开启' : '关闭'}</span>
                        </label>
                      ) : item.type === 'int' ? (
                        <input
                          type="number"
                          step={1}
                          autoFocus
                          value={String(draft)}
                          onChange={(e) => setDraft(e.target.value)}
                        />
                      ) : (
                        <input
                          type="text"
                          autoFocus
                          value={String(draft)}
                          onChange={(e) => setDraft(e.target.value)}
                        />
                      )}
                      <button
                        type="submit"
                        className="btn small primary"
                        disabled={savingKey !== null || (item.type === 'string' && String(draft).trim() === '')}
                      >
                        {savingKey === item.key ? '保存中…' : '保存'}
                      </button>
                      <button
                        type="button"
                        className="btn small"
                        disabled={savingKey !== null}
                        onClick={() => { setEditingKey(null); setRowError('') }}
                      >
                        取消
                      </button>
                    </form>
                  ) : (
                    <>
                      <span className={item.type === 'bool' ? `badge ${item.value ? 'available' : 'failed'}` : 'setting-value-mono'}>
                        {item.type === 'bool' ? (item.value ? '开启' : '关闭') : formatSettingValue(item.value)}
                      </span>
                      <button className="btn small" onClick={() => startEdit(item)}>编辑</button>
                    </>
                  )}
                </div>
                {editing && rowError && <div className="error-text setting-row-error">{rowError}</div>}
              </div>
            )
          })}
        </div>
      ))}
    </div>
  )
}
