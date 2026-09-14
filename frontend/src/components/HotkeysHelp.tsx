// 快捷键帮助弹窗（v1.1）：'?' 触发，Escape/点击遮罩关闭。
// HOTKEY_DOCS 为全部快捷键的单一清单（帮助弹窗与仪表盘提示卡共用），
// 新增快捷键时在此登记。
import { useEffect } from 'react'
import { Modal } from './FileBrowser'

export interface HotkeyDoc {
  keys: string[]
  desc: string
}

export const HOTKEY_DOCS: Array<{ group: string; items: HotkeyDoc[] }> = [
  {
    group: '全局',
    items: [
      { keys: ['/'], desc: '聚焦顶栏全文搜索' },
      { keys: ['g', 'f'], desc: '前往文件' },
      { keys: ['g', 't'], desc: '前往团队' },
      { keys: ['g', 's'], desc: '前往我的分享' },
      { keys: ['g', 'h'], desc: '前往回收站' },
      { keys: ['?'], desc: '显示快捷键帮助' },
    ],
  },
  {
    group: '文件页',
    items: [
      { keys: ['n'], desc: '新建文件夹' },
      { keys: ['u'], desc: '上传文件' },
      { keys: ['Del'], desc: '删除选中项' },
      { keys: ['Esc'], desc: '清空选择 / 关闭对话框' },
    ],
  },
]

export default function HotkeysHelp({ onClose }: { onClose: () => void }) {
  // Escape 关闭帮助（useHotkeys 在弹窗打开时跳过，此处自行监听）。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <Modal wide title="键盘快捷键" onClose={onClose}>
      <div className="hotkeys-help">
        {HOTKEY_DOCS.map((group) => (
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
        <p className="hint">提示：焦点在输入框或弹窗打开时，除 Esc 外的快捷键不生效。</p>
      </div>
    </Modal>
  )
}
