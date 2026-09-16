// ONLYOFFICE 在线编辑页（/edit/:fileId）：
// - GET /onlyoffice/config 探测集成，取 server_url 动态注入
//   {server_url}/web-apps/apps/api/documents/api.js（卸载时移除 script）；
// - POST /onlyoffice/session 取 DocEditor 配置（后端已整体 JWT 签名），
//   追加 events 后 new window.DocsAPI.DocEditor(placeholder, config)；
// - onDocumentStateChange/onChange 后 debounce 提示「已保存为新版本」——回调
//   落版本无服务端推送，故提供手动「刷新版本」；并写 localStorage 标记，
//   其他窗口经 storage 事件感知后自动刷新（跨标签页的简单实现）。
// - 销毁时调用 docEditor.destroyEditor()；脚本加载失败提示「编辑服务不可用」。
import { useEffect, useRef, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import {
  ApiError,
  FileWithVersion,
  OnlyOfficeEditorConfig,
  createOnlyOfficeSession,
  getFileMeta,
  onlyOfficeStatus,
} from '../api'
import { useLocale } from '../i18n'

/** DocsAPI.DocEditor 实例（仅用到的 destroyEditor）。 */
interface DocEditorInstance {
  destroyEditor: () => void
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
 * 动态注入 DocumentServer 的 api.js；同一 src 复用已在加载/已加载的 script
 * （window.DocsAPI 已存在时直接就绪）。返回移除函数（卸载时调用）。
 */
function loadDocEditorScript(serverUrl: string): { ready: Promise<void>; remove: () => void } {
  const src = `${serverUrl.replace(/\/+$/, '')}/web-apps/apps/api/documents/api.js`
  const existing = document.querySelector<HTMLScriptElement>(`script[data-docflow-onlyoffice="${src}"]`)
  if (window.DocsAPI?.DocEditor) {
    return { ready: Promise.resolve(), remove: () => {} }
  }
  if (existing) {
    return {
      ready: new Promise((resolve, reject) => {
        existing.addEventListener('load', () => resolve(), { once: true })
        existing.addEventListener('error', () => reject(new Error('编辑服务不可用')), { once: true })
      }),
      remove: () => existing.remove(),
    }
  }
  const script = document.createElement('script')
  script.src = src
  script.async = true
  script.dataset.docflowOnlyoffice = src
  const ready = new Promise<void>((resolve, reject) => {
    script.onload = () => resolve()
    script.onerror = () => reject(new ApiError(0, '编辑服务不可用'))
  })
  document.head.appendChild(script)
  return { ready, remove: () => script.remove() }
}

export default function EditorPage() {
  const { fileId = '' } = useParams()
  // ?mode=view 强制只读会话（在线预览入口）；编辑器语言跟随界面语言。
  const [searchParams] = useSearchParams()
  const viewMode = searchParams.get('mode') === 'view'
  const locale = useLocale()

  const [file, setFile] = useState<FileWithVersion | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [saveHint, setSaveHint] = useState('')

  const placeholderId = useRef(`onlyoffice-placeholder-${Math.random().toString(36).slice(2)}`)
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
            mode: viewMode ? 'view' : undefined,
            lang: locale,
          }),
          getFileMeta(fileId).catch(() => null),
        ])
        if (!alive) return
        setFile(meta)
        const { ready, remove } = loadDocEditorScript(status.server_url)
        removeFns.push(remove)
        await ready
        if (!alive || !window.DocsAPI?.DocEditor) {
          if (alive) setError('编辑服务不可用')
          return
        }
        const editorConfig: OnlyOfficeEditorConfig = {
          ...config,
          width: '100%',
          height: '100%',
          events: {
            onDocumentStateChange: () => scheduleSaveHint(),
            onChange: () => scheduleSaveHint(),
          },
        }
        editorRef.current = window.DocsAPI.DocEditor(placeholderId.current, editorConfig)
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
      editorRef.current?.destroyEditor()
      editorRef.current = null
      for (const fn of removeFns) fn()
    }
  }, [fileId, viewMode, locale])

  const versionNo = file?.current_version?.version

  return (
    <div className="editor-page">
      <div className="editor-head">
        <Link className="btn ghost small" to="/">← 返回</Link>
        <h2 className="editor-title">{file?.name ?? '加载中…'}</h2>
        {versionNo !== undefined && <span className="badge current">当前版本 v{versionNo}</span>}
        <button className="btn small" onClick={() => void refreshVersion('版本已刷新')}>刷新版本</button>
      </div>

      {saveHint && <div className="banner ok editor-hint">{saveHint}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && !error && <div className="hint">{viewMode ? '正在加载查看器…' : '正在加载编辑器…'}</div>}

      {/* DocEditor 挂载容器：未进入错误态时始终渲染，保证 placeholder 存在。 */}
      {!error && <div className="editor-shell"><div id={placeholderId.current} className="editor-placeholder" /></div>}
    </div>
  )
}
