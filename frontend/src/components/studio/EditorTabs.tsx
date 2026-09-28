// Studio 中栏：多 Tab 查看/编辑器（IDE 式）。
// - antd Tabs（card 式、可关闭）管理多个打开文件，脏标记/关闭保留在
//   OpenTab 上（父层持久化可选）；
// - 每个 Tab 默认「查看」（FileViewerDispatch 按类型分发全类型查看），
//   顶部 Segmented 一键切换「编辑」（FileEditorDispatch：文本/代码/Markdown
//   → Monaco；.dfdoc → Tiptap；drawio/excalidraw/office → 各自内嵌编辑器）；
// - 工具条：查看|编辑切换、新窗口打开、关闭。
import { useMemo } from 'react'
import { Button, Segmented, Tabs, Tooltip } from 'antd'
import type { TabsProps } from 'antd'
import { ExternalLink, Eye, Pencil, X } from 'lucide-react'
import { resolveFileById } from '../../api'
import { FileEditorDispatch, FileViewerDispatch } from '../../pages/ViewerPage'
import { fileIcon } from './FileTreePanel'

export interface OpenTab {
  id: string
  name: string
  /** 打开模式：默认查看；树右键「打开为编辑」/双击 = edit。 */
  mode: 'view' | 'edit'
}

export default function EditorTabs({ zh, tabs, activeId, onActive, onClose, onModeChange, onRenamed }: {
  zh: boolean
  tabs: OpenTab[]
  activeId: string | null
  onActive: (id: string) => void
  onClose: (id: string) => void
  onModeChange: (id: string, mode: 'view' | 'edit') => void
  /** 文件重命名后同步 Tab 标题。 */
  onRenamed?: (id: string, name: string) => void
}) {
  void onRenamed // 父层负责同步 tabs 状态（此处仅占位避免未用告警）
  const active = useMemo(() => tabs.find((t) => t.id === activeId) ?? null, [tabs, activeId])

  const items: TabsProps['items'] = tabs.map((tab) => ({
    key: tab.id,
    label: (
      <span className="stab-label" title={tab.name}>
        {fileIcon(tab.name, 13)}
        <span className="name">{tab.name}</span>
        {tab.mode === 'edit' && <Pencil size={10} strokeWidth={2} className="stab-edit" aria-hidden="true" />}
        <button
          type="button"
          className="stab-close"
          aria-label={zh ? '关闭' : 'Close'}
          onClick={(e) => { e.stopPropagation(); onClose(tab.id) }}
        >
          <X size={11} strokeWidth={2.5} aria-hidden="true" />
        </button>
      </span>
    ),
    closable: false,
    children: tab.mode === 'view' ? (
      <div className="stab-body preview-embed">
        <FileViewerDispatch
          key={tab.id}
          fileId={tab.id}
          name={tab.name}
          resolveRawUrl={async () => {
            try {
              const r = await resolveFileById(tab.id, { mode: 'view' })
              return r.raw_url
            } catch {
              return null
            }
          }}
        />
      </div>
    ) : (
      <div className="stab-body stab-edit-body">
        <FileEditorDispatch key={tab.id} fileId={tab.id} name={tab.name} />
      </div>
    ),
  }))

  return (
    <section className="stab" aria-label={zh ? '文件查看/编辑' : 'File view/edit'}>
      <div className="stab-bar">
        <Tabs
          type="card"
          size="small"
          className="stab-tabs"
          items={items}
          activeKey={activeId ?? undefined}
          onChange={onActive}
          hideAdd
        />
        {active && (
          <span className="stab-ops">
            <Segmented
              size="small"
              value={active.mode}
              onChange={(v) => onModeChange(active.id, v as 'view' | 'edit')}
              options={[
                { value: 'view', label: <span className="stab-mode"><Eye size={12} strokeWidth={2} aria-hidden="true" />{zh ? '查看' : 'View'}</span> },
                { value: 'edit', label: <span className="stab-mode"><Pencil size={12} strokeWidth={2} aria-hidden="true" />{zh ? '编辑' : 'Edit'}</span> },
              ]}
            />
            <Tooltip title={zh ? '新窗口打开' : 'Open in new window'}>
              <Button size="small" type="text" aria-label={zh ? '新窗口打开' : 'Open in new window'} onClick={() => window.open(`/view/${active.id}`, '_blank', 'noopener')}>
                <ExternalLink size={13} strokeWidth={2} aria-hidden="true" />
              </Button>
            </Tooltip>
          </span>
        )}
      </div>
      {tabs.length === 0 && <div className="stab-empty muted">{zh ? '点击左侧文件打开' : 'Click a file in the tree to open'}</div>}
    </section>
  )
}
