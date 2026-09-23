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
import { App as AntdApp, Button } from 'antd'
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
import AIDrawio from '../components/AIDrawio'
import { EditorLoadError } from '../components/EditorLoadError'
import type { AIEditTarget } from '../components/AIEdit'
import AIEditChat, { AIEditChatButton } from '../components/AIEditChat'
import type { AIQuickCommand } from '../components/AIEditChat'
import { useLocale } from '../i18n'
import { useColorMode } from '../theme'
import { closeEditorWithFallback, safeReturnTo } from '../editorNavigation'

/**
 * draw.io embed 编辑器/查看器 iframe URL（proto=json postMessage 协议）。
 * lang 跟随界面语言（drawio 资源名 zh/en）；ui 跟随站点明暗（浅色 min /
 * 深色 dark，编辑器界面主题与站点一致，避免深色站点里白闪编辑器）；
 * view=true 为只读查看器（viewer=1，隐藏编辑工具与保存），否则编辑模式。
 * v2.7：不再传 saveAndExit=1——drawio 的「保存并退出」按钮与 Ctrl+S 均只
 * 触发 save 事件（保存不退出），退出统一走页面「← 返回」（带 dirty 确认）。
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

/** draw.io iframe 初始化等待上限：超时仍未收到 init 事件（编辑器静态资源
 * 不可达/服务端配置了浏览器不可达的地址）即展示页面级错误卡。 */
const DRAWIO_INIT_TIMEOUT_MS = 20000

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
  /** 页面级加载失败（EditorLoadError 错误卡）：iframe 加载失败或初始化
   * 超时（超时仍未收到 init postMessage），展示图表服务地址与排查提示。 */
  const [frameError, setFrameError] = useState<string | null>(null)

  const frameRef = useRef<HTMLIFrameElement | null>(null)
  // 初始图表 XML（init 事件时发给编辑器）、未保存标记（保存失败兜底提示）
  // 与保存中标记（message 监听闭包防并发保存用，避免 state 闭包陈旧）。
  const xmlRef = useRef('')
  const dirtyRef = useRef(false)
  const savingRef = useRef(false)
  const savePromiseRef = useRef<Promise<boolean> | null>(null)
  // 自动保存 debounce（autosave 事件静置 10s 落版本；v2.7 自动保存间隔）。
  const autosaveTimerRef = useRef(0)
  // 「保存并退出」之外的退出流程抑制 beforeunload（返回确认走 antd 弹窗，
  // 不再叠加浏览器原生弹窗）。
  const closingRef = useRef(false)
  // AI 对话面板（AIEditChat）展开态（收起不清空会话）与头部下拉快捷指令。
  const [aiChatOpen, setAiChatOpen] = useState(false)
  const [aiQuick, setAiQuick] = useState<AIQuickCommand | null>(null)
  const { modal: antdModal } = AntdApp.useApp()

  // 探测集成 → 拉取文件元数据与内容 → 挂 iframe。内容读取失败或为空
  // （含新建模板上传后立即打开）回退初始模板，不阻塞编辑。
  useEffect(() => {
    let alive = true
    const init = async () => {
      setLoading(true)
      setError('')
      setNotice('')
      setFrameError(null)
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

  // postMessage JSON 协议：init → load（autosave:1 启用 autosave 事件，供
  // 父页跟踪脏态与静置自动保存）；save/export → 覆盖为新版本（保存后不退
  // 出——v2.7：Ctrl=S/保存按钮仅保存 + banner 提示，退出走「← 返回」带
  // dirty 确认）；autosave → 置脏 + 10s 静置自动落版本。
  useEffect(() => {
    if (!editorURL || viewMode) return
    // 初始化超时兜底（v2.7 反馈 8）：drawio 静态资源不可达时 iframe 不会
    // 发出 init 事件（跨域 iframe 加载失败多数也不触发 onerror），超时
    // 即展示页面级错误卡（替代无限 loading/白屏）。
    const initTimer = window.setTimeout(() => {
      setFrameError(locale === 'zh-CN'
        ? `图表编辑器初始化超时（${DRAWIO_INIT_TIMEOUT_MS / 1000} 秒内未就绪），图表服务可能不可达`
        : `Diagram editor failed to initialize within ${DRAWIO_INIT_TIMEOUT_MS / 1000}s; the diagram service may be unreachable`)
    }, DRAWIO_INIT_TIMEOUT_MS)
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
        window.clearTimeout(initTimer)
        frame.contentWindow?.postMessage(JSON.stringify({ action: 'load', xml: xmlRef.current, autosave: 1 }), '*')
        return
      }
      if (msg.event === 'exit') {
        closeEditor()
        return
      }
      if (msg.event === 'autosave') {
        if (msg.xml) {
          // 跟踪最新 XML（AI 上下文与 AI 应用前保存基线用）。
          xmlRef.current = msg.xml
          dirtyRef.current = true
          window.clearTimeout(autosaveTimerRef.current)
          autosaveTimerRef.current = window.setTimeout(() => {
            // 静置自动保存：静默落版本（不弹 banner），仍脏且无保存进行时。
            if (dirtyRef.current && !savingRef.current) void saveDiagram(msg.xml ?? '', true)
          }, 10000)
        }
        return
      }
      if (msg.event === 'save' || msg.event === 'export') {
        if (msg.xml) {
          window.clearTimeout(autosaveTimerRef.current)
          const pending = saveDiagram(msg.xml)
          savePromiseRef.current = pending
          void pending.finally(() => {
            if (savePromiseRef.current === pending) savePromiseRef.current = null
          })
        }
      }
    }
    window.addEventListener('message', onMessage)
    return () => {
      window.removeEventListener('message', onMessage)
      window.clearTimeout(initTimer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editorURL, fileId, file?.name, viewMode, locale])

  // 保存：导出 XML 作为新版本上传（file_id 会话沿用目标文件名/父目录），
  // 成功后刷新元数据展示新版本号；失败置未保存标记。返回是否保存成功。
  // silent=true 为静置自动保存（不弹「已保存为新版本」banner）。
  const saveDiagram = async (xml: string, silent = false): Promise<boolean> => {
    if (!xml) return false
    if (savingRef.current) return savePromiseRef.current ?? false
    savingRef.current = true
    setSaving(true)
    setError('')
    try {
      const blob = new File([xml], file?.name ?? 'diagram.drawio', { type: 'text/xml' })
      await uploadFileVersion(blob, fileId, () => {})
      dirtyRef.current = false
      if (!silent) setNotice('已保存为新版本')
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

  // 返回（退出）：有未落盘修改（autosave 事件置脏且静置窗口内未保存）时
  // 二次确认；与 OnlyOffice/白板/文本编辑页一致。
  const exitWithConfirm = () => {
    if (!dirtyRef.current) {
      closeEditor()
      return
    }
    antdModal.confirm({
      title: '有未保存的修改',
      content: '图表存在尚未保存到服务器的修改，直接退出可能丢失。仍要退出吗？（编辑静置 10 秒后会自动保存）',
      okText: '仍然退出',
      okButtonProps: { danger: true },
      cancelText: '继续编辑',
      onOk: () => {
        closingRef.current = true
        closeEditor()
      },
    })
  }

  // ---- 编辑器 AI（applyKind=drawio-xml）：AI 基于当前 XML 生成/修改完整
  //      drawio XML，经既有 postMessage 通道整体替换画布并落新版本 ----

  /** AI 上下文目标：图表无选区概念，恒全文（=当前最新已知 XML）。 */
  const aiGetTarget = (): AIEditTarget => ({ hasSelection: false, text: xmlRef.current })

  /** AI XML 自动应用（AIEditChat 已提取完整 drawio XML）：向 iframe 发
   * load 动作整体替换画布内容（与初始化装载同款消息）；load 本身不一定
   * 触发 autosave 事件，故随后主动发 export 请求——drawio 回 export 事件
   * 带最新 XML，走既有 saveDiagram 通道落新版本（导出→上传→版本号刷新）。
   * 返回 string=失败原因（AIEditChat 显示 applyError，画布未被修改）。 */
  const aiApply = (_mode: 'insert' | 'replace', xml: string): string | void => {
    const frame = frameRef.current?.contentWindow
    if (!frame) return '图表编辑器未就绪，未应用'
    frame.postMessage(JSON.stringify({ action: 'load', xml, autosave: 1 }), '*')
    dirtyRef.current = true
    frame.postMessage(JSON.stringify({ action: 'export', format: 'xml' }), '*')
  }

  /** 版本保护前置：应用前先把当前图表（最新已知 XML）保存为基线版本。 */
  const aiEnsureSaved = async (): Promise<{ versionId: string; version: number } | null> => {
    try {
      if (dirtyRef.current) {
        const ok = await saveDiagram(xmlRef.current, true)
        if (!ok) return null
      }
      const meta = await getFileMeta(fileId)
      return meta.current_version
        ? { versionId: meta.current_version.id, version: meta.current_version.version }
        : null
    } catch {
      return null
    }
  }

  /** 撤销回退后刷新画布：重新拉取文件内容，经 load 消息重载编辑器。 */
  const aiReload = async (): Promise<void> => {
    const text = await fetchFileText(fileId)
    xmlRef.current = text.trim() ? text : EMPTY_DRAWIO_XML
    dirtyRef.current = false
    frameRef.current?.contentWindow?.postMessage(
      JSON.stringify({ action: 'load', xml: xmlRef.current, autosave: 1 }),
      '*',
    )
    try {
      setFile(await getFileMeta(fileId))
    } catch {
      // 版本号刷新失败不影响回退结果
    }
  }

  /** 头部下拉快捷指令：打开面板并透传给 AIEditChat 自动执行。 */
  const openAiChatWith = (cmd: AIQuickCommand) => {
    setAiChatOpen(true)
    setAiQuick(cmd)
  }

  // 未保存兜底提示：仅保存失败/静置窗口内退出时拦截（dirty 由 autosave
  // 事件驱动）；退出确认流程（closingRef）不叠加浏览器原生弹窗。
  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!dirtyRef.current || closingRef.current) return
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => {
      window.removeEventListener('beforeunload', onBeforeUnload)
      window.clearTimeout(autosaveTimerRef.current)
    }
  }, [])

  const versionNo = file?.current_version?.version

  return (
    <div className={`editor-page${viewMode ? ' viewer-only' : ''}`}>
      {!viewMode && <div className="editor-head">
        <Button type="text" size="small" onClick={exitWithConfirm}>← 返回</Button>
        <h2 className="editor-title">{file?.name ?? '加载中…'}</h2>
        {versionNo !== undefined && <span className="badge current">当前版本 v{versionNo}</span>}
        {saving && <span className="badge uploading">保存中…</span>}
        {/* AI 生成（AI 能力第一版）：自然语言 → mermaid → 预览/复制/存 .mmd。 */}
        <AIDrawio fileId={fileId} />
        {/* AI 对话（applyKind=drawio-xml）：可修改模式下 AI 生成完整 drawio
            XML 自动替换画布并落新版本（版本保护可撤销）。 */}
        <AIEditChatButton open={aiChatOpen} onToggle={() => setAiChatOpen((v) => !v)} onQuick={openAiChatWith} kind="drawio-xml" disabled={loading || !!frameError} />
        {!viewMode && <span className="muted drawio-save-hint">Ctrl+S 保存（不退出）</span>}
      </div>}

      {!viewMode && notice && !frameError && <div className="banner ok editor-hint">{notice}</div>}
      {error && !frameError && <div className="banner error">{error}</div>}
      {loading && !error && !frameError && <div className="hint">{viewMode ? '正在加载图表查看器…' : '正在加载图表编辑器…'}</div>}

      {/* 页面级加载失败错误卡（v2.7 反馈 8）：图表服务地址不可达（如
          服务端返回 127.0.0.1 回环地址）或静态资源拉取失败时不再白屏。 */}
      {frameError && (
        <EditorLoadError
          message={frameError}
          resourceLabel={locale === 'zh-CN' ? '图表服务地址' : 'Diagram service URL'}
          resourceUrl={editorURL}
          hints={locale === 'zh-CN' ? [
            '该地址来自平台 draw.io 集成配置，必须是当前浏览器可达的地址。',
            '跨机/容器访问时请勿使用 127.0.0.1 等回环地址：请将 DRAWIO_PUBLIC_URL 配置为外部可达地址，或经反向代理域名访问。',
            'HTTPS 页面无法加载 HTTP 资源（混合内容拦截），请保持协议一致；也可刷新页面重试。',
          ] : [
            'This URL comes from the platform draw.io integration config and must be reachable from your browser.',
            'For cross-machine/container access, avoid 127.0.0.1 loopback addresses: configure DRAWIO_PUBLIC_URL to an externally reachable URL, or access via a reverse-proxy domain.',
            'An HTTPS page cannot load HTTP resources (mixed content); keep the protocol consistent, or retry by reloading.',
          ]}
          onRetry={() => window.location.reload()}
          retryText={locale === 'zh-CN' ? '刷新重试' : 'Reload'}
        />
      )}

      {!error && !frameError && !loading && editorURL && (viewMode ? (
        <DrawioViewer
          baseURL={editorURL}
          xml={xmlRef.current}
          title={file?.name ?? '图表'}
          dark={colorMode === 'dark'}
          lang={viewerLang}
        />
      ) : (
        <div className="editor-with-ai">
          <div className="editor-shell">
            <iframe
              ref={frameRef}
              className="drawio-frame"
              src={editorURL}
              title={file?.name ?? '图表编辑器'}
              onError={() => {
                // iframe 加载失败（部分浏览器对无效 src 触发；多数不可达场景
                // 由上方 init 超时兜底覆盖）。
                setFrameError(locale === 'zh-CN'
                  ? '图表编辑器 iframe 加载失败，图表服务不可达'
                  : 'Failed to load the diagram editor iframe; the diagram service is unreachable')
              }}
            />
          </div>
          {/* AI 对话式创作面板（drawio XML 整体替换；应用前 aiEnsureSaved 保存
              基线版本，撤销回退后 aiReload 重载画布）。 */}
          <AIEditChat
            open={aiChatOpen}
            onClose={() => setAiChatOpen(false)}
            getTarget={aiGetTarget}
            getAllText={() => xmlRef.current}
            onApply={aiApply}
            fileId={fileId}
            ensureSaved={aiEnsureSaved}
            reload={aiReload}
            quickCommand={aiQuick}
            onQuickConsumed={() => setAiQuick(null)}
            applyKind="drawio-xml"
          />
        </div>
      ))}
    </div>
  )
}
