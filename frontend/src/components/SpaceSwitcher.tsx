import { useEffect, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Star, Clock, Layers } from 'lucide-react'
import { Segmented, Select } from 'antd'
import { Space, listSpaces } from '../api'
import { t, useLocale } from '../i18n'

/**
 * 空间切换控件组（文件页顶栏，统一空间模型）：
 * - 空间下拉：列出我可见的全部空间（默认空间置顶，含默认徽标）；选择后
 *   以 /files?space=<id> 切换，缺省即默认空间。v2.4：下拉底部「管理空间」
 *   入口移除（空间管理统一入口 = 右侧成员栏顶部按钮 / /spaces 页卡片的
 *   「管理」按钮），下拉只做切换。
 * - 全局视图（全部 / 收藏 / 最近）迷你 Segmented——受控组件，状态由页面
 *   持有并透传给 FileBrowser 的 activeView。
 * 徽标取空间名首字符（frontend hash 配色，styles.css .space-avatar-*）。
 */

/** 空间名首字符的稳定 hash（配色桶 0..7）。 */
export function spaceAvatarBucket(name: string): number {
  let h = 0
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0
  return h % 8
}

/** 空间徽标：首字母 + hash 配色（列表/下拉通用）。 */
export function SpaceAvatar({ name, size = 22 }: { name: string; size?: number }) {
  const ch = (name.trim()[0] ?? '?').toUpperCase()
  return (
    <span className={`space-avatar space-avatar-${spaceAvatarBucket(name)}`} style={{ width: size, height: size, fontSize: size * 0.5 }} aria-hidden="true">
      {ch}
    </span>
  )
}

export default function SpaceSwitcher({
  activeView,
  onViewChange,
}: {
  /** 当前视图（受控）：FilesPage 持有并传给 FileBrowser。 */
  activeView?: 'all' | 'starred' | 'recent'
  /** 视图切换回调；提供时渲染 全部/收藏/最近 Segmented。 */
  onViewChange?: (view: 'all' | 'starred' | 'recent') => void
}) {
  const navigate = useNavigate()
  const location = useLocation()
  const locale = useLocale()
  const [spaces, setSpaces] = useState<Space[]>([])
  useEffect(() => { void listSpaces().then(setSpaces).catch(() => setSpaces([])) }, [location.search === '' ? 0 : 1])

  const currentSpaceId = new URLSearchParams(location.search).get('space') ?? ''
  const ordered = [...spaces.filter((s) => s.is_default), ...spaces.filter((s) => !s.is_default)]

  return (
    <div className="space-switcher">
      <Select
        size="small"
        className="space-select"
        aria-label={locale === 'zh-CN' ? '选择空间' : 'Select space'}
        value={currentSpaceId || ordered.find((s) => s.is_default)?.id || ''}
        onChange={(v) => navigate(v ? `/files?space=${v}` : '/files')}
        options={ordered.map((s) => ({
          value: s.id,
          label: (
            <span className="space-option">
              <SpaceAvatar name={s.name} size={18} />
              <span className="space-option-name">{s.name}</span>
              {s.is_default && <span className="badge badge-default">{locale === 'zh-CN' ? '默认' : 'default'}</span>}
            </span>
          ),
        }))}
      />
      {onViewChange && (
        <Segmented
          size="small"
          aria-label={locale === 'zh-CN' ? '视图' : 'View'}
          value={activeView}
          onChange={(v) => onViewChange(v as 'all' | 'starred' | 'recent')}
          options={[
            { label: t(locale, 'viewAll'), value: 'all', icon: <Layers size={14} /> },
            { label: t(locale, 'viewStarred'), value: 'starred', icon: <Star size={14} /> },
            { label: t(locale, 'viewRecent'), value: 'recent', icon: <Clock size={14} /> },
          ]}
        />
      )}
    </div>
  )
}
