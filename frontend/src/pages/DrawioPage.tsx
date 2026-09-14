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
import { Link, useParams } from 'react-router-dom'
import {
  EMPTY_DRAWIO_XML,
  FileWithVersion,
  drawioStatus,
  fetchFileText,
  getFileMeta,
  uploadFileVersion,
} from '../api'

/** draw.io embed 编辑器 iframe URL（proto=json postMessage 协议）。 */
function drawioEditorURL(base: string): string {
  const trimmed = base.replace(/\/+$/, '')
  return `${trimmed}/?embed=1&proto=json&spin=1&saveAndExit=1&noSaveBtn=0&libraries=1`
}

/** draw.io postMessage JSON 协议消息（仅用到的事件/字段）。 */
interface DrawioMessage {
  event?: string
  action?: string
  xml?: string
}

export default function DrawioPage() {
  const { fileId = '' } = useParams()

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
        setEditorURL(drawioEditorURL(status.url))
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
  }, [fileId])

  // postMessage JSON 协议：init → load；save/export → 覆盖为新版本。
  useEffect(() => {
    if (!editorURL) return
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
      if (msg.event === 'save' || msg.event === 'export') {
        if (msg.xml) void saveDiagram(msg.xml)
      }
    }
    window.addEventListener('message', onMessage)
    return () => window.removeEventListener('message', onMessage)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editorURL, fileId, file?.name])

  // 保存：导出 XML 作为新版本上传（file_id 会话沿用目标文件名/父目录），
  // 成功后刷新元数据展示新版本号；失败置未保存标记。
  const saveDiagram = async (xml: string) => {
    if (savingRef.current) return
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
    } catch (err) {
      dirtyRef.current = true
      setError(err instanceof Error ? err.message : '保存失败')
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
    <div className="editor-page">
      <div className="editor-head">
        <Link className="btn ghost small" to="/">← 返回</Link>
        <h2 className="editor-title">{file?.name ?? '加载中…'}</h2>
        {versionNo !== undefined && <span className="badge current">当前版本 v{versionNo}</span>}
        {saving && <span className="badge uploading">保存中…</span>}
      </div>

      {notice && <div className="banner ok editor-hint">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && !error && <div className="hint">正在加载图表编辑器…</div>}

      {/* iframe 编辑器：未进入错误态且地址就绪后渲染（init 事件由监听器应答）。 */}
      {!error && !loading && editorURL && (
        <div className="editor-shell">
          <iframe
            ref={frameRef}
            className="drawio-frame"
            src={editorURL}
            title={file?.name ?? '图表编辑器'}
          />
        </div>
      )}
    </div>
  )
}
