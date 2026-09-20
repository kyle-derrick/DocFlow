// ONLYOFFICE 在线编辑页（/edit/:fileId）：
// - GET /onlyoffice/config 探测集成，取 server_url 动态注入
//   {server_url}/web-apps/apps/api/documents/api.js（卸载时移除 script）；
// - POST /onlyoffice/session 取 DocEditor 配置（后端已整体 JWT 签名），
//   追加 events 后 new window.DocsAPI.DocEditor(placeholder, config)；
// - onDocumentStateChange/onChange 后 debounce 提示「已保存为新版本」——回调
//   落版本无服务端推送，故提供手动「刷新版本」；并写 localStorage 标记，
//   其他窗口经 storage 事件感知后自动刷新（跨标签页的简单实现）。
// - v2.6「保存并退出」：executeMethod('Save') 触发 DocumentServer 立即
//   保存（callback 落版本）后返回文件页（?returnTo 优先）；「← 返回」在
//   有未保存修改（onDocumentStateChange data=true 未落盘）时二次确认。
// - 销毁时调用 docEditor.destroyEditor()；脚本加载失败提示「编辑服务不可用」。
import { useEffect, useRef, useState } from 'react'
import { App as AntdApp, Button } from 'antd'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import {
  ApiError,
  FileWithVersion,
  OnlyOfficeEditorConfig,
  createOnlyOfficeSession,
  getFileMeta,
  onlyOfficeStatus,
} from '../api'
import { useLocale } from '../i18n'
import { useColorMode } from '../theme'

/** DocsAPI.DocEditor 实例（destroyEditor + executeMethod('Save')）。 */
interface DocEditorInstance {
  destroyEditor: () => void
  executeMethod?: (name: string, ...args: unknown[]) => unknown
}

declare global {
  interface Window {
    DocsAPI?: { DocEditor: (placeholder: string, config: Record<string, unknown>) => DocEditorInstance }
  }
}

/** 保存提示 debounce 时长：编辑事件静止后提示「已保存为新版本」。 */
const SAVE_HINT_DEBOUNCE_MS = 3000

/** 跨标签页保存标记：本窗口保存后写入，其他窗口 storage 事件感知后刷新版本。 */
const savedMarkerKey = (fileId: string) => `docflow:onlyoffice-saved:${fileId}`

/**
 * 动态注入 DocumentServer 的 api.js（全局只增不删）：
 * - window.DocsAPI 一旦就绪即常驻——弹窗/组件卸载绝不移除 script（api.js
 *   移除后 DocsAPI 仍在，但其内部初始化状态与二次挂载的 placeholder 不再
 *   匹配，DocEditor 会异常回退 body 兜底 iframe，即「二次打开弹窗变成内嵌
 *   当前页面」的根因）；
 * - 同一 src 复用已在加载/已加载的 script（dataset 标记全局一次），加载
 *   失败（onerror）时才清理该 script 允许下次重试。
 */
export function loadDocEditorScript(serverUrl: string): Promise<void> {
  if (window.DocsAPI?.DocEditor) return Promise.resolve()
  const src = `${serverUrl.replace(/\/+$/, '')}/web-apps/apps/api/documents/api.js`
  const existing = document.querySelector<HTMLScriptElement>(`script[data-docflow-onlyoffice="${src}"]`)
  if (existing) {
    return new Promise((resolve, reject) => {
      existing.addEventListener('load', () => resolve(), { once: true })
      existing.addEventListener('error', () => reject(new ApiError(0, '编辑服务不可用')), { once: true })
    })
  }
  return new Promise((resolve, reject) => {
    const script = document.createElement('script')
    script.src = src
    script.async = true
    script.dataset.docflowOnlyoffice = src
    script.onload = () => resolve()
    script.onerror = () => {
      script.remove()
      reject(new ApiError(0, '编辑服务不可用'))
    }
    document.head.appendChild(script)
  })
}

export default function EditorPage({ mode, fileId: fileIdProp }: { mode?: 'edit' | 'view'; fileId?: string } = {}) {
  const navigate = useNavigate()
  const { fileId: routeFileId = '' } = useParams()
  // by-path 路由经 prop 传入 resolve 得到的 file_id；缺省回退路由参数。
  const fileId = fileIdProp ?? routeFileId
  // 独立 /view 路由或 ?mode=view 均强制只读会话；编辑器语言跟随界面语言。
  const [searchParams] = useSearchParams()
  const viewMode = mode === 'view' || searchParams.get('mode') === 'view'
  const locale = useLocale()
  const colorMode = useColorMode()

  const [file, setFile] = useState<FileWithVersion | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [saveHint, setSaveHint] = useState('')
  const { modal: antdModal } = AntdApp.useApp()

  // 未保存修改跟踪：onDocumentStateChange data=true（有修改待保存）置位、
  // data=false（保存完成）清除；「← 返回」时仍有未保存修改则二次确认。
  const dirtyRef = useRef(false)

  // DocEditor 挂载容器（shell）。placeholder 节点在每次 init 时以全新 id
  // 命令式创建（弹窗复用组件实例/主题变化等二次 init 时，旧 placeholder 已
  // 被 destroyEditor 清理，复用旧 id 会让 DocEditor 找不到挂载点）。
  const shellRef = useRef<HTMLDivElement | null>(null)
  const editorRef = useRef<DocEditorInstance | null>(null)

  // 手动/跨标签页刷新当前版本指针（current_version 摘要）。
  const refreshVersion = async (hint: string) => {
    try {
      const meta = await getFileMeta(fileId)
      setFile(meta)
      setSaveHint(hint)
    } catch {
      setSaveHint('版本刷新失败，请稍后重试')
    }
  }

  useEffect(() => {
    let alive = true
    let saveTimer = 0
    const removeFns: Array<() => void> = []

    // 编辑事件后 debounce：静置 SAVE_HINT_DEBOUNCE_MS 提示已保存并通知其他标签页。
    const scheduleSaveHint = () => {
      window.clearTimeout(saveTimer)
      saveTimer = window.setTimeout(() => {
        setSaveHint('已保存为新版本（可在「刷新版本」后查看最新历史）')
        try {
          localStorage.setItem(savedMarkerKey(fileId), String(Date.now()))
        } catch {
          // 隐私模式等 localStorage 不可用时跳过跨标签页通知
        }
      }, SAVE_HINT_DEBOUNCE_MS)
    }

    // 其他标签页保存后（storage 事件）刷新本页版本展示。
    const onStorage = (e: StorageEvent) => {
      if (e.key === savedMarkerKey(fileId)) void refreshVersion('已在其他窗口保存，版本已更新')
    }
    window.addEventListener('storage', onStorage)
    removeFns.push(() => window.removeEventListener('storage', onStorage))

    const init = async () => {
      setLoading(true)
      setError('')
      try {
        const status = await onlyOfficeStatus()
        if (!alive) return
        if (!status.enabled || !status.server_url) {
          setError('在线编辑未启用（ONLYOFFICE 集成已关闭）')
          return
        }
        const [config, meta] = await Promise.all([
          createOnlyOfficeSession(fileId, {
            mode: viewMode ? 'view' : 'edit',
            lang: locale,
          }),
          getFileMeta(fileId).catch(() => null),
        ])
        if (!alive) return
        setFile(meta)
        await loadDocEditorScript(status.server_url)
        if (!alive || !window.DocsAPI?.DocEditor) {
          if (alive) setError('编辑服务不可用')
          return
        }
        const editorConfig: OnlyOfficeEditorConfig = {
          ...config,
          width: '100%',
          height: '100%',
          // 编辑器 UI 明暗跟随站点主题（DS 8.x 默认跟随系统，浏览器深色
          // 模式下会把浅色站点里的编辑器渲染成深色，与站点观感割裂）。
          customization: {
            ...((config.customization as Record<string, unknown> | undefined) ?? {}),
            uiTheme: colorMode === 'dark' ? 'theme-dark' : 'theme-classic-light',
          },
          events: {
            // data=true：文档有未保存修改（正在保存）；data=false：保存完成。
            onDocumentStateChange: (e: { data?: boolean } | undefined) => {
              dirtyRef.current = e?.data === true
              scheduleSaveHint()
            },
            onChange: () => scheduleSaveHint(),
          },
        }
        // 全新 id 的 placeholder 节点（init 与 DOM 就绪在同一帧后完成）。
        const holder = document.createElement('div')
        const holderId = `onlyoffice-placeholder-${Math.random().toString(36).slice(2)}`
        holder.id = holderId
        holder.className = 'editor-placeholder'
        if (!shellRef.current) return
        shellRef.current.replaceChildren(holder)
        editorRef.current = window.DocsAPI.DocEditor(holderId, editorConfig)
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : '编辑器加载失败')
      } finally {
        if (alive) setLoading(false)
      }
    }
    void init()

    return () => {
      alive = false
      window.clearTimeout(saveTimer)
      // destroy 只在 cleanup（卸载或依赖变化重初始化）时执行：编辑器实例
      // 与 placeholder 同生共死，script 常驻不动。
      editorRef.current?.destroyEditor()
      editorRef.current = null
      shellRef.current?.replaceChildren()
      for (const fn of removeFns) fn()
    }
  }, [fileId, viewMode, locale, colorMode])

  const versionNo = file?.current_version?.version

  // 返回目标：编辑入口 URL 的 ?returnTo（文件页带目录/空间上下文），缺省 /。
  const returnTo = (() => {
    const r = searchParams.get('returnTo')
    if (!r || !r.startsWith('/') || r.startsWith('//')) return '/'
    return r
  })()

  /** 保存并退出：OnlyOffice callback 模式下编辑本会自动保存，此处显式
   *  executeMethod('Save') 触发立即保存（DocumentServer 收到命令后回调
   *  落版本），提示后返回文件页。 */
  const saveAndExit = () => {
    try {
      editorRef.current?.executeMethod?.('Save')
    } catch {
      /* 旧版 DS 不支持命令服务：自动保存仍在工作，不阻塞返回。 */
    }
    dirtyRef.current = false
    setSaveHint('已触发保存，正在返回…')
    navigate(returnTo, { replace: true })
  }

  /** 返回（退出）：仍有未保存修改（onDocumentStateChange data=true 未
   *  落盘）时二次确认；OnlyOffice 自动保存间隔内的窗口很短。 */
  const exitWithConfirm = () => {
    if (!dirtyRef.current) {
      navigate(returnTo, { replace: true })
      return
    }
    antdModal.confirm({
      title: '有未保存的修改',
      content: '文档存在尚未保存到服务器的修改，直接退出可能丢失。仍要退出吗？（编辑器通常会在数秒内自动保存）',
      okText: '仍然退出',
      okButtonProps: { danger: true },
      cancelText: '继续编辑',
      onOk: () => navigate(returnTo, { replace: true }),
    })
  }

  return (
    <div className={`editor-page${viewMode ? ' viewer-only' : ''}`}>
      {!viewMode && <div className="editor-head">
        <Button type="text" size="small" onClick={exitWithConfirm}>← 返回</Button>
        <h2 className="editor-title">{file?.name ?? '加载中…'}</h2>
        {versionNo !== undefined && <span className="badge current">当前版本 v{versionNo}</span>}
        <Button size="small" onClick={() => void refreshVersion('版本已刷新')}>刷新版本</Button>
        {/* 保存并退出（v2.6）：触发 DS 立即保存 + 返回文件页。 */}
        <Button size="small" type="primary" onClick={saveAndExit}>保存并退出</Button>
      </div>}

      {!viewMode && saveHint && <div className="banner ok editor-hint">{saveHint}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && !error && <div className="hint">{viewMode ? '正在加载查看器…' : '正在加载编辑器…'}</div>}

      {/* DocEditor 挂载容器：未进入错误态时始终渲染，placeholder 由 effect
          内命令式创建（每次 init 全新 id）。 */}
      {!error && <div className="editor-shell" ref={shellRef} />}
    </div>
  )
}
