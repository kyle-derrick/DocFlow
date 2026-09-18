// draw.io 图表编辑页（/drawio/:fileId）：
// - GET /drawio/config 探测集成，取 url 以 iframe embed 模式加载编辑器
//   （URL 参数 embed=1&proto=json&spin=1&saveAndExit=1&noSaveBtn=0&libraries=1）；
// - postMessage JSON 协议：receive {event:"init"} → send {action:"load",
//   xml, autosave:0}；receive {event:"save", xml}（或 export）→ 经
//   uploadFileVersion 把导出 XML 作为新版本上传（覆盖链路，服务端忽略
//   name/parent_id）→ banner 提示；保存后不销毁编辑器（继续编辑）；
// - 文件内容经 fetchFileText 认证下载；读取失败/空内容回退初始模板
//   （EMPTY_DRAWIO_XML，404/无版本等容错）；
// - 页面卸载移除 message 监听；保存失败置未保存标记（beforeunload 提示
//   兜底，常规未保存保护依赖 drawio 的「保存并退出」按钮——协议本身不
//   通知父页脏态，autosave:0 亦不产生自动保存事件）。
// - 集成禁用或探测失败显示「图表服务不可用」。
import { useEffect, useRef, useState } from 'react'
import { Button } from 'antd'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import {
  EMPTY_DRAWIO_XML,
  FileWithVersion,
  drawioStatus,
  fetchFileText,
  getFileMeta,
  uploadFileVersion,
} from '../api'
import DrawioViewer from '../components/DrawioViewer'
import { useLocale } from '../i18n'
import { useColorMode } from '../theme'
import { closeEditorWithFallback, safeReturnTo } from '../editorNavigation'

/**
 * draw.io embed 编辑器/查看器 iframe URL（proto=json postMessage 协议）。
 * lang 跟随界面语言（drawio 资源名 zh/en）；ui 跟随站点明暗（浅色 min /
 * 深色 dark，编辑器界面主题与站点一致，避免深色站点里白闪编辑器）；
 * view=true 为只读查看器（viewer=1，隐藏编辑工具与保存），否则编辑模式
 * （保存并退出按钮）。
 */
function drawioEditorURL(base: string, lang: string, view: boolean, dark: boolean): string {
  const trimmed = base.replace(/\/+$/, '')
  const params = new URLSearchParams({
    embed: '1',
    proto: 'json',
    spin: '1',
    libraries: '1',
    lang,
    ui: dark ? 'dark' : 'min',
  })
  if (view) params.set('viewer', '1')
  else {
    params.set('saveAndExit', '1')
    params.set('noSaveBtn', '0')
  }
  return `${trimmed}/?${params.toString()}`
}

/** draw.io postMessage JSON 协议消息（仅用到的事件/字段；exit 标记保存并退出）。 */
interface DrawioMessage {
  event?: string
  action?: string
  xml?: string
  exit?: boolean
}

export default function DrawioPage({ mode, fileId: fileIdProp }: { mode?: 'edit' | 'view'; fileId?: string } = {}) {
  const { fileId: routeFileId = '' } = useParams()
  // by-path 路由经 prop 传入 resolve 得到的 file_id；缺省回退路由参数。
  const fileId = fileIdProp ?? routeFileId
  // 独立 /view 路由或 ?mode=view 均强制只读；编辑器语言跟随界面语言。
  const [searchParams] = useSearchParams()
  const viewMode = mode === 'view' || searchParams.get('mode') === 'view'
  const returnTo = safeReturnTo(searchParams.get('returnTo'))
  const locale = useLocale()
  const navigate = useNavigate()
  // 只读渲染跟随站点明暗主题与界面语言（zh-CN → drawio 中文资源）。
  const colorMode = useColorMode()
  const viewerLang = locale === 'zh-CN' ? 'zh' : ''

  const [file, setFile] = useState<FileWithVersion | null>(null)
  const [editorURL, setEditorURL] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [saving, setSaving] = useState(false)

  const frameRef = useRef<HTMLIFrameElement | null>(null)
  // 初始图表 XML（init 事件时发给编辑器）、未保存标记（保存失败兜底提示）
  // 与保存中标记（message 监听闭包防并发保存用，避免 state 闭包陈旧）。
  const xmlRef = useRef('')
  const dirtyRef = useRef(false)
  const savingRef = useRef(false)
  const savePromiseRef = useRef<Promise<boolean> | null>(null)
  const exitPendingRef = useRef(false)

  // 探测集成 → 拉取文件元数据与内容 → 挂 iframe。内容读取失败或为空
  // （含新建模板上传后立即打开）回退初始模板，不阻塞编辑。
  useEffect(() => {
    let alive = true
    const init = async () => {
      setLoading(true)
      setError('')
      setNotice('')
      try {
        const status = await drawioStatus()
        if (!alive) return
        if (!status.enabled || !status.url) {
          setError('图表服务不可用（draw.io 集成已关闭或无法访问）')
          return
        }
        const [meta, text] = await Promise.all([
          getFileMeta(fileId).catch(() => null),
          fetchFileText(fileId).catch(() => ''),
        ])
        if (!alive) return
        setFile(meta)
        xmlRef.current = text.trim() ? text : EMPTY_DRAWIO_XML
        // ui 主题取当前明暗（效应不依赖 colorMode，切换主题不重载编辑器 iframe，
        // 避免编辑中途丢失未保存内容；下次进入生效）。
        setEditorURL(viewMode ? status.url.replace(/\/+$/, '') : drawioEditorURL(status.url, locale === 'zh-CN' ? 'zh' : 'en', false, colorMode === 'dark'))
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : '图表编辑器加载失败')
      } finally {
        if (alive) setLoading(false)
      }
    }
    void init()
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId, viewMode, locale])

  // 退出编辑器：显式返回打开时记录的文件页，不依赖 noopener 新窗口中不可靠的
  // history。脚本可关闭由 window.open 创建的窗口；普通标签页则确定性导航。
  const closeEditor = () => closeEditorWithFallback(navigate, returnTo)

  // postMessage JSON 协议：init → load；save/export → 覆盖为新版本
  // （exit 标记或 exit 事件时保存成功后退出编辑器）。
  useEffect(() => {
    if (!editorURL || viewMode) return
    const onMessage = (e: MessageEvent) => {
      const frame = frameRef.current
      if (!frame || e.source !== frame.contentWindow) return
      let msg: DrawioMessage
      try {
        msg = JSON.parse(String(e.data)) as DrawioMessage
      } catch {
        return // 非 JSON 协议消息忽略
      }
      if (msg.event === 'init') {
        frame.contentWindow?.postMessage(JSON.stringify({ action: 'load', xml: xmlRef.current, autosave: 0 }), '*')
        return
      }
      if (msg.event === 'exit') {
        if (savePromiseRef.current) exitPendingRef.current = true
        else closeEditor()
        return
      }
      if (msg.event === 'save' || msg.event === 'export') {
        if (msg.xml) {
          exitPendingRef.current = msg.exit === true
          const pending = saveDiagram(msg.xml)
          savePromiseRef.current = pending
          void pending.then((ok) => {
            if (ok && exitPendingRef.current) closeEditor()
          }).finally(() => {
            if (savePromiseRef.current === pending) savePromiseRef.current = null
          })
        } else if (msg.exit) {
          closeEditor()
        }
      }
    }
    window.addEventListener('message', onMessage)
    return () => window.removeEventListener('message', onMessage)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editorURL, fileId, file?.name, viewMode])

  // 保存：导出 XML 作为新版本上传（file_id 会话沿用目标文件名/父目录），
  // 成功后刷新元数据展示新版本号；失败置未保存标记。返回是否保存成功。
  const saveDiagram = async (xml: string): Promise<boolean> => {
    if (savingRef.current) return savePromiseRef.current ?? false
    savingRef.current = true
    setSaving(true)
    setError('')
    try {
      const blob = new File([xml], file?.name ?? 'diagram.drawio', { type: 'text/xml' })
      await uploadFileVersion(blob, fileId, () => {})
      dirtyRef.current = false
      setNotice('已保存为新版本')
      try {
        setFile(await getFileMeta(fileId))
      } catch {
        // 元数据刷新失败不影响保存结果展示
      }
      return true
    } catch (err) {
      dirtyRef.current = true
      setError(err instanceof Error ? err.message : '保存失败')
      return false
    } finally {
      savingRef.current = false
      setSaving(false)
    }
  }

  // 未保存兜底提示：仅保存失败时拦截（协议不通知父页常规脏态，
  // 常规未保存保护依赖 drawio 的「保存并退出」按钮）。
  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => window.removeEventListener('beforeunload', onBeforeUnload)
  }, [])

  const versionNo = file?.current_version?.version

  return (
    <div className={`editor-page${viewMode ? ' viewer-only' : ''}`}>
      {!viewMode && <div className="editor-head">
        <Button type="text" size="small" onClick={closeEditor}>← 返回</Button>
        <h2 className="editor-title">{file?.name ?? '加载中…'}</h2>
        {versionNo !== undefined && <span className="badge current">当前版本 v{versionNo}</span>}
        {saving && <span className="badge uploading">保存中…</span>}
      </div>}

      {!viewMode && notice && <div className="banner ok editor-hint">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && !error && <div className="hint">{viewMode ? '正在加载图表查看器…' : '正在加载图表编辑器…'}</div>}

      {!error && !loading && editorURL && (viewMode ? (
        <DrawioViewer
          baseURL={editorURL}
          xml={xmlRef.current}
          title={file?.name ?? '图表'}
          dark={colorMode === 'dark'}
          lang={viewerLang}
        />
      ) : (
        <div className="editor-shell">
          <iframe
            ref={frameRef}
            className="drawio-frame"
            src={editorURL}
            title={file?.name ?? '图表编辑器'}
          />
        </div>
      ))}
    </div>
  )
}
