// 快捷键帮助弹窗（v1.1）：'?' 触发，Escape/点击遮罩关闭。
// hotkeyDocs 为全部快捷键的单一清单（帮助弹窗与仪表盘提示卡共用），
// 新增快捷键时在此登记。
import { useEffect } from 'react'
import { Modal } from './FileBrowser'
import { Locale, t, useLocale } from '../i18n'

export interface HotkeyDoc {
  keys: string[]
  desc: string
}

/** 全部快捷键清单（按语言渲染；新增条目时在此登记）。 */
export function hotkeyDocs(locale: Locale): Array<{ group: string; items: HotkeyDoc[] }> {
  return [
    {
      group: locale === 'zh-CN' ? '全局' : 'Global',
      items: [
        { keys: ['/'], desc: locale === 'zh-CN' ? '聚焦顶栏全文搜索' : 'Focus the top search bar' },
        { keys: ['g', 'f'], desc: locale === 'zh-CN' ? '前往文件' : 'Go to Files' },
        { keys: ['g', 't'], desc: locale === 'zh-CN' ? '前往团队' : 'Go to Teams' },
        { keys: ['g', 's'], desc: locale === 'zh-CN' ? '前往我的分享' : 'Go to My shares' },
        { keys: ['?'], desc: locale === 'zh-CN' ? '显示快捷键帮助' : 'Show keyboard shortcuts' },
      ],
    },
    {
      group: locale === 'zh-CN' ? '文件页' : 'Files page',
      items: [
        { keys: ['n'], desc: locale === 'zh-CN' ? '新建文件夹' : 'New folder' },
        { keys: ['u'], desc: locale === 'zh-CN' ? '上传文件' : 'Upload file' },
        { keys: ['Ctrl/⌘', '1'], desc: t(locale, 'hotkeyViewList') },
        { keys: ['Ctrl/⌘', '2'], desc: t(locale, 'hotkeyViewGrid') },
        { keys: ['Del'], desc: locale === 'zh-CN' ? '删除选中项' : 'Delete selected items' },
        { keys: ['Esc'], desc: locale === 'zh-CN' ? '清空选择 / 关闭对话框' : 'Clear selection / close dialogs' },
      ],
    },
  ]
}

export default function HotkeysHelp({ onClose }: { onClose: () => void }) {
  const locale = useLocale()
  // Escape 关闭帮助（useHotkeys 在弹窗打开时跳过，此处自行监听）。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <Modal wide title={locale === 'zh-CN' ? '键盘快捷键' : 'Keyboard shortcuts'} onClose={onClose}>
      <div className="hotkeys-help">
        {hotkeyDocs(locale).map((group) => (
          <div key={group.group} className="hotkey-group">
            <h4>{group.group}</h4>
            {group.items.map((item) => (
              <div key={item.desc} className="hotkey-row">
                <span className="hotkey-desc">{item.desc}</span>
                <span className="hotkey-keys">
                  {item.keys.map((k) => (
                    <kbd key={k}>{k}</kbd>
                  ))}
                </span>
              </div>
            ))}
          </div>
        ))}
        <p className="hint">
          {locale === 'zh-CN'
            ? '提示：焦点在输入框或弹窗打开时，除 Esc 外的快捷键不生效。'
            : 'Tip: shortcuts (except Esc) are disabled while typing or when a dialog is open.'}
        </p>
      </div>
    </Modal>
  )
}
