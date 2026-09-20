// 按路径访问页（/view/by-path/:nsType/:nsScope/*、/edit/by-path/...）：
// 登录后经 GET /resolve 把命名空间+路径换成 file_id/raw_url，再复用
// ViewerPage 的查看分发（FileViewerDispatch）或按扩展名进入对应编辑器；
// URL 保持 by-path 不跳转。404/非法命名空间/无权限显示带返回入口的
// 友好错误页；目录默认按整站网页打开（index.html，缺省回落 index.htm）。
import { useCallback, useEffect, useState } from 'react'
import { Button } from 'antd'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import {
  ApiError,
  ResolveNamespaceType,
  isDfdocFile,
  isDrawioFile,
  isExcalidrawFile,
  isOfficeFile,
  resolvePath,
} from '../api'
import { textEditorKindFor } from '../openers'
import DfdocEditorPage from './DfdocEditorPage'
import DrawioPage from './DrawioPage'
import EditorPage from './EditorPage'
import ExcalidrawPage from './ExcalidrawPage'
import TextEditorPage from './TextEditorPage'
import { FileViewerDispatch, RawHtmlViewer } from './ViewerPage'

/** by-path 错误卡片：标题 + 说明 + 返回文件页。 */
function ByPathError({ title, detail }: { title: string; detail?: string }) {
  const navigate = useNavigate()
  return (
    <main className="text-editor-page">
      <div className="by-path-error">
        <h2>{title}</h2>
        {detail && <p className="hint">{detail}</p>}
        <Button type="primary" onClick={() => navigate('/')}>返回文件页</Button>
      </div>
    </main>
  )
}

/** 从路由参数解析（nsType、nsScope、逐段解码的路径段）。 */
function useByPathParams(): {
  nsType: ResolveNamespaceType | null
  nsScope: string
  segments: string[]
  rawPath: string
} {
  const params = useParams()
  const rawNs = params.nsType ?? ''
  const nsType: ResolveNamespaceType | null = rawNs === 'space' ? rawNs : null
  // React Router 对 params（含 splat）逐段 decodeURIComponent，此处仅去空段。
  const segments = (params['*'] ?? '')
    .split('/')
    .filter(Boolean)
  return { nsType, nsScope: params.nsScope ?? '', segments, rawPath: params['*'] ?? '' }
}

/** 按路径查看：resolve(mode=view) → 文件走 ViewerPage 分发 / 目录整站 iframe。
 * ?open=<viewMethod> 强制查看方式（透传 FileViewerDispatch force）。 */
export function ViewByPathPage() {
  const { nsType, nsScope, segments, rawPath } = useByPathParams()
  const [searchParams] = useSearchParams()
  // ?origin_content=1：raw_url 取地址的 resolve 携带 origin_content，按
  // CONTENT_PUBLIC_BASE_URL 绝对化（跨 origin 内容域场景；未配置回退相对）。
  const originContent = searchParams.get('origin_content') === '1'
  const forceOpen = searchParams.get('open') ?? undefined
  const [phase, setPhase] = useState<'loading' | 'error' | 'folder' | 'file'>('loading')
  const [error, setError] = useState('')
  // 目录整站：'' = 根 index.html 解析（raw 目录尾斜杠）；'index.htm' = 以
  // index.htm 文件 URL 渲染（raw 目录入口只认 index.html）。
  const [folderMode, setFolderMode] = useState<'' | 'index.htm'>('')
  const [target, setTarget] = useState<{ fileId: string; name: string } | null>(null)

  useEffect(() => {
    if (!nsType) return
    let alive = true
    setPhase('loading')
    setError('')
    const path = segments.join('/')
    void (async () => {
      try {
        const r = await resolvePath(nsType, nsScope, path, { mode: 'view' })
        if (!alive) return
        if (r.type === 'folder') {
          // 默认整站渲染根 index.html；404 时回落 index.htm，两者皆无则友好报错。
          try {
            await resolvePath(nsType, nsScope, [...segments, 'index.html'].join('/'), { mode: 'view' })
            setFolderMode('')
          } catch {
            try {
              await resolvePath(nsType, nsScope, [...segments, 'index.htm'].join('/'), { mode: 'view' })
              setFolderMode('index.htm')
            } catch {
              setError('该目录没有 index.html，无法作为网页打开')
              setPhase('error')
              return
            }
          }
          setPhase('folder')
          return
        }
        setTarget({ fileId: r.file_id, name: r.name })
        setPhase('file')
      } catch (err) {
        if (!alive) return
        if (err instanceof ApiError && err.status === 404) setError('路径不存在或无权访问（404）')
        else if (err instanceof ApiError && err.status === 403) setError('没有访问该路径的权限')
        else setError(err instanceof Error ? err.message : '路径解析失败')
        setPhase('error')
      }
    })()
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nsType, nsScope, rawPath])

  // 文件的 .html 网页查看 / 重试入口：重新 resolve 现取 raw_url。
  const fileResolveRawUrl = useCallback(async () => {
    if (!nsType) return null
    try {
      const r = await resolvePath(nsType, nsScope, segments.join('/'), { mode: 'view', originContent })
      return r.raw_url
    } catch {
      return null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nsType, nsScope, rawPath, originContent])

  // 目录整站查看 / 重试入口：根 index.html 存在时用目录 raw_url（尾斜杠，
  // 相对资源按目录解析）；仅有 index.htm 时用文件 raw_url（相对资源同样
  // 落在同目录）。
  const folderResolveRawUrl = useCallback(async () => {
    if (!nsType) return null
    try {
      const rel = folderMode === 'index.htm' ? [...segments, 'index.htm'].join('/') : segments.join('/')
      const r = await resolvePath(nsType, nsScope, rel, { mode: 'view', originContent })
      return r.raw_url
    } catch {
      return null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nsType, nsScope, rawPath, folderMode, originContent])

  if (!nsType) {
    return <ByPathError title="无效的访问路径" detail="命名空间类型仅支持 space（空间）。" />
  }
  if (phase === 'loading') {
    return <main className="text-editor-page"><div className="text-editor-state">正在解析路径…</div></main>
  }
  if (phase === 'error') {
    return <ByPathError title="无法打开该路径" detail={error} />
  }
  if (phase === 'folder') {
    return <RawHtmlViewer title={segments[segments.length - 1] ?? '网页'} resolveFn={folderResolveRawUrl} />
  }
  if (!target) {
    return <ByPathError title="无法打开该路径" detail={error} />
  }
  return <FileViewerDispatch fileId={target.fileId} name={target.name} resolveRawUrl={fileResolveRawUrl} force={forceOpen} />
}

/**
 * 按路径编辑：resolve(mode=edit，写权限校验) → 按扩展名进入对应编辑器
 * （office/drawio/excalidraw/markdown/html/css/js/txt，与文件页打开方式
 * 一致）；?open=<editMethod> 强制编辑方式（text=Monaco / office=OnlyOffice /
 * drawio / excalidraw / richtext=富文本）；目录与不支持类型显示友好错误页。
 */
export function EditByPathPage() {
  const { nsType, nsScope, segments, rawPath } = useByPathParams()
  const [searchParams] = useSearchParams()
  const forceOpen = searchParams.get('open') ?? undefined
  const [phase, setPhase] = useState<'loading' | 'error' | 'edit'>('loading')
  const [error, setError] = useState('')
  const [target, setTarget] = useState<{ fileId: string; name: string } | null>(null)

  useEffect(() => {
    if (!nsType) return
    let alive = true
    setPhase('loading')
    setError('')
    void (async () => {
      try {
        const r = await resolvePath(nsType, nsScope, segments.join('/'), { mode: 'edit' })
        if (!alive) return
        if (r.type === 'folder') {
          setError('目录不支持在线编辑；请进入目录后选择文件编辑')
          setPhase('error')
          return
        }
        setTarget({ fileId: r.file_id, name: r.name })
        setPhase('edit')
      } catch (err) {
        if (!alive) return
        if (err instanceof ApiError && err.status === 403) setError('没有编辑该文件的权限（guest 或只读目录）')
        else if (err instanceof ApiError && err.status === 404) setError('路径不存在或无权访问（404）')
        else setError(err instanceof Error ? err.message : '路径解析失败')
        setPhase('error')
      }
    })()
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nsType, nsScope, rawPath])

  if (!nsType) {
    return <ByPathError title="无效的访问路径" detail="命名空间类型仅支持 space（空间）。" />
  }
  if (phase === 'loading') {
    return <main className="text-editor-page"><div className="text-editor-state">正在解析路径…</div></main>
  }
  if (phase === 'error' || !target) {
    return <ByPathError title="无法编辑该路径" detail={error} />
  }

  const lower = target.name.toLowerCase()
  // ?open= 强制编辑方式：text→Monaco（md 家族 markdown 源码）、richtext→
  // .dfdoc 富文本（Tiptap JSON）；md 的 richtext 已退役，回落 Monaco 源码、
  // office/drawio/excalidraw→专项编辑器。
  if (forceOpen === 'office') return <EditorPage fileId={target.fileId} />
  if (forceOpen === 'drawio') return <DrawioPage fileId={target.fileId} />
  if (forceOpen === 'excalidraw') return <ExcalidrawPage fileId={target.fileId} />
  if (forceOpen === 'richtext') {
    if (isDfdocFile(lower)) return <DfdocEditorPage fileId={target.fileId} />
    return <TextEditorPage kind="markdown" fileId={target.fileId} />
  }
  if (forceOpen === 'text') {
    const kind = textEditorKindFor(lower)
    return <TextEditorPage kind={kind === 'markdown' || kind === null ? 'text' : kind} fileId={target.fileId} />
  }
  if (isOfficeFile(lower)) return <EditorPage fileId={target.fileId} />
  if (isDrawioFile(lower)) return <DrawioPage fileId={target.fileId} />
  if (isExcalidrawFile(lower)) return <ExcalidrawPage fileId={target.fileId} />
  if (isDfdocFile(lower)) return <DfdocEditorPage fileId={target.fileId} />
  // 文本类（md/html/css/js 及全部可安全编辑的文本扩展名，见 openers.ts）。
  const textKind = textEditorKindFor(lower)
  if (textKind) return <TextEditorPage kind={textKind} fileId={target.fileId} />
  return (
    <ByPathError
      title="该文件类型暂不支持在线编辑"
      detail="支持 Office 文档、draw.io 图表、Excalidraw 白板与文本/代码类文件。"
    />
  )
}
