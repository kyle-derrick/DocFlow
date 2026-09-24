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
  convertMarkdown,
  createOnlyOfficeSession,
  fetchFileText,
  getFileMeta,
  onlyOfficeStatus,
} from '../api'
import { useLocale } from '../i18n'
import { useColorMode } from '../theme'
import { setAIContextFile } from '../components/AIAssistant'
import { EditorLoadError } from '../components/EditorLoadError'
import AIEditChat, { AIEditChatButton } from '../components/AIEditChat'
import type { AIQuickCommand } from '../components/AIEditChat'

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

/** DocEditor 初始化超时：超时仍未收到 onDocumentReady（渲染就绪）即判定
 * Document Server 不可达/初始化异常，展示页面级错误卡（替代白屏挂起）。 */
const DOC_EDITOR_READY_TIMEOUT_MS = 15000

/** 跨标签页保存标记：本窗口保存后写入，其他窗口 storage 事件感知后刷新版本。 */
const savedMarkerKey = (fileId: string) => `docflow:onlyoffice-saved:${fileId}`

/** DocFlow AI OnlyOffice 插件 guid（与 frontend/public/onlyoffice-plugins/
 * docflow-ai/config.json 一致）：插件面板常驻监听宿主 postMessage，把 AI
 * 面板回复经官方插件 API（executeMethod PasteText/PasteHtml）插入编辑器
 * 光标处（有选区时替换选区）。 */
const DOCFLOW_AI_PLUGIN_GUID = 'asc.{B7A5D2E4-9C3F-4E8A-8B6D-1F2A4C5E6D80}'

/** 插件清单 URL：前端站点同源静态资源（构建期 public/ 拷入产物，Caddy
 * root 直接服务）。编辑器 fetch 该 config.json 并按 variations[].url 打开
 * 插件 iframe——同域反代部署（/onlyoffice/* 与主站同 origin）无 CORS 问题。 */
const docflowAIPluginConfigURL = () =>
  new URL('onlyoffice-plugins/docflow-ai/config.json', window.location.origin + '/').href

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
  /** 页面级加载失败（EditorLoadError 错误卡）：api.js 拉取失败 / DocsAPI
   * 缺失 / 初始化异常 / 15s 渲染超时；message+serverUrl 随 locale 生成。 */
  const [loadError, setLoadError] = useState<{ message: string; url: string } | null>(null)
  const [saveHint, setSaveHint] = useState('')
  const { modal: antdModal, message: antdMessage } = AntdApp.useApp()
  // AI 对话面板（AIEditChat，仅对话模式）：展开态与头部下拉快捷指令。
  const [aiChatOpen, setAiChatOpen] = useState(false)
  const [aiQuick, setAiQuick] = useState<AIQuickCommand | null>(null)
  // 文档文本上下文（仅对话面板用）：面板首次打开时经 convertMarkdown 转换
  // 端点获取并缓存一次（开源版 OnlyOffice 无内容修改/读取 API；失败或格式
  // 不支持时保持空串=无上下文对话）。
  const docTextRef = useRef<string | null>(null)

  // 未保存修改跟踪：onDocumentStateChange data=true（有修改待保存）置位、
  // data=false（保存完成）清除；「← 返回」时仍有未保存修改则二次确认。
  const dirtyRef = useRef(false)

  // DocEditor 渲染就绪标记（onDocumentReady 置位；超时判定用）。
  const readyRef = useRef(false)

  // ---- DocFlow AI 插件桥（编辑模式）：AI 面板回复 → 插件 → 文档 ----
  // 插件就绪（插件面板 iframe 加载并 postMessage 上报后置 true；面板关闭
  // 后引用悬空，靠插入回执超时兜底提示）。
  const [pluginReady, setPluginReady] = useState(false)
  // 上报 ready 的插件 iframe window（postMessage 目标）。
  const pluginWinRef = useRef<MessageEventSource | null>(null)
  // 本会话文档 fileType（word 类→插入走 PasteHtml 富文本；cell/slide 纯文本）。
  const fileTypeRef = useRef('')
  // 在途插入指令的 nonce 与回执超时定时器（区分回执、面板失联提示）。
  const insertNonceRef = useRef<number | null>(null)
  const insertTimerRef = useRef(0)

  useEffect(() => {
    const onMessage = (e: MessageEvent) => {
      // 插件 iframe 与宿主页同源（同域反代部署），严格校验 origin。
      if (e.origin !== window.location.origin) return
      const d = e.data as { type?: string; ok?: boolean; error?: string | null; nonce?: number | null } | null
      if (!d || typeof d !== 'object') return
      if (d.type === 'docflow-ai-plugin-ready') {
        pluginWinRef.current = e.source
        setPluginReady(true)
      } else if (d.type === 'docflow-ai-insert-result' && d.nonce != null && d.nonce === insertNonceRef.current) {
        window.clearTimeout(insertTimerRef.current)
        insertNonceRef.current = null
        if (d.ok) antdMessage.success('已插入文档（内容落在编辑器光标处；有选区时替换了选区）')
        else antdMessage.error(`插件插入失败：${d.error ?? '未知错误'}`)
      }
    }
    window.addEventListener('message', onMessage)
    return () => {
      window.removeEventListener('message', onMessage)
      window.clearTimeout(insertTimerRef.current)
    }
  }, [antdMessage])

  /** 把 AI 回复插入文档：word 类文档先经 marked 转 HTML（保留富文本样式，
   * PasteHtml），其余格式纯文本（PasteText）；postMessage 给插件 iframe，
   * 3s 未收到回执提示检查插件面板状态。 */
  const insertToDocument = (text: string) => {
    const win = pluginWinRef.current
    if (!win || !pluginReady) {
      void antdMessage.warning('DocFlow AI 插件未就绪：请点击编辑器左侧工具栏的插件图标，打开「DocFlow AI」面板后重试')
      return
    }
    void (async () => {
      let html: string | undefined
      const ft = fileTypeRef.current
      if (ft === 'docx' || ft === 'doc' || ft === 'odt' || ft === 'rtf') {
        try {
          const { marked } = await import('marked')
          html = String(marked.parse(text, { async: false, gfm: true, breaks: true }))
        } catch {
          html = undefined
        }
      }
      const nonce = Date.now()
      insertNonceRef.current = nonce
      window.clearTimeout(insertTimerRef.current)
      insertTimerRef.current = window.setTimeout(() => {
        if (insertNonceRef.current === nonce) {
          insertNonceRef.current = null
          void antdMessage.warning('插件未响应：请确认编辑器左侧「DocFlow AI」插件面板仍处于打开状态，必要时重新打开后重试')
        }
      }, 3000)
      try {
        // MessageEventSource 含 MessagePort 等无 targetOrigin 的成员，插件
        // iframe 恒为 Window，cast 后使用窗口版 postMessage。
        ;(win as Window).postMessage({ type: 'docflow-ai-insert', text, html: html ?? null, nonce }, window.location.origin)
      } catch {
        window.clearTimeout(insertTimerRef.current)
        if (insertNonceRef.current === nonce) insertNonceRef.current = null
        void antdMessage.error('发送到插件失败：请重新打开「DocFlow AI」插件面板后重试')
      }
    })()
  }

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
    // 初始化超时定时器（onDocumentReady 解除；cleanup/重初始化时清除）。
    let readyTimer = 0
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
      setLoadError(null)
      try {
        const status = await onlyOfficeStatus()
        if (!alive) return
        if (!status.enabled || !status.server_url) {
          setError('在线编辑未启用（ONLYOFFICE 集成已关闭）')
          return
        }
        const serverUrl = status.server_url
        const failLoad = (message: string) => {
          if (alive) setLoadError({ message, url: serverUrl })
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
        try {
          await loadDocEditorScript(serverUrl)
        } catch {
          failLoad(locale === 'zh-CN'
            ? 'OnlyOffice 编辑服务脚本（api.js）加载失败，Document Server 不可达'
            : 'Failed to load the OnlyOffice Document Server script (api.js); server unreachable')
          return
        }
        if (!alive) return
        if (!window.DocsAPI?.DocEditor) {
          failLoad(locale === 'zh-CN'
            ? 'OnlyOffice DocsAPI 未初始化（脚本加载异常）'
            : 'OnlyOffice DocsAPI is unavailable (script loaded but API missing)')
          return
        }
        // 本会话文件类型（AI 插入通道选择 PasteHtml/PasteText 用）。
        fileTypeRef.current = String(
          ((config.document as Record<string, unknown> | undefined)?.fileType as string | undefined) ?? '',
        )
        // 重初始化（主题/语言切换等）会重建编辑器与插件 iframe，重置就绪
        // 态等待新的 ready 上报。
        setPluginReady(false)
        pluginWinRef.current = null
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
          // DocFlow AI 插件（编辑会话）：pluginsData 指向同源静态清单，DS
          // 据此加载插件面板（iframe）并 autostart 自动打开。plugins 为
          // 后端 JWT 载荷未覆盖的 UI 级字段（后端仅签 document/editorConfig
          // 关键载荷，前端追加与 customization/events 同机制生效）；DS
          // 8.2.3 若 autostart 未生效，可从编辑器「插件」菜单手动打开。
          ...(!viewMode && {
            editorConfig: {
              ...((config.editorConfig as Record<string, unknown> | undefined) ?? {}),
              plugins: {
                autostart: [DOCFLOW_AI_PLUGIN_GUID],
                pluginsData: [docflowAIPluginConfigURL()],
              },
            },
          }),
          events: {
            // data=true：文档有未保存修改（正在保存）；data=false：保存完成。
            onDocumentStateChange: (e: { data?: boolean } | undefined) => {
              dirtyRef.current = e?.data === true
              scheduleSaveHint()
            },
            onChange: () => scheduleSaveHint(),
            // 渲染就绪：解除 15s 初始化超时（DS 不可达时 api.js 可能加载
            // 成功但编辑器挂起不渲染，靠超时兜底展示错误卡）。
            onDocumentReady: () => {
              readyRef.current = true
              window.clearTimeout(readyTimer)
            },
          },
        }
        // 全新 id 的 placeholder 节点（init 与 DOM 就绪在同一帧后完成）。
        const holder = document.createElement('div')
        const holderId = `onlyoffice-placeholder-${Math.random().toString(36).slice(2)}`
        holder.id = holderId
        holder.className = 'editor-placeholder'
        if (!shellRef.current) return
        // 初始化超时兜底：超时仍未 onDocumentReady → 页面级错误卡。
        readyRef.current = false
        window.clearTimeout(readyTimer)
        readyTimer = window.setTimeout(() => {
          if (!alive || readyRef.current) return
          failLoad(locale === 'zh-CN'
            ? `编辑器初始化超时（${DOC_EDITOR_READY_TIMEOUT_MS / 1000} 秒内未完成渲染），Document Server 可能不可达`
            : `Editor initialization timed out (not rendered within ${DOC_EDITOR_READY_TIMEOUT_MS / 1000}s); Document Server may be unreachable`)
        }, DOC_EDITOR_READY_TIMEOUT_MS)
        shellRef.current.replaceChildren(holder)
        try {
          editorRef.current = window.DocsAPI.DocEditor(holderId, editorConfig)
        } catch (err) {
          window.clearTimeout(readyTimer)
          failLoad(locale === 'zh-CN'
            ? `编辑器初始化异常：${err instanceof Error ? err.message : String(err)}`
            : `Editor failed to initialize: ${err instanceof Error ? err.message : String(err)}`)
          return
        }
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
      window.clearTimeout(readyTimer)
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
  useEffect(() => {
    if (file) setAIContextFile({ fileId: file.id, fileName: file.name })
    return () => setAIContextFile(null)
  }, [file])

  // AI 面板首次打开时惰性拉取文档文本上下文一次：convertMarkdown 端点转换
  // 出 <源名>.md 后下载其内容（仅支持的部分格式可转换；失败/不支持 → 空
  // 串，面板以无上下文对话）。先占位 null→'' 防止并发重复拉取。
  useEffect(() => {
    if (!aiChatOpen || docTextRef.current !== null) return
    let alive = true
    docTextRef.current = ''
    convertMarkdown(fileId)
      .then(async (r) => {
        const text = await fetchFileText(r.file_id)
        if (alive) docTextRef.current = text
      })
      .catch(() => {
        /* 不支持的格式：无上下文对话 */
      })
    return () => {
      alive = false
    }
  }, [aiChatOpen, fileId])

  /** 头部下拉快捷指令：打开面板并透传给 AIEditChat 自动执行。 */
  const openAiChatWith = (cmd: AIQuickCommand) => {
    setAiChatOpen(true)
    setAiQuick(cmd)
  }

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
        {/* AI 统一入口：完整 AIEditChat 右侧面板（对话+插入）：开源版
            OnlyOffice 无宿主侧内容修改 API，「应用到文档」经 DocFlow AI
            插件（editorConfig.plugins 注入）把回复插入编辑器光标处；
            文档文本上下文经 convertMarkdown 转换端点获取。 */}
        <AIEditChatButton open={aiChatOpen} onToggle={() => setAiChatOpen((v) => !v)} onQuick={openAiChatWith} kind="chat" disabled={loading} />
        <Button size="small" onClick={() => void refreshVersion('版本已刷新')}>刷新版本</Button>
        {/* 保存并退出（v2.6）：触发 DS 立即保存 + 返回文件页。 */}
        <Button size="small" type="primary" onClick={saveAndExit}>保存并退出</Button>
      </div>}

      {!viewMode && saveHint && <div className="banner ok editor-hint">{saveHint}</div>}
      {error && !loadError && <div className="banner error">{error}</div>}
      {loading && !error && !loadError && <div className="hint">{viewMode ? '正在加载查看器…' : '正在加载编辑器…'}</div>}

      {/* 页面级加载失败错误卡（v2.7 反馈 8）：Document Server 地址不可达
          （如后端配置的 public URL 为 127.0.0.1 回环地址）时不再白屏，
          展示当前使用的地址与排查提示。 */}
      {loadError && (
        <EditorLoadError
          message={loadError.message}
          resourceLabel={locale === 'zh-CN' ? 'Document Server 地址' : 'Document Server URL'}
          resourceUrl={loadError.url}
          hints={locale === 'zh-CN' ? [
            '该地址来自平台 ONLYOFFICE 集成配置（public URL），必须是当前浏览器可达的地址。',
            '跨机/容器访问时请勿使用 127.0.0.1 等回环地址：在后端 .env 配置 ONLYOFFICE_PUBLIC_URL 为外部可达地址，或经反向代理域名访问。',
            'HTTPS 页面无法加载 HTTP 资源（混合内容拦截），请保持协议一致；也可刷新页面重试。',
          ] : [
            'This URL comes from the platform ONLYOFFICE integration config (public URL) and must be reachable from your browser.',
            'For cross-machine/container access, avoid 127.0.0.1 loopback addresses: set ONLYOFFICE_PUBLIC_URL in the backend .env to an externally reachable address, or access via a reverse-proxy domain.',
            'An HTTPS page cannot load HTTP resources (mixed content); keep the protocol consistent, or retry by reloading.',
          ]}
          onRetry={() => window.location.reload()}
          retryText={locale === 'zh-CN' ? '刷新重试' : 'Reload'}
        />
      )}

      {/* DocEditor 挂载容器：未进入错误态时始终渲染，placeholder 由 effect
          内命令式创建（每次 init 全新 id）；编辑态包行布局容纳右侧 AI 对话
          面板（对话+插件插入：回复可经 DocFlow AI 插件应用到文档）。 */}
      {!error && !loadError && (
        <div className="editor-with-ai">
          <div className="editor-shell" ref={shellRef} />
          <AIEditChat
            open={aiChatOpen}
            onClose={() => setAiChatOpen(false)}
            getTarget={() => ({ hasSelection: false, text: docTextRef.current ?? '' })}
            getAllText={() => docTextRef.current ?? ''}
            onApply={() => {}}
            fileId={fileId}
            ensureSaved={async () => null}
            reload={async () => {}}
            quickCommand={aiQuick}
            onQuickConsumed={() => setAiQuick(null)}
            forceChatOnly
            officeInsert={viewMode ? undefined : { ready: pluginReady, onInsert: insertToDocument }}
          />
        </div>
      )}
    </div>
  )
}
