// 通用文件浏览组件：从 FilesPage 提炼的目录列表 / 面包屑 / 上传 / 下载 /
// 预览（含 office 文档的 ONLYOFFICE「编辑」入口）/ 新建文件夹逻辑，
// 默认空间与非默认空间共用。
// 通过注入 listItems / createFolderFn / uploadFn / downloadFn / previewFn
// 适配不同后端端点；写操作 403 时统一提示「无写权限」。
// v1.0 追加：多选 + 批量移动/删除（部分成功语义）、行内星标切换、
// 行内标签管理（打/去标签、新建）、顶栏标签/收藏筛选与服务端排序。
// v1.2 追加：列表/网格视图切换（设计 6.3.7；偏好持久化 localStorage，
// Ctrl/Cmd+1、Ctrl/Cmd+2 快捷键见设计 6.16.1）。
// v1.3 布局重构：顶栏+筛选栏合并为单行工具带（.files-toolbar，紧邻
// SpaceSwitcher 行下方省行高）；页面级大标题移除只留面包屑；上传改拆分
// 按钮（主点击=上传文件，附落下拉=上传目录）；「全部/收藏/最近」视图
// 切换在 SpaceSwitcher 行下拉化（见 SpaceSwitcher）；查看弹窗标题行
// 改造（标题左、拆分操作按钮右、标题字号缩小）；网页目录（has_index_web）
// 点击改弹窗内嵌 iframe；office 文档弹窗内嵌 OnlyOffice 只读视图（与
// 独立查看页一致，FileViewerDispatch 统一分发）；树点击文件经
// fileOpenSignal 受控信号触发本组件弹窗（见 FolderTreeNav）。
import { FormEvent, ReactNode, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { App as AntdApp, Badge, Button, Dropdown, Input, Menu, Modal as AntdModal, Popover, Select } from 'antd'
import type { DragEvent as ReactDragEvent } from 'react'
import type { MenuProps } from 'antd'
import {
  FileText,
  Folder,
  Globe,
  LayoutGrid,
  List,
  MoreHorizontal,
  Plus,
  Search,
  Star,
  Trash2,
  Upload,
} from 'lucide-react'
import {
  ApiError,
  BatchItemResult,
  EMPTY_DRAWIO_XML,
  EMPTY_DFDOC_JSON,
  EMPTY_EXCALIDRAW_JSON,
  FileItem,
  FileQueryOptions,
  FileWithVersion,
  OpenWithPrefs,
  SHARE_WATERMARK_DEFAULT,
  Space,
  Tag,
  UploadPhase,
  addFileTag,
  batchMoveFiles,
  batchTrashFiles,
  createOfficeTemplate,
  createShare,
  createShareBundle,
  createTag,
  deleteFile,
  downloadBatchFiles,
  downloadFile,
  downloadFolderZip,
  drawioStatus,
  getFileMeta,
  isDfdocFile,
  isDrawioFile,
  isExcalidrawFile,
  isHtmlFile,
  isOfficeFile,
  isUploadAborted,
  listFileTags,
  listFiles,
  listOpenWith,
  listSpaceFiles,
  listSpaces,
  listTags,
  onlyOfficeStatus,
  removeFileTag,
  encodePathSegments,
  renameFile,
  resolveFileById,
  resolvePath,
  setFileStarred,
  summarizeBatchResults,
  unpackZip,
} from '../api'
import {
  allEditEntries,
  allViewEntries,
  builtinOpenWith,
  editMethodLabel,
  editOptionsFor,
  effectiveOpenWithFor,
  extOf,
  isCodeFile,
  isTextEditableFile,
  viewMethodLabel,
  viewOptionsFor,
} from '../openers'
import DirPickerModal from './DirPickerModal'
import type { DirPickerTarget, PickerSpace } from './DirPickerModal'
import TrashModal from './TrashModal'
import { useHotkeys } from '../useHotkeys'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'
// 弹窗内嵌查看：复用独立查看页的按类型分发器（office/drawio/白板/
// xmind/mermaid/md/网页/文本等），保证弹窗与新窗口打开渲染一致。
import { FileViewerDispatch } from '../pages/ViewerPage'

/** 文件浏览视图模式（设计 6.3.7）：list = 现有表格，grid = 卡片网格。 */
export type ViewMode = 'list' | 'grid'

/** 视图偏好持久化 key（默认空间与空间视图共用，见设计 6.3.7）。 */
const VIEW_MODE_KEY = 'docflow.viewMode'

function loadViewMode(): ViewMode {
  return window.localStorage.getItem(VIEW_MODE_KEY) === 'grid' ? 'grid' : 'list'
}

function saveViewMode(mode: ViewMode): void {
  // 隐私模式等 localStorage 不可用时静默跳过（偏好仅本次会话生效）。
  try {
    window.localStorage.setItem(VIEW_MODE_KEY, mode)
  } catch {
    /* ignore */
  }
}

/** 字节数人类可读格式（网格卡片元信息；口径与版本历史一致）。 */
function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`
}

export function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

/**
 * 配额/用量字节数的统一格式化（全站配额展示口径）：
 * B → KiB → MiB → GiB → TiB 自适应，精确到 1 位小数。
 * zeroAsUnlimited=true（默认）时 0 显示「不限」（配额语义）；用量等场景
 * 传 false 以显示 0 字节。
 */
export function formatQuota(n: number, zeroAsUnlimited = true): string {
  if (!Number.isFinite(n) || n < 0) n = 0
  if (n === 0) return zeroAsUnlimited ? '不限' : '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return i === 0 ? `${n} B` : `${v.toFixed(1)} ${units[i]}`
}

/**
 * 弹窗层残留清扫（v2.3 页面偶现卡死修复）：Modal/confirm 在关闭动画被
 * 组件卸载打断时，antd（rc-dialog）的遮罩容器可能残留在 body 且
 * pointer-events 未释放，或 body 的滚动锁（overflow:hidden）未解除，
 * 表现为「页面可滚动但不可点击/右键」。这里在每次弹窗关闭/卸载后延迟
 * 复核：无任何可见弹窗时清掉 body 滚动锁残留，并移除不含可见内容的
 * .ant-modal-root 容器（Popconfirm/Popover 的空根节点一并清）。
 */
export function sweepModalLayer(): void {
  window.setTimeout(() => {
    const body = document.body
    // 仍存在可见弹层时不动（多弹窗叠加/动画进行中）。
    const hasVisibleLayer = document.querySelector(
      '.ant-modal-wrap:not([style*="display: none"]) .ant-modal, .ant-drawer-open, .ant-image-preview-open',
    )
    if (!hasVisibleLayer && body.style.overflow === 'hidden') {
      body.style.removeProperty('overflow')
      body.style.removeProperty('padding-right')
    }
    document.querySelectorAll<HTMLElement>('body > .ant-modal-root, body > .ant-modal-container').forEach((root) => {
      // antd5 根为 .ant-modal-root（wrap 为直接子级）；antd6 根为
      // .ant-modal-container（层级多一层，wrap 可能在任意深度）。
      const wrap = root.querySelector<HTMLElement>('.ant-modal-wrap')
      // 可见 = wrap 未隐藏且仍有弹窗内容；纯遮罩残留（无 wrap 内容）一并移除。
      const visible = wrap != null && wrap.style.display !== 'none' && wrap.childElementCount > 0
      if (!visible) root.remove()
    })
  }, 60)
}

/**
 * 视口内收缩定位 fixed 菜单：按元素实测尺寸把 (x, y) 收进视口
 * （右/下越界时向左/上收，保 8px 边距）——比按固定估算宽高翻转可靠，
 * 右键菜单与树右键菜单共用。
 */
export function clampFixedMenu(el: HTMLElement, x: number, y: number): void {
  const rect = el.getBoundingClientRect()
  const vw = window.innerWidth
  const vh = window.innerHeight
  let left = x
  let top = y
  if (left + rect.width > vw - 8) left = Math.max(8, vw - 8 - rect.width)
  if (top + rect.height > vh - 8) top = Math.max(8, vh - 8 - rect.height)
  el.style.left = `${left}px`
  el.style.top = `${top}px`
}

/**
 * 全站通用弹窗（antd Modal 薄封装，保持既有签名）：title/onClose/wide/
 * className/headExtra/children 与旧自写 Modal 一致，40+ 调用点零改动；
 * 视觉经 styles.css「antd Modal 适配」节对齐旧 .modal。onClose 映射 onCancel
 * （mask 点击 / Esc / 关闭按钮均触发）。
 *
 * 尺寸体系统一（以视口为参照）：
 * - 普通表单弹窗 width=min(520px, 92vw)；wide=min(760px, 92vw)；
 * - 查看弹窗（className 含 modal-viewer）width=min(1180px, 94vw)，body 定高
 *   min(76vh, 760px) 内部滚动（内容 .preview-embed flex:1 撑满 → 内嵌查看器
 *   高度链完整，浏览器不出现页面级滚动条）；
 * - body overflow/maxHeight 统一经 styles prop 设置，删除各处零散高度 hack。
 */
export function Modal({
  title,
  onClose,
  wide,
  className,
  headExtra,
  children,
}: {
  title: string
  onClose: () => void
  wide?: boolean
  /** 追加到弹窗根元素的自定义类（如查看弹窗 modal-viewer 加宽加高）。 */
  className?: string
  /** 标题行右侧追加内容（查看弹窗的「新窗口查看/编辑/下载」拆分按钮组）。 */
  headExtra?: ReactNode
  children: ReactNode
}) {
  const viewer = className?.split(/\s+/).includes('modal-viewer') ?? false
  const handleClose = () => {
    onClose()
    sweepModalLayer()
  }
  // 卸载清扫：宿主普遍以条件渲染（{open && <Modal/>}）关闭弹窗，antd 的
  // 关闭动画被卸载打断时遮罩/滚动锁可能残留（页面可滚动不可点击），统一
  // 在卸载后延迟复核清理（见 sweepModalLayer）。
  useEffect(() => sweepModalLayer, [])
  return (
    <AntdModal
      open
      centered
      footer={null}
      width={viewer ? 'min(1180px, 94vw)' : wide ? 'min(760px, 92vw)' : 'min(520px, 92vw)'}
      styles={{
        body: viewer
          ? { overflow: 'auto', height: 'min(76vh, 760px)', maxHeight: 'min(76vh, 760px)' }
          : { overflow: 'auto', maxHeight: 'calc(94vh - 160px)' },
      }}
      /* 弹窗内部布局定制经官方 classNames 通道注入自有类（styles.css
         「antd Modal 薄封装」节），不再钩 .ant-modal-* 内部结构。 */
      classNames={{
        header: 'docflow-modal-header',
        title: 'docflow-modal-title',
        body: 'docflow-modal-body',
        close: 'docflow-modal-close',
      }}
      onCancel={handleClose}
      title={
        headExtra ? (
          <div className="docflow-modal-title-row">
            <span className="docflow-modal-title-text">{title}</span>
            <span className="docflow-modal-head-extra">{headExtra}</span>
          </div>
        ) : (
          title
        )
      }
      className={className ? `docflow-modal ${className}` : 'docflow-modal'}
    >
      {children}
    </AntdModal>
  )
}

/** antd modal API 类型（App.useApp().modal）。 */
export type AntdModalApi = ReturnType<typeof AntdApp.useApp>['modal']

/**
 * 文件查看弹窗（v2.4 从文件页查看弹窗抽出的最小复用单元）：Modal 薄封装
 * 的查看器规格（modal-viewer：min(1180px,94vw) 宽 + 定高 body）+
 * FileViewerDispatch 统一分发——与文件页行点击查看完全一致。供概览「最近
 * 文件」等页外入口弹窗查看（不新开窗口）。file 仅需 id/name。
 */
export function FileViewModal({ file, onClose }: { file: { id: string; name: string }; onClose: () => void }) {
  return (
    <Modal wide className="modal-viewer" title={`查看「${file.name}」`} onClose={onClose}>
      <div className="preview-embed">
        <FileViewerDispatch
          fileId={file.id}
          name={file.name}
          resolveRawUrl={async () => {
            try {
              const r = await resolveFileById(file.id, { mode: 'view' })
              return r.raw_url
            } catch {
              return null
            }
          }}
        />
      </div>
    </Modal>
  )
}

/** modal.confirm 的 Promise 封装：确认 resolve(true)、取消 resolve(false)；
 *  结束后清扫弹窗层残留（遮罩/滚动锁，见 sweepModalLayer）。 */
export function confirmDialog(
  modal: AntdModalApi,
  opts: { title: string; content?: ReactNode; okText: string; danger?: boolean; cancelText: string },
): Promise<boolean> {
  return new Promise((resolve) => {
    const settle = (ok: boolean) => {
      resolve(ok)
      sweepModalLayer()
    }
    modal.confirm({
      title: opts.title,
      content: opts.content,
      okText: opts.okText,
      okButtonProps: { danger: opts.danger },
      cancelText: opts.cancelText,
      onOk: () => settle(true),
      onCancel: () => settle(false),
    })
  })
}

/** window.prompt 的 antd 替代：modal.confirm + 受控 Input，确认回传输入值、取消回传 null。 */
export function promptViaModal(
  modal: AntdModalApi,
  opts: { title: string; label?: string; initialValue?: string; placeholder?: string; okText: string; cancelText: string },
): Promise<string | null> {
  let value = opts.initialValue ?? ''
  const SyncedInput = () => {
    const [text, setText] = useState(value)
    return (
      <div style={{ marginTop: 12 }}>
        {opts.label && <div style={{ marginBottom: 8 }}>{opts.label}</div>}
        <Input
          autoFocus
          allowClear
          value={text}
          placeholder={opts.placeholder}
          onChange={(e) => {
            setText(e.target.value)
            value = e.target.value
          }}
        />
      </div>
    )
  }
  return new Promise((resolve) => {
    modal.confirm({
      title: opts.title,
      icon: null,
      content: <SyncedInput />,
      okText: opts.okText,
      cancelText: opts.cancelText,
      onOk: () => resolve(value.trim()),
      onCancel: () => resolve(null),
    })
  })
}

export const phaseText: Record<UploadPhase | 'error' | 'canceled', string> = {
  creating: '创建会话…',
  uploading: '上传中…',
  completing: '提交处理…',
  verifying: '校验中…',
  scanning: '安全扫描中…',
  available: '已完成',
  quarantined: '已隔离',
  failed: '失败',
  error: '失败',
  canceled: '已取消',
}

/** 上传任务行 phase：UploadPhase + 本地终态（error=失败 / canceled=用户取消）。 */
type UploadRowPhase = UploadPhase | 'error' | 'canceled'

/** 上传任务是否仍在进行（Badge 计数与「清空已完成」口径：非进行中即可清）。 */
function uploadPhaseActive(p: UploadRowPhase): boolean {
  return p !== 'available' && p !== 'error' && p !== 'canceled' && p !== 'quarantined' && p !== 'failed'
}

/**
 * 树导航步进点击标记：FolderTreeNav 双击导航经 DOM click 逐段点击目录行，
 * 网页目录行的用户点击语义是「网页预览弹窗」——程序化导航点击时由包装器
 * 置位本标记，使该次点击表现为「进入目录」（见 openItem）。
 */
export const treeNavClick = { armed: false }

/** 批量错误码 → 中文提示（与后端 openapi BatchResultItem.error_code 对应）。 */
export const batchErrorText: Record<string, string> = {
  NOT_FOUND: '不存在或无权访问',
  FORBIDDEN: '无写权限',
  ROOT: '根目录不可操作',
  NAME_CONFLICT: '目标目录存在同名项',
  INVALID_TARGET: '不能移动到自身或其子目录',
  PARENT_DELETED: '原目录已删除',
  NOT_DELETED: '不在回收站',
  INTERNAL: '服务内部错误',
}

export function describeBatchResults(results: BatchItemResult[]): string {
  const failures = results.filter((r) => !r.ok)
  if (failures.length === 0) return `全部 ${results.length} 项成功`
  const parts = failures.map((r) => `${r.id.slice(0, 8)}…：${batchErrorText[r.error_code ?? 'INTERNAL'] ?? r.error_code}`)
  return `${summarizeBatchResults(results)}。${parts.join('；')}`
}

interface UploadRow {
  key: number
  name: string
  phase: UploadRowPhase
  error?: string
}

interface Crumb {
  /** 列表查询用的目录 ID；null 表示根（按注入的 listItems 语义，默认根 / 空间根）。 */
  id: string | null
  /** 真实目录 ID（上传/建目录用）；空间根由列表响应回填，默认根为 null。 */
  folderId: string | null
  name: string
}

/** listItems 的返回：目录条目 + 当前列出目录的真实 ID。 */
export interface DirListing {
  items: FileItem[]
  folderId: string | null
}

/**
 * 面包屑 → 命名空间内相对路径段（去掉首段根标签；默认空间与空间视图
 * 通用，供「作为网页打开」等需要按路径 resolve 的场景拼路径复用）。
 */
export function pathSegmentsOf(crumbs: Array<{ id: string | null; name: string }>): string[] {
  return crumbs.slice(1).map((c) => c.name)
}

/** 「编辑文本」入口的适用扩展名（全部文本类，见 openers.ts isTextEditableFile）。 */
function isTextEditable(name: string): boolean {
  return isTextEditableFile(name)
}

/** 「文本文件」新建：按用户自带扩展名推断 MIME（File 构造用）。 */
const TEXT_FILE_MIME: Record<string, string> = {
  html: 'text/html',
  htm: 'text/html',
  css: 'text/css',
  js: 'text/javascript',
  mjs: 'text/javascript',
  json: 'application/json',
  md: 'text/markdown',
  markdown: 'text/markdown',
  txt: 'text/plain',
}

/** 「文本文件」新建：按扩展名选择创建后打开的编辑器路由（与 openers 分发一致）。 */
function textFileRoute(name: string): 'view' | 'markdown' | 'code' | 'text' {
  const lower = name.toLowerCase()
  if (lower.endsWith('.html') || lower.endsWith('.htm')) return 'view'
  if (lower.endsWith('.md') || lower.endsWith('.markdown')) return 'markdown'
  if (isCodeFile(lower)) return 'code'
  return 'text'
}

function sortItems(items: FileItem[]): FileItem[] {
  return [...items].sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'folder' ? -1 : 1))
}

/** 写操作错误文案：403 统一为「无写权限」（guest、只读目录等）。 */
function writeErrorText(err: unknown, fallback: string): string {
  if (err instanceof ApiError && err.status === 403) return '无写权限'
  return err instanceof Error ? err.message : fallback
}

export interface FileBrowserProps {
  /** 面包屑根名称。 */
  rootLabel: string
  listItems: (parentId: string | null, opts?: FileQueryOptions) => Promise<DirListing>
  /** 提供时显示「新建文件夹」。 */
  createFolderFn?: (name: string, parentId: string | null) => Promise<unknown>
  /** 提供时显示「上传文件」。 */
  uploadFn?: (file: File, parentId: string | null, onPhase: (phase: UploadPhase) => void, signal?: AbortSignal) => Promise<unknown>
  /** 下载实现，缺省走个人文件端点。 */
  downloadFn?: (item: FileItem) => Promise<void>
  /** 每行追加操作按钮（分享 / 重命名 / 删除等由调用方渲染）。 */
  rowActions?: (item: FileItem) => ReactNode
  emptyHint?: string
  /** 变化时重新加载当前目录（外部操作成功后刷新列表用）。 */
  reloadKey?: number
  /**
   * 受控视图（全部/收藏/最近）：由 SpaceSwitcher 驱动（视图切换 UI 已上移到
   * 空间切换行，本组件顶栏不再渲染）；变化时同步内部筛选状态并重新查询。
   */
  activeView?: 'all' | 'starred' | 'recent'
  /** 提供时文件行显示「复制」（parentId 为目标目录 UUID；留空目标由本组件解析为源目录）。 */
  copyFn?: (fileId: string, parentId: string) => Promise<unknown>
  /** 文件元数据来源（GET /files/{id}）；提供时网格视图卡片惰性补齐文件大小。 */
  fileMetaFn?: (fileId: string) => Promise<FileWithVersion | null>
  /**
   * 当前空间命名空间（供目录「作为网页打开」按路径 resolve）：默认空间
   * 不传（后端缺省）；非默认空间 space + 空间 ID。检索模式（跨目录）
   * 下面包屑不代表条目位置，菜单项自动隐藏。
   */
  ns?: { type: 'space'; scope: string }
  /**
   * 外部「打开文件」受控信号（左侧目录树文件节点点击触发）：seq 变化时
   * 在当前列表按 fileId 定位并打开查看弹窗；不在当前目录时经
   * fileMetaFn/getFileMeta 拉取元数据后打开。pathSegments 提供时（树内
   * 已知路径）弹窗内 by-path 路由按其构建，避免面包屑不对应。
   */
  fileOpenSignal?: { fileId: string; seq: number; pathSegments?: string[] }
  /**
   * 工具带前缀槽：渲染在工具行最左（空间切换 + 全部/收藏/最近 Segmented，
   * 见 SpaceSwitcher——v1.4 由独立行并入本行）。
   */
  toolbarPrefix?: ReactNode
  /**
   * 工具栏宿主元素（v1.5 顶栏化）：提供时工具行经 React portal 渲染到该
   * 节点——FileBrowserWithTree 把它放在三栏布局上方的全宽顶条
   *（.files-topbar），使工具栏横跨目录树/主区/右侧栏；未提供时回退为
   * 本组件内部的常规渲染（弹窗等宿主缺省场景）。
   */
  toolbarHost?: HTMLElement | null
  /**
   * 复制/移动弹窗（DirPickerModal）的目录列表数据源：缺省复用 listItems。
   * FileBrowserWithTree 会对 listItems 做导航感知包装（登记左侧树节点、
   * 同步当前目录高亮）——弹窗内展开目录若走包装版会联动左侧主树。此
   * prop 供包装器回传「未包装」的原始 listItems，保证弹窗目录树与主树
   * 状态完全隔离（见 DirPickerModal）。
   */
  pickerListItems?: (parentId: string | null, opts?: FileQueryOptions) => Promise<DirListing>
  /** 面包屑前缀槽（空间视图「← 返回空间列表」入口，不占独立行）。 */
  crumbPrefix?: ReactNode
  /**
   * 条目级「分享」入口（右键菜单）：注入时显示（复用宿主页分享对话框）；
   * 缺省不显示（宿主页未注入分享入口时）。
   */
  shareFn?: (item: FileItem) => void
  /**
   * 条目级「重命名」入口（右键菜单）：注入时用宿主页实现；缺省回退本组件
   * 内置的通用重命名（renameFile + prompt 弹窗，端点通用）。
   */
  renameFn?: (item: FileItem) => void
  /**
   * 条目级「删除」入口（右键菜单）：注入时用宿主页实现（如个人空间的撤销
   * 横幅）；缺省回退本组件内置的通用删除确认（deleteFile，回收站可恢复）。
   */
  deleteFn?: (item: FileItem) => void
  /**
   * 隐藏工具栏行尾的「回收站」按钮（v2.2：文件页把回收站移到工具行左端
   * 空间切换旁，经 trashSignal 受控触发本组件的回收站弹窗）。
   */
  hideToolbarTrash?: boolean
  /**
   * 外部「打开回收站」受控信号（数值变化时打开回收站弹窗；0/不变不触发）：
   * 配合 hideToolbarTrash，由宿主在工具行任意位置自绘回收站入口。
   */
  trashSignal?: number
}

export default function FileBrowser({
  rootLabel,
  listItems,
  createFolderFn,
  uploadFn,
  downloadFn,
  rowActions,
  emptyHint,
  reloadKey,
  activeView,
  copyFn,
  fileMetaFn,
  ns,
  fileOpenSignal,
  toolbarPrefix,
  toolbarHost,
  pickerListItems,
  crumbPrefix,
  shareFn,
  renameFn,
  deleteFn,
  hideToolbarTrash,
  trashSignal,
}: FileBrowserProps) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const { modal: antdModal } = AntdApp.useApp()
  const doDownload = downloadFn ?? downloadFile

  const [crumbs, setCrumbs] = useState<Crumb[]>([{ id: null, folderId: null, name: rootLabel }])
  const [items, setItems] = useState<FileItem[]>([])
  const [directoryQuery, setDirectoryQuery] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // 顶栏筛选与服务端排序（标签/收藏激活时进入跨目录检索模式；最近访问为独立视图）。
  const [tags, setTags] = useState<Tag[]>([])
  const [tagFilter, setTagFilter] = useState('')
  const [starredFilter, setStarredFilter] = useState('')
  const [recentView, setRecentView] = useState(false)
  const [sortKey, setSortKey] = useState<'name' | 'updated_at' | 'size'>('name')
  const [sortOrder, setSortOrder] = useState<'asc' | 'desc'>('asc')
  const searchMode = tagFilter !== '' || starredFilter !== '' || recentView

  // 多选与批量操作。
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [batchBusy, setBatchBusy] = useState(false)

  // 视图模式（设计 6.3.7）：list / grid，偏好持久化到 localStorage，
  // 默认空间与空间视图共用同一偏好（状态在两视图间共享，选择互通）。
  const [viewMode, setViewMode] = useState<ViewMode>(loadViewMode)

  // 网格卡片的「⋯」操作菜单：当前展开的条目 ID（null = 关闭）。
  const [cardMenuFor, setCardMenuFor] = useState<string | null>(null)

  // 右键 / 列表行「⋯」菜单：目标条目 + 视口坐标（null = 关闭）。
  const [ctxMenu, setCtxMenu] = useState<{ item: FileItem; x: number; y: number } | null>(null)
  // 菜单浮层元素：渲染后按实测尺寸收进视口（右/下越界向左/上翻，见 clampFixedMenu）。
  const ctxMenuRef = useRef<HTMLDivElement | null>(null)
  useLayoutEffect(() => {
    if (!ctxMenu || !ctxMenuRef.current) return
    clampFixedMenu(ctxMenuRef.current, ctxMenu.x, ctxMenu.y)
  }, [ctxMenu])
  // 「＋ 新建」下拉开关。
  const [createMenuOpen, setCreateMenuOpen] = useState(false)
  // 回收站弹窗（v1.5：整页路由删除，入口为工具栏按钮）。
  const [trashOpen, setTrashOpen] = useState(false)

  // 右键菜单与新建下拉点击外部关闭（菜单内部动作在冒泡阶段完成后收口）。
  // 注意：antd Dropdown 菜单面板挂在 body（.ant-dropdown），Menu 内联子菜单
  // 弹层挂在 body（.ant-menu-submenu-popup，如「打开方式」二级）——两者都
  // 不在触发按钮包裹层内，必须一并纳入豁免，否则 mousedown 先把 open 置
  // false、面板卸载，子菜单项的 click 永远不触发（即「打开方式不可点」的根因）。
  useEffect(() => {
    if (!ctxMenu && !createMenuOpen) return
    const onDown = (e: MouseEvent) => {
      const el = e.target as HTMLElement | null
      if (el?.closest?.('.ctx-menu, .create-menu-wrap, .split-btn, .ant-dropdown, .ant-menu-submenu-popup')) return
      setCtxMenu(null)
      setCreateMenuOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [ctxMenu, createMenuOpen])

  const changeViewMode = (mode: ViewMode) => {
    setCardMenuFor(null)
    setViewMode(mode)
    saveViewMode(mode)
  }

  // 网格视图文件大小：列表接口不返回 size，经 fileMetaFn 惰性补齐（ref 缓存
  // 避免重复请求；-1 占位表示已请求过/未知，不再重试）。
  const sizeCache = useRef<Map<string, number>>(new Map())
  const [, setSizesTick] = useState(0)
  useEffect(() => {
    if (viewMode !== 'grid' || !fileMetaFn) return
    const targets = items.filter((it) => it.type === 'file' && !sizeCache.current.has(it.id))
    if (targets.length === 0) return
    targets.forEach((it) => sizeCache.current.set(it.id, -1))
    void Promise.all(targets.map((it) => fileMetaFn(it.id))).then((metas) => {
      let changed = false
      metas.forEach((meta, i) => {
        const size = meta?.current_version?.size ?? 0
        if (size > 0) {
          sizeCache.current.set(targets[i].id, size)
          changed = true
        }
      })
      if (changed) setSizesTick((n) => n + 1)
    })
  }, [viewMode, items, fileMetaFn])

  // 卡片菜单点击外部关闭（点菜单按钮本身由其 onClick 处理开合切换；
  // 「打开方式」等内联子菜单弹层挂 body，一并豁免）。
  useEffect(() => {
    if (cardMenuFor === null) return
    const onDown = (e: MouseEvent) => {
      const el = e.target as HTMLElement | null
      if (el?.closest?.('.file-card-menu, .card-menu-btn, .ant-menu-submenu-popup')) return
      setCardMenuFor(null)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [cardMenuFor])

  const [batchNotice, setBatchNotice] = useState('')
  const [batchError, setBatchError] = useState('')
  // zip 解包为目录树（POST /files/:id/unpack）：进行中条目 ID + 部分失败明细随结果展示。
  const [unpackBusyId, setUnpackBusyId] = useState<string | null>(null)

  // 复制 / 移动目标目录选择器（替代手输 UUID）：
  // - mode=move：单条移动（menu）或批量移动（batch-bar）共用；
  // - mode=copy：单文件复制（copyFn 注入时）。
  const [dirPicker, setDirPicker] = useState<{ mode: 'copy' | 'move'; item: FileItem | null } | null>(null)
  const [pickerBusy, setPickerBusy] = useState(false)
  const [pickerError, setPickerError] = useState('')

  // 批量分享（v1.6：打包一个链接——多选 ≥2 项创建目录式打包分享；单项
  // 回退普通分享）；选项与单项分享创建对齐（权限/有效期/次数/密码/水印）。
  const [batchShareOpen, setBatchShareOpen] = useState(false)
  const [batchShareBusy, setBatchShareBusy] = useState(false)
  const [batchShareError, setBatchShareError] = useState('')
  const [batchSharePermission, setBatchSharePermission] = useState<'view' | 'download'>('download')
  const [batchShareHours, setBatchShareHours] = useState('0')
  const [batchShareMax, setBatchShareMax] = useState('')
  const [batchSharePassword, setBatchSharePassword] = useState('')
  const [batchShareWatermark, setBatchShareWatermark] = useState(true)
  const [batchShareWatermarkText, setBatchShareWatermarkText] = useState('')
  const [batchShareTitle, setBatchShareTitle] = useState('')
  const [shareLinks, setShareLinks] = useState<Array<{ name: string; url: string }>>([])
  const [shareListOpen, setShareListOpen] = useState(false)
  const [copiedShareIdx, setCopiedShareIdx] = useState(-1)

  // 批量打标签：从已有标签中选择一个应用到全部选中项。
  const [batchTagOpen, setBatchTagOpen] = useState(false)
  const [batchTagId, setBatchTagId] = useState('')
  const [batchTagBusy, setBatchTagBusy] = useState(false)
  const [batchTagError, setBatchTagError] = useState('')

  // 行内标签管理。
  const [tagModalTarget, setTagModalTarget] = useState<FileItem | null>(null)
  const [tagModalFileTagIds, setTagModalFileTagIds] = useState<Set<string>>(new Set())
  const [tagModalNewName, setTagModalNewName] = useState('')
  const [tagModalError, setTagModalError] = useState('')
  const [tagModalBusy, setTagModalBusy] = useState(false)

  // 条目属性弹窗（右键菜单「属性」，文件与目录通用）：元数据惰性拉取
  //（大小/创建时间列表接口不回，经 fileMetaFn/getFileMeta 补齐）。
  const [propsTarget, setPropsTarget] = useState<FileItem | null>(null)
  const [propsMeta, setPropsMeta] = useState<FileWithVersion | null>(null)
  const [propsLoading, setPropsLoading] = useState(false)
  const [propsError, setPropsError] = useState('')

  const openProps = async (item: FileItem) => {
    setPropsTarget(item)
    setPropsMeta(null)
    setPropsError('')
    setPropsLoading(true)
    try {
      const m = await (fileMetaFn ?? getFileMeta)(item.id)
      setPropsMeta(m)
    } catch (err) {
      setPropsError(err instanceof Error ? err.message : '加载属性失败')
    } finally {
      setPropsLoading(false)
    }
  }

  // ONLYOFFICE 集成探测（会话级缓存）：启用且为 office 文档时文件行显示「编辑」。
  const [ooEnabled, setOoEnabled] = useState(false)
  useEffect(() => {
    let alive = true
    void onlyOfficeStatus().then((s) => {
      if (alive) setOoEnabled(s.enabled)
    })
    return () => {
      alive = false
    }
  }, [])

  // draw.io 图表编辑集成探测（会话级缓存）：启用时 .drawio 文件行显示
  //「图表」按钮、工具栏显示「新建图表」。
  const [drawioEnabled, setDrawioEnabled] = useState(false)
  useEffect(() => {
    let alive = true
    void drawioStatus().then((s) => {
      if (alive) setDrawioEnabled(s.enabled)
    })
    return () => {
      alive = false
    }
  }, [])

  // ---- 默认打开方式偏好（/me/open-with）：初始化加载（管理入口在设置页）。 ----
  const [openWith, setOpenWithMap] = useState<OpenWithPrefs>({})
  useEffect(() => {
    let alive = true
    void listOpenWith()
      .then((map) => {
        if (alive) setOpenWithMap(map)
      })
      .catch(() => {
        /* 偏好不可用（旧后端等）：按内置默认分发 */
      })
    return () => {
      alive = false
    }
  }, [])

  // ---- 目录上传（webkitdirectory）：进行中进度与结束后的成功/失败明细。 ----
  const dirInputRef = useRef<HTMLInputElement>(null)
  const [dirUpload, setDirUpload] = useState<{ done: number; total: number } | null>(null)
  const [dirResult, setDirResult] = useState<{
    root: string
    ok: number
    failures: Array<{ path: string; reason: string }>
  } | null>(null)

  // ---- 目录打包下载（download.zip）：进行中的条目 ID（按钮禁用/文案用）。 ----
  const [zipBusyId, setZipBusyId] = useState<string | null>(null)

  const [folderOpen, setFolderOpen] = useState(false)
  const [folderName, setFolderName] = useState('')
  const [folderError, setFolderError] = useState('')
  const [createKind, setCreateKind] = useState<'md' | 'dfdoc' | 'textfile' | 'drawio' | 'whiteboard' | 'word' | 'spreadsheet' | 'presentation' | null>(null)
  const [createName, setCreateName] = useState('')
  const [createError, setCreateError] = useState('')

  // 查看弹窗：目标条目（文件 / 网页目录）；webPreviewUrl 为网页目录内嵌
  // iframe 的 raw_url；previewPathOverride 为外部（树）打开文件的命名空间
  // 路径段（弹窗内 by-path 路由用，见 routeFor）。
  const [previewTarget, setPreviewTarget] = useState<FileItem | null>(null)
  const [webPreviewUrl, setWebPreviewUrl] = useState<string | null>(null)
  const [previewPathOverride, setPreviewPathOverride] = useState<string[] | null>(null)

  const [uploads, setUploads] = useState<UploadRow[]>([])
  const fileInputRef = useRef<HTMLInputElement>(null)
  const uploadKey = useRef(0)
  // 上传任务面板（v1.6：浮条改工具栏按钮 + 弹窗；Badge 显示进行中数量）。
  const [uploadPanelOpen, setUploadPanelOpen] = useState(false)
  // 上传取消（v1.6）：进行中任务的 AbortController（key → controller）与
  // 「开始前即被取消」的 key 集合（顺序队列里尚未轮到的文件直接跳过）。
  const uploadAbortRef = useRef(new Map<number, AbortController>())
  const uploadCancelReqRef = useRef(new Set<number>())

  // ---- 拖拽上传（v1.6）：拖文件/文件夹到文件管理区 → 上传到当前目录 ----
  // webkitGetAsEntry 递归展开目录树；悬停高亮 drop zone（计数器法防子元素闪烁）。
  const [dropActive, setDropActive] = useState(false)
  const dragDepthRef = useRef(0)

  const currentParent = crumbs[crumbs.length - 1].id
  const currentFolderId = crumbs[crumbs.length - 1].folderId
  // 窄屏搜索 Popover 开合（<1280px 目录搜索收窄为图标按钮，见 toolbar）。
  const [searchPopOpen, setSearchPopOpen] = useState(false)
  const visibleItems = directoryQuery.trim()
    ? items.filter((item) => item.name.toLocaleLowerCase().includes(directoryQuery.trim().toLocaleLowerCase()))
    : items

  const currentOpts = (): FileQueryOptions => ({
    tagId: tagFilter || null,
    starred: starredFilter === '' ? undefined : starredFilter === 'true',
    recent: recentView,
    sort: sortKey,
    order: sortOrder,
    // 拉满 limit（后端钳制 1000）：目录树与列表共用该查询结果，默认 100
    // 会截断多子项目录（树 caret 误判「空」的根因，见 FolderTreeNav）。
    limit: 1000,
  })

  const refreshTags = () => {
    void listTags()
      .then(setTags)
      .catch(() => {})
  }

  useEffect(() => {
    refreshTags()
  }, [])

  const load = async (parentId: string | null) => {
    setLoading(true)
    setError('')
    setSelected(new Set())
    try {
      const opts = currentOpts()
      const { items: list, folderId } = await listItems(searchMode ? null : parentId, opts)
      // name 排序保持「目录优先 + 名称」的本地归并（服务端 name 为纯字典序）；
      // 其余排序键与最近访问视图（last_access_at 倒序）直接采用服务端顺序。
      setItems(sortKey === 'name' && !opts.recent ? sortItems(list) : list)
      if (folderId) {
        // 空间根目录：列表响应回填真实目录 ID，供上传/建目录使用。
        setCrumbs((prev) => prev.map((c, i) => (i === prev.length - 1 ? { ...c, folderId } : c)))
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('loadFailed'))
      setItems([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load(null)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 筛选/排序/视图变化时重新查询当前目录（跳过首挂载，避免与初始 load 重复）。
  const mounted = useRef(false)
  useEffect(() => {
    if (!mounted.current) {
      mounted.current = true
      return
    }
    void load(currentParent)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tagFilter, starredFilter, recentView, sortKey, sortOrder])

  useEffect(() => {
    if (reloadKey) void load(currentParent)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reloadKey])

  const openFolder = (item: FileItem) => {
    if (searchMode) return
    setCrumbs((prev) => [...prev, { id: item.id, folderId: item.id, name: item.name }])
    void load(item.id)
  }

  const gotoCrumb = (index: number) => {
    if (searchMode) return
    setCrumbs((prev) => prev.slice(0, index + 1))
    void load(crumbs[index].id)
  }

  // 表头排序：点击已激活键翻转方向；切换键时取该键默认方向
  //（名称升序；修改时间/大小默认降序 = 最新/最大优先）。
  const toggleSort = (key: 'name' | 'updated_at' | 'size') => {
    if (sortKey === key) {
      setSortOrder(sortOrder === 'asc' ? 'desc' : 'asc')
      return
    }
    setSortKey(key)
    setSortOrder(key === 'name' ? 'asc' : 'desc')
  }

  // 视图切换（全部 / 收藏 / 最近）：UI 已上移到 SpaceSwitcher，本组件经受控
  // prop activeView 驱动——外部值变化时同步内部筛选状态（收藏视图即
  // starred=true；最近视图走 ?recent=true，后端忽略其余过滤；其余清空），
  // 由此触发下方的筛选 effect 重新查询。
  useEffect(() => {
    if (!activeView) return
    setRecentView(activeView === 'recent')
    setTagFilter('')
    setStarredFilter(activeView === 'starred' ? 'true' : '')
  }, [activeView])

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const allSelected = items.length > 0 && items.every((item) => selected.has(item.id))
  const toggleSelectAll = () => {
    setSelected(allSelected ? new Set() : new Set(items.map((item) => item.id)))
  }

  const selectedIds = [...selected]
  const finishBatch = (results: BatchItemResult[]) => {
    setBatchNotice(describeBatchResults(results))
    setBatchError('')
    void load(currentParent)
  }

  // ---- 移动 / 复制（DirPickerModal 目标目录选择器） ----

  /** 打开目录选择器：单条移动（item 非空）或批量移动（item=null，作用于选中集）。
   * 每次打开都刷新空间列表（挂载后新建的空间立即可选为跨空间目标）。 */
  const refreshPickerSpaces = () => {
    void listSpaces()
      .then(setPickerAllSpaces)
      .catch(() => {})
  }

  const openMovePicker = (item: FileItem | null) => {
    setPickerError('')
    refreshPickerSpaces()
    setDirPicker({ mode: 'move', item })
  }

  /** 打开目录选择器：复制（单条 copyFn；批量 copyFn 逐项循环，目录同样
   * 支持子树复制）。 */
  const openCopyPicker = (item: FileItem | null) => {
    if (!copyFn) return
    setPickerError('')
    refreshPickerSpaces()
    setDirPicker({ mode: 'copy', item })
  }

  /** 目录选择器数据源：优先注入的 pickerListItems（未被树导航包装的原始
   * 版本，保证弹窗展开目录不联动左侧主树），缺省复用注入的 listItems
   *（返回全量列表，弹窗内过滤目录）。 */
  const listChildrenForPicker = useCallback(
    async (parentId: string | null): Promise<FileItem[]> => {
      const { items } = await (pickerListItems ?? listItems)(parentId)
      return items
    },
    [pickerListItems, listItems],
  )

  // ---- 复制/移动目标空间（v1.6 跨空间，v2.0 统一空间模型）：当前空间 +
  // 我的其余空间（默认空间「我的文件」+ 其他空间） ----
  // 后端 batch/move 与 copy 均按目标目录继承空间作用域并做写权限
  // 校验，target_parent_id 指向其他空间目录即可跨 root。空间列表惰性加载；
  // 仅有当前一个空间时不显示空间切换（回退单空间模式）。
  const [pickerAllSpaces, setPickerAllSpaces] = useState<Space[]>([])
  useEffect(() => {
    let alive = true
    void listSpaces()
      .then((list) => {
        if (alive) setPickerAllSpaces(list)
      })
      .catch(() => {
        /* 空间列表不可用：仅当前空间（跨空间入口隐藏） */
      })
    return () => {
      alive = false
    }
  }, [])

  const pickerSpaces = useMemo<PickerSpace[]>(() => {
    const zh = locale === 'zh-CN'
    // 当前空间为非默认空间时，current 根经 listSpaceFiles 响应 parent_id
    // 解析（空空间根也能拿到 UUID；默认空间根留 ''，batch/move 缺省即默认根）。
    const currentResolveRoot = ns
      ? async () => (await listSpaceFiles(ns.scope, null)).parent_id ?? ''
      : undefined
    const out: PickerSpace[] = [{
      key: 'current',
      label: rootLabel,
      listChildren: (pid) => listChildrenForPicker(pid),
      resolveRootId: currentResolveRoot,
    }]
    for (const s of pickerAllSpaces) {
      // 当前空间（未传 ns=默认空间；传 ns=ns.scope）已固定在首位，跳过。
      const isCurrent = ns ? s.id === ns.scope : s.is_default
      if (isCurrent) continue
      if (s.is_default) {
        out.push({
          key: `space:${s.id}`,
          label: zh ? '我的文件' : 'My files',
          listChildren: async (pid) => listFiles(pid),
        })
      } else {
        out.push({
          key: `space:${s.id}`,
          label: s.name,
          listChildren: async (pid) => (await listSpaceFiles(s.id, pid)).files ?? [],
          // 空空间根也须能定位根 UUID（batch/move 的 '' 缺省=默认空间根，
          // 会静默移错空间）。
          resolveRootId: async () => (await listSpaceFiles(s.id, null)).parent_id ?? '',
        })
      }
    }
    return out
  }, [locale, ns, rootLabel, pickerAllSpaces, listChildrenForPicker])

  /** 选择器确认：移动走 batch/move（单条/批量同端点，部分成功语义）；复制走
   * copyFn（单条直调；批量逐项循环收集部分失败——目录子树复制由后端
   * CopyFolder 支持，目标可为跨空间目录）。 */
  const handleDirPickerConfirm = async (target: DirPickerTarget) => {
    if (!dirPicker) return
    setPickerBusy(true)
    setPickerError('')
    try {
      if (dirPicker.mode === 'move') {
        // 单条移动：目标可为其父目录等；批量移动作用于当前选中集。
        const ids = dirPicker.item ? [dirPicker.item.id] : selectedIds
        if (ids.length === 0) return
        const results = await batchMoveFiles(ids, target.id)
        setDirPicker(null)
        finishBatch(results)
      } else {
        if (!copyFn) return
        const ids = dirPicker.item ? [dirPicker.item.id] : selectedIds
        if (ids.length === 0) return
        // 根目录为空时无法从子项 parent_id 探测根 UUID（copy 端点要求显式
        // parent_id，无 ''=根 的便捷语义），给出可读错误而非后端 400。
        if (!target.id) {
          setPickerError(locale === 'zh-CN' ? '目标根目录为空，无法确定目录 ID；请先在目标根目录创建任意文件/目录' : 'Cannot resolve the empty target root folder ID; create any item under it first')
          return
        }
        const failures: string[] = []
        for (const id of ids) {
          try {
            await copyFn(id, target.id)
          } catch (err) {
            failures.push(`${id.slice(0, 8)}…：${writeErrorText(err, msg('fileCopyFailed'))}`)
          }
        }
        setDirPicker(null)
        if (failures.length > 0) {
          setBatchNotice('')
          setBatchError(`复制部分失败（${ids.length - failures.length}/${ids.length} 成功）：${failures.join('；')}`)
        } else {
          setBatchError('')
          setBatchNotice(msg('copyOk'))
        }
        await load(currentParent)
      }
    } catch (err) {
      setPickerError(writeErrorText(err, dirPicker.mode === 'move' ? '移动失败' : msg('fileCopyFailed')))
    } finally {
      setPickerBusy(false)
    }
  }

  // 批量删除确认：antd Modal.confirm（走 App 上下文，明暗/accent 主题一致）。
  const handleBatchTrash = () => {
    if (selectedIds.length === 0) return
    antdModal.confirm({
      title: msg('delete'),
      content: formatMessage(msg('batchTrashConfirm'), { n: selectedIds.length }),
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        setBatchBusy(true)
        setBatchNotice('')
        try {
          const results = await batchTrashFiles(selectedIds)
          finishBatch(results)
        } catch (err) {
          setBatchError(writeErrorText(err, msg('deleteFailed')))
        } finally {
          setBatchBusy(false)
        }
      },
    })
  }

  // ---- 批量下载（zip 流） ----

  const handleBatchDownload = async () => {
    if (selectedIds.length === 0) return
    setBatchBusy(true)
    setBatchError('')
    setBatchNotice('')
    try {
      await downloadBatchFiles(selectedIds)
    } catch (err) {
      setBatchError(err instanceof Error ? err.message : msg('downloadFailed'))
    } finally {
      setBatchBusy(false)
    }
  }

  // ---- 批量分享（v1.6：打包一个链接）----
  // 多选 ≥2 项 → 创建目录式「打包分享」（后端 share_files，一个 /s/<token>
  // 链接展示全部选中项，可逐项预览/下载或整包 zip）；单项选中 → 回退普通
  // 公开分享。选项（权限/有效期/次数/密码/水印）与单项分享创建对齐。

  /** 打包分享标题智能默认：首个非「根目录」条目名 + 「等 N 项」；全部命中
   * 兜底占位（后端再回退「打包分享（N 项）」）。单项分享不设标题。 */
  const suggestShareTitle = (ids: string[]): string => {
    const zh = locale === 'zh-CN'
    const names = ids
      .map((id) => items.find((it) => it.id === id)?.name)
      .filter((n): n is string => Boolean(n) && n !== '根目录' && n !== 'root')
    if (names.length === 0) return ''
    const head = names[0].length > 24 ? `${names[0].slice(0, 24)}…` : names[0]
    return ids.length > 1
      ? (zh ? `${head} 等 ${ids.length} 项` : `${head} + ${ids.length - 1} more`)
      : head
  }

  const openBatchShareDialog = () => {
    if (selectedIds.length === 0) {
      setBatchError(msg('batchShareEmpty'))
      return
    }
    setBatchSharePermission('download')
    setBatchShareHours('0')
    setBatchShareMax('')
    setBatchSharePassword('')
    setBatchShareWatermark(true)
    setBatchShareWatermarkText(SHARE_WATERMARK_DEFAULT)
    setBatchShareTitle(selectedIds.length > 1 ? suggestShareTitle(selectedIds) : '')
    setBatchShareError('')
    setBatchShareOpen(true)
  }

  const handleBatchShareCreate = async (e: FormEvent) => {
    e.preventDefault()
    const ids = selectedIds
    if (ids.length === 0 || batchShareBusy) return
    const pwd = batchSharePassword.trim()
    if (pwd && (pwd.length < 4 || pwd.length > 64)) {
      setBatchShareError('访问密码须为 4-64 个字符')
      return
    }
    setBatchShareBusy(true)
    setBatchShareError('')
    try {
      const opts = {
        permission: batchSharePermission,
        expiresInHours: Number(batchShareHours) || 0,
        maxDownloads: batchShareMax.trim() === '' ? undefined : Number(batchShareMax),
        password: pwd || undefined,
        watermarkEnabled: batchShareWatermark,
        watermarkText: batchShareWatermark ? (batchShareWatermarkText.trim() || undefined) : undefined,
      }
      const created = ids.length === 1
        ? await createShare({ fileId: ids[0], visibility: 'public', ...opts })
        : await createShareBundle({ fileIds: ids, title: batchShareTitle.trim() || undefined, ...opts })
      const name = ids.length === 1
        ? (items.find((it) => it.id === ids[0])?.name ?? '分享')
        : (batchShareTitle.trim() || `打包分享（${ids.length} 项）`)
      if (created.token) {
        setShareLinks([{ name, url: `${window.location.origin}/s/${created.token}` }])
        setCopiedShareIdx(-1)
        setBatchShareOpen(false)
        setShareListOpen(true)
        setBatchError('')
        setBatchNotice(ids.length === 1 ? '分享链接已创建' : `已创建打包分享链接（${ids.length} 项）`)
      } else {
        setBatchShareError('创建分享失败：未返回令牌')
      }
    } catch (err) {
      setBatchShareError(err instanceof Error ? err.message : '创建分享失败')
    } finally {
      setBatchShareBusy(false)
    }
  }

  const copyShareLink = async (url: string, index: number) => {
    try {
      await navigator.clipboard.writeText(url)
      setCopiedShareIdx(index)
    } catch {
      setBatchError(msg('clipboardCopyFailed'))
    }
  }

  // ---- 批量打标签（对每个选中项打同一已有标签） ----

  const openBatchTagDialog = () => {
    if (tags.length === 0) {
      setBatchError(msg('noTagsHint'))
      return
    }
    setBatchTagId(tags[0]?.id ?? '')
    setBatchTagError('')
    setBatchTagOpen(true)
  }

  const handleBatchTag = async (e: FormEvent) => {
    e.preventDefault()
    if (!batchTagId || selectedIds.length === 0) return
    setBatchTagBusy(true)
    setBatchTagError('')
    let ok = 0
    let fail = 0
    for (const id of selectedIds) {
      try {
        await addFileTag(id, batchTagId)
        ok++
      } catch {
        fail++
      }
    }
    setBatchTagBusy(false)
    if (fail > 0) {
      setBatchTagError(formatMessage(msg('batchTagPartial'), { ok, fail }))
      return
    }
    setBatchTagOpen(false)
    setBatchError('')
    setBatchNotice(formatMessage(msg('batchTagOk'), { n: ok }))
  }

  // ---- 批量收藏 / 取消收藏（循环 PATCH starred） ----

  const selectedItems = items.filter((it) => selected.has(it.id))
  const allSelectedStarred = selectedItems.length > 0 && selectedItems.every((it) => it.is_starred)

  const handleBatchStar = async () => {
    if (selectedIds.length === 0) return
    const target = !allSelectedStarred
    setBatchBusy(true)
    setBatchError('')
    setBatchNotice('')
    let fail = 0
    for (const id of selectedIds) {
      try {
        await setFileStarred(id, target)
      } catch {
        fail++
      }
    }
    setBatchBusy(false)
    if (fail > 0) {
      setBatchError(formatMessage(msg('batchStarFailed'), { fail }))
      return
    }
    setBatchNotice(formatMessage(target ? msg('batchStarOk') : msg('batchUnstarOk'), { n: selectedIds.length }))
    void load(currentParent)
  }

  // ---- 星标 ----

  const toggleStar = async (item: FileItem) => {
    setError('')
    try {
      const updated = await setFileStarred(item.id, !item.is_starred)
      setItems((prev) => prev.map((it) => (it.id === item.id ? { ...it, is_starred: updated.is_starred } : it)))
    } catch (err) {
      setError(err instanceof Error ? err.message : '收藏操作失败')
    }
  }

  // ---- 行内标签 ----

  const openTagModal = async (item: FileItem) => {
    setTagModalTarget(item)
    setTagModalNewName('')
    setTagModalError('')
    setTagModalFileTagIds(new Set())
    try {
      const attached = await listFileTags(item.id)
      setTagModalFileTagIds(new Set(attached.map((t) => t.id)))
    } catch (err) {
      setTagModalError(err instanceof Error ? err.message : '加载标签失败')
    }
  }

  const toggleFileTag = async (tag: Tag) => {
    if (!tagModalTarget) return
    const attached = tagModalFileTagIds.has(tag.id)
    setTagModalBusy(true)
    setTagModalError('')
    try {
      if (attached) {
        await removeFileTag(tagModalTarget.id, tag.id)
        setTagModalFileTagIds((prev) => {
          const next = new Set(prev)
          next.delete(tag.id)
          return next
        })
      } else {
        await addFileTag(tagModalTarget.id, tag.id)
        setTagModalFileTagIds((prev) => new Set(prev).add(tag.id))
      }
    } catch (err) {
      setTagModalError(writeErrorText(err, '标签操作失败'))
    } finally {
      setTagModalBusy(false)
    }
  }

  const handleCreateTagAndAttach = async (e: FormEvent) => {
    e.preventDefault()
    if (!tagModalTarget) return
    const name = tagModalNewName.trim()
    if (!name) return
    setTagModalBusy(true)
    setTagModalError('')
    try {
      const created = await createTag(name)
      setTags((prev) => [...prev, created].sort((a, b) => a.name.localeCompare(b.name)))
      await addFileTag(tagModalTarget.id, created.id)
      setTagModalFileTagIds((prev) => new Set(prev).add(created.id))
      setTagModalNewName('')
    } catch (err) {
      setTagModalError(err instanceof Error ? err.message : '创建标签失败')
    } finally {
      setTagModalBusy(false)
    }
  }

  // ---- 既有操作 ----

  const handleCreateFolder = async (e: FormEvent) => {
    e.preventDefault()
    if (!createFolderFn) return
    const name = folderName.trim()
    if (!name) return
    setFolderError('')
    try {
      await createFolderFn(name, currentFolderId)
      setFolderOpen(false)
      setFolderName('')
      await load(currentParent)
    } catch (err) {
      setFolderError(writeErrorText(err, '创建失败'))
    }
  }

  const handleDownload = async (item: FileItem) => {
    setError('')
    try {
      await doDownload(item)
    } catch (err) {
      setError(err instanceof Error ? err.message : '下载失败')
    }
  }

  // ---- 打开路由：by-path 优先（网页相对引用不漂移），检索模式回退 uuid ----

  // 命名空间内按路径构造 /view|edit/by-path URL：URL 携带完整目录路径，
  // html/js 内部相对引用（ajax、相对 src/href）按路径层级解析不漂移；
  // by-path 页按扩展名自动分发查看器/编辑器。跨目录检索模式（面包屑
  // 不代表条目位置）或 ns 缺失时回退 UUID 路由；外部（树）打开的文件
  // 携带 previewPathOverride 时按其路径构建。
  const routeFor = (prefix: 'view' | 'edit', item: FileItem) => {
    if (!ns || searchMode) return `/${prefix}/${item.id}`
    const segs = (
      item.id === previewTarget?.id && previewPathOverride
        ? previewPathOverride
        : [...pathSegmentsOf(crumbs), item.name]
    ).map(encodeURIComponent)
    return `/${prefix}/by-path/${ns.type}/${encodeURIComponent(ns.scope)}/${segs.join('/')}/`
  }

  // ---- 目录「作为网页打开」（resolve 目录下 index.html → raw_url / 新窗口） ----

  /** 目录在命名空间内的路径段（面包屑拼当前目录路径，含空间视图）；检索模式返回 null。 */
  const folderSegmentsOf = (item: FileItem): string[] | null => {
    if (!ns || searchMode) return null
    return [...pathSegmentsOf(crumbs), item.name]
  }

  /** 解析网页目录入口（index.html，回退 index.htm）的 raw_url；不存在返回 null。 */
  const resolveWebFolderUrl = async (segments: string[]): Promise<string | null> => {
    if (!ns) return null
    try {
      return (await resolvePath(ns.type, ns.scope, [...segments, 'index.html'].join('/'), { mode: 'view' })).raw_url
    } catch {
      // index.html 不存在：尝试 index.htm。
      try {
        return (await resolvePath(ns.type, ns.scope, [...segments, 'index.htm'].join('/'), { mode: 'view' })).raw_url
      } catch {
        return null
      }
    }
  }

  /** 菜单「作为网页打开（新窗口）」：resolve 校验入口存在后打开独立查看页。 */
  const openAsWebsite = async (item: FileItem) => {
    const segments = folderSegmentsOf(item)
    if (!ns || !segments) return
    setError('')
    const url = await resolveWebFolderUrl(segments)
    if (!url) {
      setError('该目录没有 index.html')
      return
    }
    const encoded = encodePathSegments(segments)
    openEditorWindow(`/view/by-path/${ns.type}/${encodeURIComponent(ns.scope)}/${encoded}/`)
  }

  /**
   * 网页目录弹窗内嵌查看（默认点击行为）：resolve 目录入口 raw_url 后
   * 复用 previewTarget 机制弹窗，内容渲染 sandbox iframe（菜单保留
   * 「作为网页打开（新窗口）」入口）。fallbackToFolder：目录名启发式
   *（名称含「网页」）触发但实际无 index.html 时静默回退为进入目录。
   */
  const openWebFolderPreview = async (item: FileItem, fallbackToFolder = false) => {
    const segments = folderSegmentsOf(item)
    if (!segments) return
    setError('')
    const url = await resolveWebFolderUrl(segments)
    if (!url) {
      if (fallbackToFolder) {
        openFolder(item)
        return
      }
      setError('该目录没有 index.html')
      return
    }
    setPreviewTarget(item)
    setPreviewPathOverride(null)
    setWebPreviewUrl(url)
  }

  // ---- zip 解包为目录树（父目录下以 zip 名建目录，部分成功语义） ----

  const handleUnpack = async (item: FileItem) => {
    if (unpackBusyId) return
    setUnpackBusyId(item.id)
    setBatchNotice(`正在解包「${item.name}」…`)
    setBatchError('')
    try {
      const r = await unpackZip(item.id)
      const summary = `新建 ${r.created_folders} 个目录、${r.created_files} 个文件${r.skipped > 0 ? `，同名跳过 ${r.skipped} 项` : ''}`
      if (r.failures && r.failures.length > 0) {
        const detail = r.failures.map((f) => `${f.path}：${f.error}`).join('；')
        setBatchNotice('')
        setBatchError(`解包完成（${summary}），但 ${r.failures.length} 项失败：${detail}`)
      } else {
        setBatchError('')
        setBatchNotice(`解包完成：${summary}。`)
      }
      await load(currentParent)
    } catch (err) {
      setBatchNotice('')
      setBatchError(writeErrorText(err, '解包失败'))
    } finally {
      setUnpackBusyId(null)
    }
  }

  // 弹窗查看：内容渲染由 FileViewerDispatch 就地完成（无预加载逻辑）。
  const openPreview = (item: FileItem) => {
    setPreviewTarget(item)
    setPreviewPathOverride(null)
    setWebPreviewUrl(null)
  }

  const closePreview = () => {
    setPreviewTarget(null)
    setPreviewPathOverride(null)
    setWebPreviewUrl(null)
  }

  // 外部「打开文件」受控信号（左侧目录树文件节点点击）：当前列表命中直接
  // 弹窗；未命中（不在当前目录）经 fileMetaFn/getFileMeta 拉取元数据后弹窗，
  // pathSegments 一并记录供弹窗内 by-path 路由使用。
  // v2.3 信号改为「变化驱动 + 首次挂载跳过」：FileBrowserWithTree 树导航会
  // 以 key 重挂载本组件，挂载期不按旧信号重放（否则点目录复开上次的查看
  // 弹窗——串扰 bug 根因之一），同一实例上的后续信号变化正常触发。
  const fileOpenSignalMountedRef = useRef(false)
  useEffect(() => {
    if (!fileOpenSignalMountedRef.current) {
      fileOpenSignalMountedRef.current = true
      return
    }
    if (!fileOpenSignal || fileOpenSignal.seq <= 0) return
    const { fileId, pathSegments } = fileOpenSignal
    const found = items.find((it) => it.id === fileId && it.type === 'file')
    if (found) {
      setPreviewTarget(found)
      setPreviewPathOverride(null)
      setWebPreviewUrl(null)
      return
    }
    const meta = fileMetaFn ?? ((id: string) => getFileMeta(id))
    void meta(fileId)
      .then((m) => {
        if (!m) return
        setPreviewTarget(m)
        setPreviewPathOverride(pathSegments ?? null)
        setWebPreviewUrl(null)
      })
      .catch(() => {
        /* 元数据拉取失败（无权限/已删除）：静默 */
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileOpenSignal])

  // 外部「打开回收站」受控信号（数值变化时打开回收站弹窗；0 不触发）。
  // 依据：与 fileOpenSignal 同模式——树导航以 key 重挂载本组件，挂载期跳过
  // 旧信号（v2.3 修复：打开回收站→关闭→点目录树导航，重挂载后回收站弹窗
  // 「复开」的串扰 bug）；同一实例上的后续信号变化正常触发。
  const trashSignalMountedRef = useRef(false)
  useEffect(() => {
    if (!trashSignalMountedRef.current) {
      trashSignalMountedRef.current = true
      return
    }
    if (!trashSignal || trashSignal <= 0) return
    setTrashOpen(true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [trashSignal])

  // ---- 受控上传队列（v1.6 取消）：文件选择 / 拖拽两条队列逐文件串行，
  //      每文件一条任务行；取消 = 排队中直接标记跳过 / 进行中 abort 传输。 ----

  /** 排队中的任务在开始前检查取消请求：命中则行标记「已取消」并返回 true。 */
  const skipCanceledRow = (key: number): boolean => {
    if (!uploadCancelReqRef.current.has(key)) return false
    setUploads((prev) => prev.map((r) => (r.key === key ? { ...r, phase: 'canceled', error: undefined } : r)))
    return true
  }

  /** 单文件受控上传：登记 AbortController、驱动行内 phase；失败归类
   *（取消 → canceled，其余 → error 中文文案）。返回 ok=成功 / canceled=用户取消 / error=失败。 */
  const runTrackedUpload = async (file: File, parentId: string | null, key: number): Promise<'ok' | 'canceled' | 'error'> => {
    if (!uploadFn) return 'error'
    if (skipCanceledRow(key)) return 'canceled'
    const ctrl = new AbortController()
    uploadAbortRef.current.set(key, ctrl)
    try {
      await uploadFn(file, parentId, (phase) => {
        setUploads((prev) => prev.map((r) => (r.key === key ? { ...r, phase } : r)))
      }, ctrl.signal)
      return 'ok'
    } catch (err) {
      const canceled = isUploadAborted(err) || uploadCancelReqRef.current.has(key)
      setUploads((prev) =>
        prev.map((r) =>
          r.key === key
            ? (canceled
              ? { ...r, phase: 'canceled', error: undefined }
              : { ...r, phase: 'error', error: writeErrorText(err, '上传失败') })
            : r,
        ),
      )
      return canceled ? 'canceled' : 'error'
    } finally {
      uploadAbortRef.current.delete(key)
      uploadCancelReqRef.current.delete(key)
    }
  }

  /** 取消上传任务（面板行按钮）：进行中 → abort（api 层抛 AbortError）；
   *  排队中（顺序队列未轮到）→ 直接标记「已取消」，轮到时跳过。 */
  const cancelUploadRow = (key: number) => {
    uploadCancelReqRef.current.add(key)
    const ctrl = uploadAbortRef.current.get(key)
    if (ctrl) {
      ctrl.abort()
      return
    }
    setUploads((prev) =>
      prev.map((r) => (r.key === key && uploadPhaseActive(r.phase) ? { ...r, phase: 'canceled', error: undefined } : r)),
    )
  }

  const handleFilesPicked = async (files: FileList | null) => {
    if (!uploadFn || !files || files.length === 0) return
    for (const file of Array.from(files)) {
      const key = ++uploadKey.current
      setUploads((prev) => [...prev, { key, name: file.name, phase: 'creating' }])
      await runTrackedUpload(file, currentFolderId, key)
    }
    if (fileInputRef.current) fileInputRef.current.value = ''
    await load(currentParent)
  }

  const [docCreating, setDocCreating] = useState(false)

  // 新建项规格：md（Markdown 文档）、dfrt（富文本文档 .dfrt，Tiptap JSON；
  // kind 键名 dfdoc 为历史命名，路由 /dfdoc 同名复用）、textfile（文本文件
  // 合一：TXT/代码/HTML 由用户填写的扩展名决定）、drawio、白板与 office
  // 模板；XMind 已移除（前端仅解析查看、不支持编辑，空白创建价值低）。
  const createSpec = createKind ? {
    md: { label: 'Markdown 文档', ext: '.md', content: '# 新文档\n', mime: 'text/markdown', route: 'markdown' },
    dfdoc: { label: '富文本文档', ext: '.dfrt', content: EMPTY_DFDOC_JSON, mime: 'application/json', route: 'dfdoc' },
    // 「文本文件」：扩展名由用户自带（默认建议 untitled.txt），MIME 与
    // 创建后路由按扩展名推断（见 TEXT_FILE_MIME / textFileRoute）。
    textfile: { label: '文本文件', ext: '', content: '', mime: '', route: '' },
    drawio: { label: 'draw.io', ext: '.drawio', content: EMPTY_DRAWIO_XML, mime: 'text/xml', route: 'drawio' },
    whiteboard: { label: '白板', ext: '.excalidraw', content: EMPTY_EXCALIDRAW_JSON, mime: 'application/json', route: 'excalidraw' },
    word: { label: 'Word', ext: '.docx', content: '', mime: '', route: 'edit' },
    spreadsheet: { label: 'Excel', ext: '.xlsx', content: '', mime: '', route: 'edit' },
    presentation: { label: 'PPT', ext: '.pptx', content: '', mime: '', route: 'edit' },
  }[createKind] : null

  const beginNamedCreate = (kind: NonNullable<typeof createKind>) => {
    const defaults: Record<NonNullable<typeof createKind>, string> = {
      md: '新文档.md', dfdoc: '新文档.dfrt', textfile: 'untitled.txt', drawio: '新图表.drawio',
      whiteboard: '新白板.excalidraw', word: '新文档.docx',
      spreadsheet: '新表格.xlsx', presentation: '新演示文稿.pptx',
    }
    setCreateKind(kind)
    setCreateName(defaults[kind])
    setCreateError('')
    setCreateMenuOpen(false)
  }

  const handleNamedCreate = async (e: FormEvent) => {
    e.preventDefault()
    if (!createKind || !createSpec || docCreating) return
    let name = createName.trim()
    if (!name) return
    if (createKind === 'textfile') {
      // 文本文件：必须自带扩展名（弹框占位符已提示支持类型）。
      if (!/\.[A-Za-z0-9]{1,8}$/.test(name)) {
        setCreateError('请填写包含扩展名的文件名（如 untitled.txt、index.html）')
        return
      }
    } else if (!name.toLowerCase().endsWith(createSpec.ext)) {
      name += createSpec.ext
    }
    if (/[/\\:*?"<>|]/.test(name)) {
      setCreateError('文件名不能包含 / \\ : * ? " < > |')
      return
    }
    // 文本文件按扩展名推断 MIME 与创建后打开的编辑器路由。
    const ext = name.slice(name.lastIndexOf('.') + 1).toLowerCase()
    const fileMime = createKind === 'textfile' ? (TEXT_FILE_MIME[ext] ?? 'text/plain') : createSpec.mime
    const openRoute = createKind === 'textfile' ? textFileRoute(name) : createSpec.route
    const child = window.open('about:blank', '_blank')
    setDocCreating(true)
    setCreateError('')
    const key = ++uploadKey.current
    setUploads((prev) => [...prev, { key, name, phase: 'creating' }])
    try {
      let fileID = ''
      if (createKind === 'word' || createKind === 'spreadsheet' || createKind === 'presentation') {
        const created = await createOfficeTemplate(createKind, currentFolderId, name)
        fileID = created.id
      } else if (uploadFn) {
        const created = await uploadFn(new File([createSpec.content], name, { type: fileMime }), currentFolderId, (phase) => {
          setUploads((prev) => prev.map((r) => (r.key === key ? { ...r, phase } : r)))
        })
        fileID = typeof created === 'object' && created !== null && 'file_id' in created
          ? String((created as { file_id?: string }).file_id ?? '') : ''
        // 全零 UUID（uuid.Nil）视为无目标文件，避免打开 /text/00000000-…。
        if (fileID === '00000000-0000-0000-0000-000000000000') fileID = ''
      }
      await load(currentParent)
      setCreateKind(null)
      if (fileID && child) child.location.href = `/${openRoute}/${fileID}`
      else child?.close()
    } catch (err) {
      child?.close()
      setCreateError(writeErrorText(err, '新建失败'))
      setUploads((prev) => prev.map((r) => r.key === key ? { ...r, phase: 'error', error: writeErrorText(err, '新建失败') } : r))
    } finally {
      setDocCreating(false)
    }
  }

  // 页面快捷键（v1.1）：n 新建文件夹 / u 上传 / Delete 删除选中 /
  // Escape 依次关弹窗（预览→标签→移动→新建文件夹），无弹窗时清空选择。
  // v1.2（设计 6.16.1）：Ctrl/Cmd+1 列表视图、Ctrl/Cmd+2 网格视图
  // （经 useHotkeys 的 'mod+' 白名单注册）。
  useHotkeys({
    'mod+1': () => changeViewMode('list'),
    'mod+2': () => changeViewMode('grid'),
    n: () => {
      if (createFolderFn && !searchMode) {
        setFolderOpen(true)
        setFolderName('')
        setFolderError('')
      }
    },
    u: () => {
      if (uploadFn && !searchMode) fileInputRef.current?.click()
    },
    Delete: () => {
      if (selected.size > 0 && !batchBusy) void handleBatchTrash()
    },
    Escape: () => {
      if (trashOpen) {
        // antd Modal 自身响应 Esc 关闭；此处拦截避免同时清空列表选择。
        return
      }
      if (ctxMenu) {
        setCtxMenu(null)
        return
      }
      if (createMenuOpen) {
        setCreateMenuOpen(false)
        return
      }
      if (cardMenuFor) {
        setCardMenuFor(null)
        return
      }
      if (batchShareOpen) {
        setBatchShareOpen(false)
        return
      }
      if (uploadPanelOpen) {
        setUploadPanelOpen(false)
        return
      }
      if (previewTarget) {
        closePreview()
        return
      }
      if (shareListOpen) {
        setShareListOpen(false)
        return
      }
      if (batchTagOpen) {
        setBatchTagOpen(false)
        return
      }
      if (dirPicker) {
        setDirPicker(null)
        return
      }
      if (tagModalTarget) {
        setTagModalTarget(null)
        return
      }
      if (propsTarget) {
        setPropsTarget(null)
        return
      }
      if (folderOpen) {
        setFolderOpen(false)
        return
      }
      if (selected.size > 0) setSelected(new Set())
    },
  })

  // 集成编辑/查看页统一在新窗口打开（独立窗口便于与文件列表并行操作，
  // 编辑器自身带「返回」：window.open 打开的窗口可直接关闭）。
  const openEditorWindow = (path: string, openMethod?: string) => {
    const url = new URL(path, window.location.origin)
    if (openMethod) url.searchParams.set('open', openMethod)
    url.searchParams.set('returnTo', `${window.location.pathname}${window.location.search}`)
    window.open(`${url.pathname}${url.search}`, '_blank', 'noopener')
  }

  // ---- 默认打开方式分发（openers.ts 枚举体系） ----

  /** 集成可用性对方式的门槛：office 须 OnlyOffice、drawio 须 draw.io embed。 */
  const methodEnabled = (m: string): boolean =>
    !(m === 'office' && !ooEnabled) && !(m === 'drawio' && !drawioEnabled)

  /** 该文件可用的查看方式（合法性 + 集成门槛过滤；首项即内置默认）。 */
  const viewChoicesFor = (name: string) => viewOptionsFor(extOf(name)).filter(methodEnabled)

  /** 该文件可用的编辑方式（合法性 + 集成门槛过滤；空 = 不支持编辑）。 */
  const editChoicesFor = (name: string) => editOptionsFor(extOf(name)).filter(methodEnabled)

  /**
   * 以指定方式在新窗口打开：
   * - kind=view → /view/by-path（?open=<方式> 强制查看器分发）；
   * - kind=edit → /edit/by-path（?open=<方式> 强制编辑器分发）。
   * 方式缺省或等于内置默认时省略 open 参数——by-path 页按扩展名自动
   * 分发与内置默认一致，且保留 mermaid/xml 嗅探等细化行为。
   */
  const openWithMethod = (item: FileItem, kind: 'view' | 'edit', method?: string) => {
    const builtin = builtinOpenWith(extOf(item.name))
    const effective = method ?? (kind === 'view' ? builtin.view : builtin.edit)
    const omit = effective === (kind === 'view' ? builtin.view : builtin.edit)
    openEditorWindow(routeFor(kind, item), omit ? undefined : effective)
  }

  // ---- 目录打包下载 ZIP（download.zip 流式归档，认证 fetch 转 blob 保存）。 ----

  const handleZipDownload = async (item: FileItem) => {
    if (zipBusyId) return
    setZipBusyId(item.id)
    setError('')
    try {
      await downloadFolderZip(item.id, item.name)
    } catch (err) {
      setError(err instanceof Error ? err.message : '目录打包下载失败')
    } finally {
      setZipBusyId(null)
    }
  }

  // ---- 目录/树形上传共用：按相对路径递归创建目录（已存在复用） ----

  /** 目录树保障器：dirPath（'a/b' 形式，''=当前目录）→ 目录 ID；目录先于
   * 子项创建，409 同名冲突视为成功（列出父目录定位既有目录）。 */
  const createDirEnsurer = () => {
    const dirIds = new Map<string, string>([['', currentFolderId ?? '']])
    const ensureDir = async (path: string): Promise<string> => {
      const known = dirIds.get(path)
      if (known !== undefined) return known
      const idx = path.lastIndexOf('/')
      const parentPath = idx >= 0 ? path.slice(0, idx) : ''
      const name = idx >= 0 ? path.slice(idx + 1) : path
      const parentId = await ensureDir(parentPath)
      let id = ''
      if (!createFolderFn) return parentId
      try {
        const created = await createFolderFn(name, parentId || null)
        id = (created as { id?: string } | null)?.id ?? ''
      } catch (err) {
        // 已存在（409 name conflict）视为成功：列出父目录定位同名目录。
        if (err instanceof ApiError && err.status === 409) {
          try {
            const { items: siblings } = await listItems(parentId || null)
            id = siblings.find((it) => it.type === 'folder' && it.name === name)?.id ?? ''
          } catch {
            /* 定位失败走下方抛出 */
          }
        }
        if (!id) throw err
      }
      dirIds.set(path, id)
      return id
    }
    return { dirIds, ensureDir }
  }

  // ---- 目录上传（webkitdirectory）：按 webkitRelativePath 重建目录树，
  //      目录先于子项创建；任务进上传面板（行内 phase 由 uploadFn 驱动）。 ----

  const handleDirPicked = async (files: FileList | null) => {
    if (!uploadFn || !createFolderFn || dirUpload || !files || files.length === 0) return
    if (dirInputRef.current) dirInputRef.current.value = ''
    const list = Array.from(files)
    const relOf = (f: File) => (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name
    const firstRel = relOf(list[0])
    // 目录上传的目标根 = 当前目录下与所选目录同名的首段文件夹；无目录段时直接入当前目录。
    const rootName = firstRel.includes('/') ? firstRel.split('/')[0] : '目录'
    const failures: Array<{ path: string; reason: string }> = []
    const { ensureDir } = createDirEnsurer()
    const rootId = currentFolderId ?? ''

    setDirResult(null)
    setDirUpload({ done: 0, total: list.length })
    let ok = 0
    for (let i = 0; i < list.length; i++) {
      const file = list[i]
      const rel = relOf(file)
      const segments = rel.split('/').filter(Boolean)
      const dirPath = segments.slice(0, -1).join('/')
      try {
        const parentId = dirPath ? await ensureDir(dirPath) : rootId
        await uploadFn(file, parentId || null, () => {})
        ok++
      } catch (err) {
        failures.push({ path: rel, reason: writeErrorText(err, '上传失败') })
      }
      setDirUpload({ done: i + 1, total: list.length })
    }
    setDirUpload(null)
    setDirResult({ root: rootName, ok, failures })
    await load(currentParent)
  }

  // ---- 拖拽上传（v1.6）：拖文件/文件夹到文件管理区 → 上传到当前目录。
  //      webkitGetAsEntry 递归展开目录（须在事件处理器内同步取 entry，
  //      DataTransferItemList 异步后失效）；每文件一条上传任务进上传面板。 ----

  /** 递归读取 DataTransfer 条目（文件 + 目录树）为 {file, relPath} 列表。 */
  const readDropEntry = async (entry: FileSystemEntry, path: string, out: Array<{ file: File; relPath: string }>): Promise<void> => {
    if (entry.isFile) {
      const file = await new Promise<File | null>((resolve) => {
        ;(entry as FileSystemFileEntry).file(resolve, () => resolve(null))
      })
      if (file) out.push({ file, relPath: path ? `${path}/${file.name}` : file.name })
      return
    }
    if (!entry.isDirectory) return
    const reader = (entry as FileSystemDirectoryEntry).createReader()
    const dirPath = path ? `${path}/${entry.name}` : entry.name
    for (;;) {
      const batch = await new Promise<FileSystemEntry[]>((resolve) => {
        reader.readEntries(resolve, () => resolve([]))
      })
      if (batch.length === 0) break
      for (const child of batch) await readDropEntry(child, dirPath, out)
    }
  }

  const canDropUpload = Boolean(uploadFn) && !searchMode

  const handleDrop = async (e: ReactDragEvent<HTMLDivElement>) => {
    e.preventDefault()
    dragDepthRef.current = 0
    setDropActive(false)
    if (!uploadFn || searchMode) return
    // 同步取 entry（异步遍历在持有 entry 对象后进行）；无 entry 支持时回退 files。
    const entries = Array.from(e.dataTransfer?.items ?? [])
      .map((it) => (typeof it.webkitGetAsEntry === 'function' ? it.webkitGetAsEntry() : null))
      .filter((en): en is FileSystemEntry => en !== null)
    const dropped: Array<{ file: File; relPath: string }> = []
    if (entries.length > 0) {
      for (const en of entries) await readDropEntry(en, '', dropped)
    } else {
      for (const f of Array.from(e.dataTransfer?.files ?? [])) dropped.push({ file: f, relPath: f.name })
    }
    if (dropped.length === 0) return
    const { ensureDir } = createDirEnsurer()
    let ok = 0
    const failed: string[] = []
    for (const { file, relPath } of dropped) {
      const segments = relPath.split('/').filter(Boolean)
      const dirPath = segments.slice(0, -1).join('/')
      const key = ++uploadKey.current
      setUploads((prev) => [...prev, { key, name: relPath, phase: 'creating' }])
      if (skipCanceledRow(key)) continue
      try {
        const parentId = dirPath ? await ensureDir(dirPath) : currentFolderId ?? ''
        const r = await runTrackedUpload(file, parentId || null, key)
        if (r === 'ok') ok++
        else if (r === 'error') failed.push(relPath)
      } catch (err) {
        const reason = writeErrorText(err, '上传失败')
        failed.push(relPath)
        setUploads((prev) =>
          prev.map((r) => (r.key === key ? { ...r, phase: 'error', error: reason } : r)),
        )
      }
    }
    setBatchNotice('')
    setBatchError(failed.length > 0
      ? `拖拽上传：成功 ${ok} 个，失败 ${failed.length} 个（${failed.slice(0, 5).join('；')}${failed.length > 5 ? ' 等' : ''}）`
      : '')
    if (failed.length === 0) setBatchNotice(`拖拽上传完成：${ok} 个文件`)
    await load(currentParent)
  }

  // 默认单击：文件 → 弹窗查看（zip 网页包由查看分发器自动切 web 预览）；
  // 网页目录（has_index_web，或名称含「网页」的目录启发式）→ 网页预览弹窗
  //（无 index.html 时静默回退进入）；普通目录 → 进入该目录。
  const isWebFolder = (item: FileItem): boolean =>
    item.type === 'folder' && (Boolean(item.has_index_web) || item.name.includes('网页'))

  const openItem = (item: FileItem) => {
    if (item.type === 'folder') {
      if (searchMode) return
      if (isWebFolder(item)) {
        // 树导航的程序化步进点击（treeNavClick.armed）走「进入」而非预览。
        if (treeNavClick.armed) {
          openFolder(item)
          return
        }
        void openWebFolderPreview(item, !item.has_index_web)
        return
      }
      openFolder(item)
      return
    }
    openPreview(item)
  }

  /**
   * 查看弹窗标题行操作区（与标题同行，左标题右操作）：
   * - 网页目录：「作为网页打开（新窗口）」；
   * - 文件：「新窗口查看」拆分按钮（主点击=默认查看 by-path；下拉可选
   *   查看 Office / 查看网页 / 查看文本 / 下载，按文件类型显示适用项）
   *   +「编辑」拆分按钮（主点击=默认编辑；下拉可选 编辑 Office / 编辑文本 /
   *   图表编辑 / 白板编辑，无适用编辑方式的类型不渲染）。
   */
  const previewHeadExtra = (item: FileItem) => {
    if (item.type === 'folder') {
      return (
        <Button
          size="small"
          title={locale === 'zh-CN' ? '在独立窗口打开该静态网站' : 'Open this site in a new window'}
          onClick={() => void openAsWebsite(item)}
        >
          {locale === 'zh-CN' ? '作为网页打开（新窗口）' : 'Open as website'}
        </Button>
      )
    }
    const lower = item.name.toLowerCase()
    const isOffice = isOfficeFile(lower)
    const isHtml = isHtmlFile(lower)
    const isTxtLike = isTextEditable(lower) || isCodeFile(lower)
    const isDrawio = isDrawioFile(lower)
    const isBoard = isExcalidrawFile(lower)
    const isRichDoc = isDfdocFile(lower)
    const viewOptions: Array<{ label: string; run: () => void }> = []
    if (isOffice && ooEnabled) viewOptions.push({ label: locale === 'zh-CN' ? '查看 Office' : 'View Office', run: () => openEditorWindow(routeFor('view', item)) })
    if (isHtml) viewOptions.push({ label: locale === 'zh-CN' ? '查看网页' : 'View web', run: () => openEditorWindow(routeFor('view', item)) })
    // 查看文本走 /view 只读分发（by-path），绝不进 /text 编辑页（查看=纯渲染）。
    if (isTxtLike) viewOptions.push({ label: locale === 'zh-CN' ? '查看文本' : 'View text', run: () => openEditorWindow(routeFor('view', item)) })
    viewOptions.push({ label: msg('download'), run: () => void handleDownload(item) })
    const editOptions: Array<{ label: string; run: () => void }> = []
    if (isOffice && ooEnabled) editOptions.push({ label: locale === 'zh-CN' ? '编辑 Office' : 'Edit Office', run: () => openEditorWindow(routeFor('edit', item)) })
    if (isTxtLike) editOptions.push({ label: locale === 'zh-CN' ? '编辑文本' : 'Edit text', run: () => openEditorWindow(routeFor('edit', item)) })
    if (isDrawio && drawioEnabled) editOptions.push({ label: locale === 'zh-CN' ? '图表编辑' : 'Edit diagram', run: () => openEditorWindow(routeFor('edit', item)) })
    if (isBoard) editOptions.push({ label: locale === 'zh-CN' ? '白板编辑' : 'Edit whiteboard', run: () => openEditorWindow(routeFor('edit', item)) })
    // .dfrt/.dfdoc 富文本文档：编辑进 Tiptap（by-path 按扩展名分发）。
    if (isRichDoc) editOptions.push({ label: locale === 'zh-CN' ? '编辑富文本' : 'Edit rich text', run: () => openEditorWindow(routeFor('edit', item)) })
    // 默认编辑路由：office/drawio 集成未启用时回落只读查看（与 openFileWith 一致）。
    const editFallbackView = (isOffice && !ooEnabled) || (isDrawio && !drawioEnabled)
    const menuOf = (options: Array<{ label: string; run: () => void }>): MenuProps => ({
      items: options.map((op) => ({ key: op.label, label: op.label })),
      onClick: ({ key }) => options.find((op) => op.label === key)?.run(),
    })
    return (
      <>
        <Dropdown.Button
          size="small"
          menu={menuOf(viewOptions)}
          onClick={() => openEditorWindow(routeFor('view', item))}
        >
          {locale === 'zh-CN' ? '新窗口查看' : 'View in new window'}
        </Dropdown.Button>
        {editOptions.length > 0 && (
          <Dropdown.Button
            size="small"
            menu={menuOf(editOptions)}
            onClick={() => openEditorWindow(routeFor(editFallbackView ? 'view' : 'edit', item))}
          >
            {locale === 'zh-CN' ? '编辑' : 'Edit'}
          </Dropdown.Button>
        )}
      </>
    )
  }

  // ---- 条目重命名 / 删除（内置默认实现；宿主页可经 renameFn/deleteFn 覆盖，
  //      如默认空间的删除撤销横幅）。端点通用。 ----

  const builtinRename = async (item: FileItem) => {
    const name = await promptViaModal(antdModal, {
      title: locale === 'zh-CN' ? `重命名「${item.name}」` : `Rename “${item.name}”`,
      label: locale === 'zh-CN' ? '新名称' : 'New name',
      initialValue: item.name,
      okText: locale === 'zh-CN' ? '保存' : 'Save',
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
    })
    if (name === null || name === '' || name === item.name) return
    setError('')
    try {
      await renameFile(item.id, name)
      await load(currentParent)
    } catch (err) {
      setError(writeErrorText(err, '重命名失败'))
    }
  }

  const builtinDelete = (item: FileItem) => {
    antdModal.confirm({
      title: msg('delete'),
      content: locale === 'zh-CN'
        ? `确定删除「${item.name}」？可在回收站中恢复。`
        : `Delete “${item.name}”? You can restore it from trash.`,
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await deleteFile(item.id)
          await load(currentParent)
        } catch (err) {
          setError(writeErrorText(err, '删除失败'))
        }
      },
    })
  }

  // 条目操作菜单（列表行右键 /「⋯」、网格卡片菜单共用，antd Menu）：
  // 文件：查看（生效方式，新窗口）/ 编辑（生效方式，不支持或集成未启用时
  // 不显示）/ 打开方式 >（全部查看方式 + 分组线 + 全部编辑方式——仅集成
  // 未启用（office/drawio）的项灰显注明原因，其余（含对该扩展非法的方式）
  // 均可点，点击后按用户显式选择经 ?open= 强制分发）/ 下载 / 解压为目录
  //（.zip）/ 标签 / 复制 / 移动；目录：进入 / 下载为 ZIP / 网页预览（has_
  // index_web，弹窗）/ 作为网页打开 / 标签 / 复制（子树）/ 移动 / 分享 /
  // 重命名 / 删除（分享/重命名/删除经宿主页注入，缺省用内置实现）；
  // 调用方 rowActions（历史/权限等）追加在末位。默认单击文件为弹窗查看；
  // 网页目录单击为网页预览弹窗，普通目录单击进入。
  const itemMenuItems = (item: FileItem): MenuProps['items'] => {
    const zh = locale === 'zh-CN'
    const entries: NonNullable<MenuProps['items']> = []
    if (item.type === 'folder') {
      entries.push({
        key: 'open',
        label: zh ? '进入' : 'Open',
      })
      entries.push({
        key: 'zip',
        disabled: zipBusyId !== null,
        label: zipBusyId === item.id
          ? (zh ? '打包中…' : 'Zipping…')
          : (zh ? '下载为 ZIP' : 'Download as ZIP'),
      })
      if (ns && !searchMode && item.has_index_web) {
        entries.push({ key: 'preview-web', label: zh ? '网页预览（弹窗）' : 'Preview as website' })
        entries.push({ key: 'open-web', label: zh ? '作为网页打开' : 'Open as website' })
      }
    } else {
      // 查看方式：用户偏好合并内置默认；集成门槛（office/drawio）过滤。
      const effective = effectiveOpenWithFor(item.name, openWith)
      const viewMethods = viewChoicesFor(item.name)
      const editMethods = editChoicesFor(item.name)
      const effView = viewMethods.includes(effective.view) ? effective.view : viewMethods[0]
      if (effView) {
        entries.push({ key: 'view-default', label: zh ? `查看（${viewMethodLabel(effView, true, extOf(item.name))}）` : `View (${viewMethodLabel(effView, false, extOf(item.name))})` })
      }
      const effEdit = editMethods.includes(effective.edit) ? effective.edit : null
      if (effEdit) {
        entries.push({ key: 'edit-default', label: zh ? `编辑（${editMethodLabel(effEdit, true)}）` : `Edit (${editMethodLabel(effEdit, false)})` })
      }
      // 「打开方式 >」：全量查看方式组 + 分组线 + 全量编辑方式组——所有
      // 可见项均可点（不置灰；对该扩展非法的方式也可选，点击后按用户显式
      // 选择经 ?open= 强制分发，非法组合的兜底由查看分发层负责，如 raw 看
      // 二进制给下载提示）；仅集成未启用（office/drawio）的项隐藏。
      const ext = extOf(item.name)
      const av = { office: ooEnabled, drawio: drawioEnabled }
      const gated = (m: string) => (m === 'office' && !ooEnabled) || (m === 'drawio' && !drawioEnabled)
      const viewEntries = allViewEntries(ext, av, zh).filter(({ method }) => !gated(method))
      const editEntries = allEditEntries(ext, av, zh).filter(({ method }) => !gated(method))
      entries.push({
        key: 'openwith',
        label: zh ? '打开方式' : 'Open with',
        children: [
          {
            key: 'group-openwith-view',
            type: 'group' as const,
            label: zh ? '查看' : 'View',
            children: viewEntries.map(({ method }) => ({
              key: `openview:${method}`,
              label: `${zh ? '查看 · ' : 'View · '}${viewMethodLabel(method as Parameters<typeof viewMethodLabel>[0], zh, ext)}`,
            })),
          },
          {
            key: 'group-openwith-edit',
            type: 'group' as const,
            label: zh ? '编辑' : 'Edit',
            children: editEntries.map(({ method }) => ({
              key: `openedit:${method}`,
              label: `${zh ? '编辑 · ' : 'Edit · '}${editMethodLabel(method as Parameters<typeof editMethodLabel>[0], zh)}`,
            })),
          },
        ],
      })
      entries.push({ key: 'download', label: msg('download') })
      if (item.name.toLowerCase().endsWith('.zip')) {
        entries.push({
          key: 'unpack',
          disabled: unpackBusyId !== null,
          label: unpackBusyId === item.id
            ? (zh ? '解包中…' : 'Unpacking…')
            : (zh ? '解压为目录' : 'Unpack to folder'),
        })
      }
    }
    entries.push({ type: 'divider' })
    entries.push({ key: 'tag', label: msg('tag') })
    if (copyFn) {
      // 文件与目录均可复制（目录为后端子树深复制，目标可跨空间）。
      entries.push({ key: 'copy', label: msg('copy') })
    }
    entries.push({ key: 'move', label: zh ? '移动到…' : 'Move to…' })
    if (shareFn) {
      entries.push({ key: 'share', label: zh ? '分享…' : 'Share…' })
    }
    entries.push({ key: 'rename', label: zh ? '重命名…' : 'Rename…' })
    entries.push({ key: 'delete', label: msg('delete'), danger: true })
    entries.push({ key: 'properties', label: zh ? '属性' : 'Properties' })
    const extra = rowActions?.(item)
    if (extra) {
      entries.push({ type: 'divider' })
      entries.push({ key: 'row-actions', label: extra })
    }
    return entries
  }

  /** 菜单项点击分发（key 见 itemMenuItems）。 */
  const runItemMenuAction = (item: FileItem, key: string) => {
    switch (key) {
      case 'open':
        openFolder(item)
        return
      case 'zip':
        void handleZipDownload(item)
        return
      case 'open-web':
        void openAsWebsite(item)
        return
      case 'preview-web':
        void openWebFolderPreview(item)
        return
      case 'view-default':
        openWithMethod(item, 'view', effectiveOpenWithFor(item.name, openWith).view)
        return
      case 'edit-default':
        openWithMethod(item, 'edit', effectiveOpenWithFor(item.name, openWith).edit)
        return
      case 'unpack':
        void handleUnpack(item)
        return
      case 'download':
        void handleDownload(item)
        return
      case 'tag':
        void openTagModal(item)
        return
      case 'copy':
        openCopyPicker(item)
        return
      case 'move':
        openMovePicker(item)
        return
      case 'share':
        shareFn?.(item)
        return
      case 'rename':
        if (renameFn) renameFn(item)
        else void builtinRename(item)
        return
      case 'delete':
        ;(deleteFn ?? builtinDelete)(item)
        return
      case 'properties':
        void openProps(item)
        return
      default:
        if (key.startsWith('openview:')) {
          openWithMethod(item, 'view', key.slice('openview:'.length))
          return
        }
        if (key.startsWith('openedit:')) {
          openWithMethod(item, 'edit', key.slice('openedit:'.length))
        }
    }
  }

  /** 渲染条目操作菜单（antd Menu，透明背景由 .ctx-antd-menu 适配）；
   * 二级（打开方式）为 vertical 模式的独立 popup（挂 body，紧凑规格由
   * styles.css「浮层菜单统一 Win11 紧凑规格」全局节覆盖，与一级同视觉），
   * 自身带视口翻转定位；onOpenChange 后仍重跑一级菜单的视口收缩定位。 */
  const renderItemMenu = (item: FileItem) => (
    <Menu
      className="ctx-antd-menu"
      mode="vertical"
      selectable={false}
      items={itemMenuItems(item)}
      onClick={({ key }) => runItemMenuAction(item, key)}
      onOpenChange={() => {
        // 内联子菜单展开在下一帧才反映到 DOM，延迟一帧后重跑视口收缩。
        window.requestAnimationFrame(() => {
          if (ctxMenuRef.current) clampFixedMenu(ctxMenuRef.current, ctxMenu?.x ?? 0, ctxMenu?.y ?? 0)
        })
      }}
    />
  )

  // 全站唯一顶栏（v1.6.1 布局定稿）：左（空间切换 + 面包屑）｜弹性间隔｜
  // 右（目录搜索 → 视图切换 → 标签过滤 → 网格排序 → 新建 → 上传 → 上传
  // 任务 → 回收站）。搜索框与视图切换紧邻标签过滤之前（用户定稿顺序）。
  // 经 toolbarHost portal 渲染为全宽顶条；顶条本体无独立背景/边框（与页面
  // 融合，消除与全局 40px 顶栏的双栏观感，见 styles.css .files-topbar 节）；
  // 面包屑并入本行（工具元素与面包屑同层：左面包屑 + 右工具组）。
  // 控件统一 antd size="small"（24px 原生小尺寸，不做 height 拉伸）+ 13px 字号。
  const breadcrumbNav = !searchMode ? (
    <nav className="breadcrumb files-toolbar-crumbs">
      {crumbPrefix}
      {crumbs.map((crumb, index) => (
        <span key={crumb.id ?? 'root'} className="crumb">
          {index > 0 && <span className="sep">/</span>}
          <button
            className={index === crumbs.length - 1 ? 'current' : ''}
            onClick={() => gotoCrumb(index)}
          >
            {crumb.name}
          </button>
        </span>
      ))}
    </nav>
  ) : null

  // 上传任务按钮徽标：进行中（非终态）任务数。
  const activeUploadCount = uploads.filter((r) => uploadPhaseActive(r.phase)).length

  const toolbar = (
    <div className="files-toolbar toolbar-mini">
      {toolbarPrefix}
      {toolbarHost ? breadcrumbNav : null}
      <span className="toolbar-spacer" />
      {/* 目录搜索：宽屏常驻输入框；<1280px 收窄为图标按钮 + Popover 展开
          （同一 directoryQuery 状态，见 styles.css .files-toolbar 响应式节）。 */}
      <label className="filter-item directory-search">
        <Input
          size="small"
          allowClear
          value={directoryQuery}
          placeholder={locale === 'zh-CN' ? '搜索当前目录…' : 'Search this folder…'}
          onChange={(event) => setDirectoryQuery(event.target.value)}
        />
      </label>
      <Popover
        trigger="click"
        placement="bottomRight"
        open={searchPopOpen}
        onOpenChange={setSearchPopOpen}
        content={
          <Input
            size="small"
            allowClear
            autoFocus
            value={directoryQuery}
            placeholder={locale === 'zh-CN' ? '搜索当前目录…' : 'Search this folder…'}
            onChange={(event) => setDirectoryQuery(event.target.value)}
            style={{ width: 220 }}
          />
        }
      >
        <Button
          type="text"
          size="small"
          className="toolbar-search-narrow"
          title={locale === 'zh-CN' ? '搜索当前目录' : 'Search this folder'}
          aria-label={locale === 'zh-CN' ? '搜索当前目录' : 'Search this folder'}
          aria-expanded={searchPopOpen}
        >
          <Search size={14} strokeWidth={2} aria-hidden="true" />
        </Button>
      </Popover>
      {/* 列表/网格切换合一：单按钮按当前模式显示对侧图标（title 提示目标模式）。 */}
      <Button
        type="text"
        size="small"
        className="view-toggle-btn"
        title={viewMode === 'list' ? msg('viewModeGrid') : msg('viewModeList')}
        aria-label={viewMode === 'list' ? msg('viewModeGrid') : msg('viewModeList')}
        aria-pressed={viewMode === 'grid'}
        onClick={() => changeViewMode(viewMode === 'list' ? 'grid' : 'list')}
      >
        {viewMode === 'list'
          ? <LayoutGrid size={14} strokeWidth={2} aria-hidden="true" />
          : <List size={14} strokeWidth={2} aria-hidden="true" />}
      </Button>
      <label className="filter-item">
        <span>{msg('tag')}</span>
        <Select
          size="small"
          className="filter-select"
          value={tagFilter}
          onChange={(v) => { setTagFilter(v); setRecentView(false) }}
          options={[{ value: '', label: msg('all') }, ...tags.map((tg) => ({ value: tg.id, label: `#${tg.name}` }))]}
        />
      </label>
      {viewMode === 'grid' && (
        <label className="filter-item">
          <span>{msg('sort')}</span>
          <Select
            size="small"
            className="filter-select"
            value={`${sortKey}:${sortOrder}`}
            onChange={(v) => {
              const [key, order] = v.split(':')
              setSortKey(key as 'name' | 'updated_at' | 'size')
              setSortOrder(order as 'asc' | 'desc')
            }}
            options={[
              { value: 'name:asc', label: `${msg('sortOrderName')} ↑` },
              { value: 'name:desc', label: `${msg('sortOrderName')} ↓` },
              { value: 'updated_at:desc', label: `${msg('sortOrderUpdated')} ↓` },
              { value: 'updated_at:asc', label: `${msg('sortOrderUpdated')} ↑` },
              { value: 'size:desc', label: `${msg('sortOrderSize')} ↓` },
              { value: 'size:asc', label: `${msg('sortOrderSize')} ↑` },
            ]}
          />
        </label>
      )}
      {(createFolderFn || uploadFn) && !searchMode && (
        <Dropdown
          trigger={['click']}
          open={createMenuOpen}
          onOpenChange={setCreateMenuOpen}
          menu={{
            items: [
              ...(createFolderFn
                ? [{ key: 'folder', label: locale === 'zh-CN' ? '文件夹' : 'Folder' }]
                : []),
              ...uploadFn
                ? (['md', 'dfdoc', 'textfile', 'drawio', 'whiteboard', 'word', 'spreadsheet', 'presentation'] as const).map((kind) => ({
                    key: kind,
                    disabled: docCreating,
                    label: { md: 'Markdown 文档（.md）', dfdoc: locale === 'zh-CN' ? '富文本（.dfrt）' : 'Rich text（.dfrt）', textfile: '文本文件（.txt/.html/.js…）', drawio: 'draw.io', whiteboard: '白板', word: 'Word', spreadsheet: 'Excel', presentation: 'PPT' }[kind],
                  }))
                : [],
            ],
            onClick: ({ key }) => {
              if (key === 'folder') {
                setFolderOpen(true)
                setFolderName('')
                setFolderError('')
              } else {
                beginNamedCreate(key as NonNullable<typeof createKind>)
              }
            },
          }}
        >
          <Button size="small" icon={<Plus size={13} strokeWidth={2} aria-hidden="true" />}>
            {locale === 'zh-CN' ? '新建' : 'New'}
          </Button>
        </Dropdown>
      )}
      {/* 上传拆分按钮（antd Dropdown.Button）：主点击=上传文件；箭头下拉
          含「上传目录」（目录上传依赖建目录权限）。 */}
      {uploadFn && !searchMode && (
        <div className="create-menu-wrap">
          <Dropdown.Button
            size="small"
            type="primary"
            disabled={dirUpload !== null}
            menu={{
              items: [
                { key: 'files', label: locale === 'zh-CN' ? '上传文件' : 'Upload files' },
                ...(createFolderFn
                  ? [{ key: 'folder', label: locale === 'zh-CN' ? '上传目录' : 'Upload folder', disabled: dirUpload !== null }]
                  : []),
              ],
              onClick: ({ key }) => (key === 'files' ? fileInputRef.current?.click() : dirInputRef.current?.click()),
            }}
            onClick={() => fileInputRef.current?.click()}
          >
            <Upload size={13} strokeWidth={2} aria-hidden="true" />{' '}
            {dirUpload !== null ? (locale === 'zh-CN' ? '上传中…' : 'Uploading…') : locale === 'zh-CN' ? '上传' : 'Upload'}
          </Dropdown.Button>
          <input
            ref={fileInputRef}
            type="file"
            multiple
            hidden
            onChange={(e) => void handleFilesPicked(e.target.files)}
          />
          {createFolderFn && (
            <input
              ref={dirInputRef}
              type="file"
              multiple
              hidden
              onChange={(e) => void handleDirPicked(e.target.files)}
              {...({ webkitdirectory: '', directory: '' } as Record<string, string>)}
            />
          )}
        </div>
      )}
      {/* 上传任务入口（v1.6：浮条改工具栏按钮 + 徽标，点击弹窗查看任务列表）。 */}
      {uploads.length > 0 && (
        <Badge count={activeUploadCount} size="small" offset={[-2, 0]}>
          <Button
            size="small"
            title={locale === 'zh-CN' ? '上传任务' : 'Upload tasks'}
            onClick={() => setUploadPanelOpen(true)}
          >
            <Upload size={13} strokeWidth={2} aria-hidden="true" />
          </Button>
        </Badge>
      )}
      {/* 回收站入口（v1.5 弹窗化：原 /trash 整页路由已删除；hideToolbarTrash
          时由宿主在工具行左端自绘入口、经 trashSignal 受控打开）。 */}
      {!hideToolbarTrash && (
        <Button
          size="small"
          title={msg('trash')}
          onClick={() => setTrashOpen(true)}
        >
          <Trash2 size={13} strokeWidth={2} aria-hidden="true" /> {msg('trash')}
        </Button>
      )}
    </div>
  )

  return (
    <div className="file-browser">
      {toolbarHost ? createPortal(toolbar, toolbarHost) : toolbar}

      {/* 工具行之下的滚动内容区（三栏布局的中栏内部滚动）；v1.6 兼拖拽
          上传 drop zone（悬停高亮，见 .files-body.dropzone-active）。 */}
      <div
        className={`files-body${dropActive && canDropUpload ? ' dropzone-active' : ''}`}
        onDragEnter={(e) => {
          if (!canDropUpload) return
          e.preventDefault()
          dragDepthRef.current += 1
          setDropActive(true)
        }}
        onDragOver={(e) => {
          if (!canDropUpload) return
          e.preventDefault()
          if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy'
        }}
        onDragLeave={() => {
          if (!canDropUpload) return
          dragDepthRef.current = Math.max(0, dragDepthRef.current - 1)
          if (dragDepthRef.current === 0) setDropActive(false)
        }}
        onDrop={(e) => void handleDrop(e)}
      >
      {!searchMode && !toolbarHost && breadcrumbNav}
      {searchMode && (
        <div className="hint search-mode-hint">
          {recentView ? msg('recentHint') : msg('searchModeHint')}
        </div>
      )}

      {selected.size > 0 && (
        <div className="batch-bar">
          <span>{formatMessage(msg('selectedCount'), { n: selected.size })}</span>
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={() => openMovePicker(null)}>{msg('batchMove')}</Button>
          {copyFn && (
            <Button size="small" disabled={batchBusy || batchShareBusy} onClick={() => openCopyPicker(null)}>{msg('copy')}</Button>
          )}
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={() => void handleBatchDownload()}>
            {msg('batchDownload')}
          </Button>
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={openBatchShareDialog}>
            {batchShareBusy ? msg('loading') : msg('batchShare')}
          </Button>
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={openBatchTagDialog}>
            {msg('batchTag')}
          </Button>
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={() => void handleBatchStar()}>
            {batchBusy ? msg('loading') : allSelectedStarred ? msg('batchUnstar') : msg('batchStar')}
          </Button>
          <Button size="small" danger disabled={batchBusy || batchShareBusy} onClick={handleBatchTrash}>
            {msg('delete')}
          </Button>
          <Button type="text" size="small" onClick={() => setSelected(new Set())}>{msg('clearSelection')}</Button>
        </div>
      )}
      {batchNotice && <div className="banner ok">{batchNotice}</div>}
      {batchError && <div className="banner error">{batchError}</div>}

      {dirUpload && (
        <div className="banner dir-upload-progress">
          {locale === 'zh-CN'
            ? `目录上传中 已完成 ${dirUpload.done}/${dirUpload.total}`
            : `Uploading folder ${dirUpload.done}/${dirUpload.total}`}
        </div>
      )}
      {dirResult && (
        <div className={`banner ${dirResult.failures.length > 0 ? 'error' : 'ok'} dir-upload-result`}>
          <span>
            {locale === 'zh-CN'
              ? `「${dirResult.root}」上传完成：成功 ${dirResult.ok}/${dirResult.ok + dirResult.failures.length} 个文件`
              : `“${dirResult.root}” uploaded: ${dirResult.ok}/${dirResult.ok + dirResult.failures.length} files`}
            {dirResult.failures.length > 0 && (
              <>
                {locale === 'zh-CN' ? '；失败明细：' : '; failures: '}
                {dirResult.failures.slice(0, 10).map((f) => `${f.path}：${f.reason}`).join('；')}
                {dirResult.failures.length > 10
                  ? (locale === 'zh-CN' ? `；等共 ${dirResult.failures.length} 项` : `; ${dirResult.failures.length} in total`)
                  : ''}
              </>
            )}
          </span>
          <Button type="text" size="small" onClick={() => setDirResult(null)} aria-label={msg('close')}>×</Button>
        </div>
      )}

      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}
      {!loading && visibleItems.length === 0 && !error && (
        <div className="empty">{directoryQuery.trim() || searchMode ? msg('noMatch') : emptyHint ?? (locale === 'zh-CN' ? '此目录为空，上传文件或新建文件夹开始使用' : 'This folder is empty. Upload a file or create a folder to get started.')}</div>
      )}

      {visibleItems.length > 0 && viewMode === 'list' && (
        <table className="file-table">
          <thead>
            <tr>
              <th className="col-check">
                <input
                  type="checkbox"
                  checked={allSelected}
                  onChange={toggleSelectAll}
                  aria-label={msg('selectAll')}
                />
              </th>
              {/* 表头排序：整个单元格可点（th onClick），激活键高亮 + 方向箭头。 */}
              <th
                className={`th-sort-cell${sortKey === 'name' ? ' active' : ''}`}
                title={msg('sort')}
                onClick={() => toggleSort('name')}
              >
                <span className="th-sort">
                  {msg('name')}
                  {sortKey === 'name' && (
                    <span className="th-sort-arrow" aria-hidden="true">{sortOrder === 'asc' ? '↑' : '↓'}</span>
                  )}
                </span>
              </th>
              <th
                className={`th-sort-cell${sortKey === 'updated_at' ? ' active' : ''}`}
                title={msg('sort')}
                onClick={() => toggleSort('updated_at')}
              >
                <span className="th-sort">
                  {msg('sortOrderUpdated')}
                  {sortKey === 'updated_at' && (
                    <span className="th-sort-arrow" aria-hidden="true">{sortOrder === 'asc' ? '↑' : '↓'}</span>
                  )}
                </span>
              </th>
              <th className="col-actions">{msg('actions')}</th>
            </tr>
          </thead>
          <tbody>
            {visibleItems.map((item) => (
              <tr
                key={item.id}
                className={selected.has(item.id) ? 'selected' : ''}
                onContextMenu={(e) => {
                  e.preventDefault()
                  setCtxMenu({ item, x: e.clientX, y: e.clientY })
                }}
              >
                <td className="col-check">
                  <input
                    type="checkbox"
                    checked={selected.has(item.id)}
                    onChange={() => toggleSelect(item.id)}
                    aria-label={`${msg('selectItem')} ${item.name}`}
                  />
                </td>
                {/* 名称单元格整格可点（文件打开 / 网页目录预览 / 目录进入）；
                    星标按钮 stopPropagation 避免误触。 */}
                <td
                  className="name-cell"
                  onClick={() => !(item.type === 'folder' && searchMode) && openItem(item)}
                >
                  <button
                    className="star-btn"
                    title={item.is_starred ? '取消收藏' : '收藏'}
                    onClick={(e) => {
                      e.stopPropagation()
                      void toggleStar(item)
                    }}
                  >
                    <Star size={16} strokeWidth={2} aria-hidden="true" fill={item.is_starred ? 'currentColor' : 'none'} />
                  </button>
                  {item.type === 'folder' && !searchMode ? (
                    <button className="name-btn" onClick={(e) => { e.stopPropagation(); openItem(item) }}>
                      <span className="icon">{item.has_index_web
                        ? <Globe size={14} strokeWidth={2} aria-hidden="true" />
                        : <Folder size={14} strokeWidth={2} aria-hidden="true" />}</span>
                      {item.name}{item.has_index_web && <span className="web-folder-badge">网页</span>}
                    </button>
                  ) : item.type === 'folder' ? (
                    <span className="name-btn muted">
                      <span className="icon"><Folder size={14} strokeWidth={2} aria-hidden="true" /></span>
                      {item.name}
                    </span>
                  ) : (
                    <button className="name-btn" title={msg('preview')} onClick={(e) => { e.stopPropagation(); openItem(item) }}>
                      <span className="icon">{item.has_index_web
                        ? <Globe size={14} strokeWidth={2} aria-hidden="true" />
                        : <FileText size={14} strokeWidth={2} aria-hidden="true" />}</span>
                      {item.name}
                      {/* zip 网页包（解包就绪 = 含 index.html）打「网页」徽标，样式同目录。 */}
                      {item.has_index_web && <span className="web-folder-badge">网页</span>}
                    </button>
                  )}
                </td>
                <td className="muted">{formatTime(item.updated_at)}</td>
                <td className="col-actions">
                  {/* 操作收进「⋯」/右键菜单（操作项较多，不再平铺）。 */}
                  <Button
                    type="text"
                    size="small"
                    className="card-menu-btn"
                    title={msg('actions')}
                    aria-haspopup="menu"
                    onClick={(e) => {
                      const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
                      setCtxMenu({ item, x: rect.left, y: rect.bottom + 4 })
                    }}
                  >
                    <MoreHorizontal size={16} strokeWidth={2} aria-hidden="true" />
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {/* 网格视图（设计 6.3.7）：卡片 = 图标/文件名/大小/时间；文件夹卡片点击
          进入，文件卡片点击预览；右上「⋯」打开与列表行一致的操作菜单；
          选择状态与列表视图共享（多选 + 批量工具条两视图通用）。 */}
      {visibleItems.length > 0 && viewMode === 'grid' && (
        <div className="file-grid">
          {visibleItems.map((item) => {
            const size = sizeCache.current.get(item.id) ?? 0
            return (
              <div
                key={item.id}
                className={`file-card${selected.has(item.id) ? ' selected' : ''}`}
                onContextMenu={(e) => {
                  e.preventDefault()
                  setCtxMenu({ item, x: e.clientX, y: e.clientY })
                }}
              >
                <div className="file-card-top">
                  <input
                    type="checkbox"
                    checked={selected.has(item.id)}
                    onChange={() => toggleSelect(item.id)}
                    aria-label={`${msg('selectItem')} ${item.name}`}
                  />
                  <span className="file-card-top-actions">
                    <button
                      className="star-btn"
                      title={item.is_starred ? '取消收藏' : '收藏'}
                      onClick={() => void toggleStar(item)}
                    >
                      <Star size={16} strokeWidth={2} aria-hidden="true" fill={item.is_starred ? 'currentColor' : 'none'} />
                    </button>
                    <Button
                      type="text"
                      size="small"
                      className="card-menu-btn"
                      title={msg('actions')}
                      aria-haspopup="menu"
                      aria-expanded={cardMenuFor === item.id}
                      onClick={() => setCardMenuFor(cardMenuFor === item.id ? null : item.id)}
                    >
                      <MoreHorizontal size={16} strokeWidth={2} aria-hidden="true" />
                    </Button>
                  </span>
                </div>
                <button
                  className="file-card-body"
                  title={item.type === 'file' ? (locale === 'zh-CN' ? '查看' : 'View') : item.name}
                  onClick={() => openItem(item)}
                >
                  <span className="file-card-icon">{item.type === 'folder'
                    ? (item.has_index_web
                      ? <Globe size={22} strokeWidth={2} aria-hidden="true" />
                      : <Folder size={22} strokeWidth={2} aria-hidden="true" />)
                    : item.has_index_web
                      ? <Globe size={22} strokeWidth={2} aria-hidden="true" />
                      : <FileText size={22} strokeWidth={2} aria-hidden="true" />}</span>
                  <span className="file-card-name" title={item.name}>{item.name}{item.has_index_web && <span className="web-folder-badge">网页</span>}</span>
                  <span className="file-card-meta muted">
                    {size > 0 ? `${formatSize(size)} · ` : ''}
                    {formatTime(item.updated_at)}
                  </span>
                </button>
                {cardMenuFor === item.id && (
                  <div className="file-card-menu" role="menu" onClick={() => setCardMenuFor(null)}>
                    {renderItemMenu(item)}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}

      {/* 上传任务（v1.6）：浮条已移除——工具栏「上传任务」按钮（Badge 进行中
          数量）点击弹窗展示任务列表（复用同一 uploads 数据）。 */}
      </div>

      {folderOpen && createFolderFn && (
        <Modal title="新建文件夹" onClose={() => setFolderOpen(false)}>
          <form onSubmit={handleCreateFolder}>
            <label className="field">
              <span>名称</span>
              <Input autoFocus allowClear value={folderName} onChange={(e) => setFolderName(e.target.value)} placeholder="新文件夹" />
            </label>
            {folderError && <div className="error-text">{folderError}</div>}
            <div className="modal-actions">
              <Button onClick={() => setFolderOpen(false)}>取消</Button>
              <Button type="primary" htmlType="submit" disabled={!folderName.trim()}>创建</Button>
            </div>
          </form>
        </Modal>
      )}

      {createKind && createSpec && (
        <Modal title={`新建 ${createSpec.label}`} onClose={() => !docCreating && setCreateKind(null)}>
          <form onSubmit={handleNamedCreate}>
            <label className="field">
              <span>文件名</span>
              <Input autoFocus allowClear value={createName} onChange={(e) => setCreateName(e.target.value)} placeholder={`名称${createSpec.ext}`} />
            </label>
            <p className="hint">未填写 {createSpec.ext} 扩展名时会自动补全。</p>
            {createError && <div className="error-text">{createError}</div>}
            <div className="modal-actions">
              <Button disabled={docCreating} onClick={() => setCreateKind(null)}>取消</Button>
              <Button type="primary" htmlType="submit" disabled={docCreating || !createName.trim()}>{docCreating ? '创建中…' : '创建并打开'}</Button>
            </div>
          </form>
        </Modal>
      )}

      {/* 复制 / 移动目标目录选择器（懒加载目录树 + 单选 + 路径面包屑，
          替代手输 UUID；移动 = batch/move 部分成功语义，复制 = copyFn）。 */}
      <DirPickerModal
        open={dirPicker !== null}
        title={dirPicker
          ? (dirPicker.mode === 'copy'
            ? (dirPicker.item
              ? formatMessage(msg('copyTitle'), { name: dirPicker.item.name })
              : `${msg('copy')} ${selected.size} ${locale === 'zh-CN' ? '项' : 'items'}`)
            : dirPicker.item
              ? `移动「${dirPicker.item.name}」`
              : `移动 ${selected.size} 项`)
          : ''}
        rootLabel={rootLabel}
        listChildren={listChildrenForPicker}
        spaces={pickerSpaces}
        excludeId={dirPicker?.mode === 'move' && dirPicker.item ? dirPicker.item.id : undefined}
        busy={pickerBusy}
        errorText={pickerError}
        onCancel={() => {
          if (!pickerBusy) setDirPicker(null)
        }}
        onConfirm={(target) => void handleDirPickerConfirm(target)}
      />

      {shareListOpen && shareLinks.length > 0 && (
        <Modal wide title={msg('batchShareTitle')} onClose={() => setShareListOpen(false)}>
          <p className="hint">{msg('batchShareLinks')}</p>
          <div className="share-link-list">
            {shareLinks.map((link, index) => (
              <div key={link.url} className="share-link-item">
                <span className="share-link-name muted">{link.name}</span>
                <div className="share-link">
                  <Input readOnly value={link.url} onFocus={(e) => e.currentTarget.select()} />
                  <Button
                    size="small"
                    onClick={() => void copyShareLink(link.url, index)}
                  >
                    {copiedShareIdx === index ? msg('copied') : msg('copyLink')}
                  </Button>
                </div>
              </div>
            ))}
          </div>
          <div className="modal-actions">
            <Button onClick={() => setShareListOpen(false)}>{msg('close')}</Button>
          </div>
        </Modal>
      )}

      {/* 上传任务弹窗（v1.6：工具栏按钮入口；列表 = uploads 数据，进行中
          徽标计数；进行中任务可取消（排队中直接标记跳过 / 传输中 abort）；
          「清空已完成」保留进行中任务）。 */}
      {uploadPanelOpen && (
        <Modal title={locale === 'zh-CN' ? '上传任务' : 'Upload tasks'} onClose={() => setUploadPanelOpen(false)}>
          {uploads.length === 0 ? (
            <p className="hint">{locale === 'zh-CN' ? '暂无上传任务。' : 'No upload tasks.'}</p>
          ) : (
            <>
              <div className="upload-list">
                {uploads.map((row) => (
                  <div key={row.key} className="upload-row">
                    <span className="upload-name">{row.name}</span>
                    <span className={`badge ${row.phase}`}>{phaseText[row.phase]}</span>
                    {row.error && <span className="error-text">{row.error}</span>}
                    {uploadPhaseActive(row.phase) && (
                      <Button size="small" onClick={() => cancelUploadRow(row.key)}>
                        {locale === 'zh-CN' ? '取消' : 'Cancel'}
                      </Button>
                    )}
                  </div>
                ))}
              </div>
              <div className="modal-actions">
                <Button
                  disabled={activeUploadCount === uploads.length}
                  onClick={() => setUploads((prev) => prev.filter((r) => uploadPhaseActive(r.phase)))}
                >
                  {locale === 'zh-CN' ? '清空已完成' : 'Clear finished'}
                </Button>
                <Button onClick={() => setUploadPanelOpen(false)}>{msg('close')}</Button>
              </div>
            </>
          )}
        </Modal>
      )}

      {/* 批量分享弹窗（v1.6：打包一个链接）：选项与单项分享创建对齐；提交后
          经 shareLinks 结果弹窗展示唯一链接。 */}
      {batchShareOpen && (
        <Modal title={msg('batchShareTitle')} onClose={() => !batchShareBusy && setBatchShareOpen(false)}>
          <form onSubmit={handleBatchShareCreate}>
            <p className="hint">
              {selectedIds.length > 1
                ? (locale === 'zh-CN'
                  ? `将 ${selectedIds.length} 项打包为一个目录式分享链接：访问者可逐项预览/下载或整包下载。`
                  : `Bundle ${selectedIds.length} items into one directory-style share link.`)
                : (locale === 'zh-CN' ? '为所选内容创建公开分享链接。' : 'Create a public share link for the selection.')}
            </p>
            {selectedIds.length > 1 && (
              <label className="field">
                <span>{locale === 'zh-CN' ? '分享标题' : 'Share title'}</span>
                <Input
                  allowClear
                  maxLength={100}
                  value={batchShareTitle}
                  onChange={(e) => setBatchShareTitle(e.target.value)}
                  placeholder={locale === 'zh-CN' ? '打包分享的展示标题（默认按所选内容智能命名）' : 'Display title of the bundle'}
                />
              </label>
            )}
            <label className="field">
              <span>{locale === 'zh-CN' ? '权限' : 'Permission'}</span>
              <Select
                value={batchSharePermission}
                onChange={(v) => setBatchSharePermission(v as 'view' | 'download')}
                options={[
                  { value: 'download', label: locale === 'zh-CN' ? '可下载' : 'Download' },
                  { value: 'view', label: locale === 'zh-CN' ? '仅查看' : 'View only' },
                ]}
              />
            </label>
            <label className="field">
              <span>{locale === 'zh-CN' ? '有效期' : 'Expiry'}</span>
              <Select
                value={batchShareHours}
                onChange={(v) => setBatchShareHours(v)}
                options={[
                  { value: '0', label: locale === 'zh-CN' ? '永久' : 'Forever' },
                  { value: '1', label: locale === 'zh-CN' ? '1 小时' : '1 hour' },
                  { value: '24', label: locale === 'zh-CN' ? '24 小时' : '24 hours' },
                  { value: '168', label: locale === 'zh-CN' ? '7 天' : '7 days' },
                ]}
              />
            </label>
            <label className="field">
              <span>{locale === 'zh-CN' ? '最大下载次数（留空不限）' : 'Max downloads (empty = unlimited)'}</span>
              <Input
                type="number"
                min={1}
                allowClear
                value={batchShareMax}
                onChange={(e) => setBatchShareMax(e.target.value)}
                placeholder={locale === 'zh-CN' ? '不限' : 'Unlimited'}
              />
            </label>
            <label className="field">
              <span>{locale === 'zh-CN' ? '访问密码（留空不设密码，4-64 字符）' : 'Password (optional, 4-64 chars)'}</span>
              <Input.Password
                value={batchSharePassword}
                onChange={(e) => setBatchSharePassword(e.target.value)}
                placeholder={locale === 'zh-CN' ? '可选：访问者须输入密码' : 'Optional'}
                autoComplete="new-password"
              />
            </label>
            <div className="field">
              <span>{locale === 'zh-CN' ? '水印' : 'Watermark'}</span>
              <label className="check-item">
                <input
                  type="checkbox"
                  checked={batchShareWatermark}
                  onChange={(e) => setBatchShareWatermark(e.target.checked)}
                />
                <span>{locale === 'zh-CN' ? '公开访问页叠加斜排水印' : 'Overlay watermark on public pages'}</span>
              </label>
              {batchShareWatermark && (
                <Input
                  allowClear
                  style={{ marginTop: 8 }}
                  maxLength={256}
                  value={batchShareWatermarkText}
                  onChange={(e) => setBatchShareWatermarkText(e.target.value)}
                  placeholder={locale === 'zh-CN'
                    ? '水印内容（占位符：{user} 访问者 / {date} 日期 / {name} 文件名）'
                    : 'Watermark text ({user}/{date}/{name})'}
                />
              )}
            </div>
            {batchShareError && <div className="error-text">{batchShareError}</div>}
            <div className="modal-actions">
              <Button disabled={batchShareBusy} onClick={() => setBatchShareOpen(false)}>{msg('cancel')}</Button>
              <Button type="primary" htmlType="submit" disabled={batchShareBusy || selectedIds.length === 0}>
                {batchShareBusy ? msg('loading') : locale === 'zh-CN' ? '创建链接' : 'Create link'}
              </Button>
            </div>
          </form>
        </Modal>
      )}

      {batchTagOpen && (
        <Modal title={formatMessage(msg('batchTagTitle'), { n: selected.size })} onClose={() => setBatchTagOpen(false)}>
          <form onSubmit={handleBatchTag}>
            <label className="field">
              <span>{msg('tag')}</span>
              <Select
                autoFocus
                value={batchTagId}
                onChange={(v) => setBatchTagId(v)}
                options={tags.map((t) => ({ value: t.id, label: `#${t.name}` }))}
              />
            </label>
            {batchTagError && <div className="error-text">{batchTagError}</div>}
            <div className="modal-actions">
              <Button onClick={() => setBatchTagOpen(false)}>{msg('cancel')}</Button>
              <Button type="primary" htmlType="submit" disabled={batchTagBusy || !batchTagId}>
                {batchTagBusy ? msg('loading') : msg('apply')}
              </Button>
            </div>
          </form>
        </Modal>
      )}

      {tagModalTarget && (
        <Modal title={`标签「${tagModalTarget.name}」`} onClose={() => setTagModalTarget(null)}>
          <div className="tag-modal">
            {tags.length === 0 ? (
              <p className="hint">你还没有标签，先创建一个。</p>
            ) : (
              <div className="check-list">
                {tags.map((t) => (
                  <label key={t.id} className="check-item">
                    <input
                      type="checkbox"
                      disabled={tagModalBusy}
                      checked={tagModalFileTagIds.has(t.id)}
                      onChange={() => void toggleFileTag(t)}
                    />
                    <span>#{t.name}</span>
                  </label>
                ))}
              </div>
            )}
            <form className="tag-create" onSubmit={handleCreateTagAndAttach}>
              <Input
                allowClear
                value={tagModalNewName}
                onChange={(e) => setTagModalNewName(e.target.value)}
                placeholder="新标签名称（≤64 字符）"
                maxLength={64}
              />
              <Button htmlType="submit" disabled={tagModalBusy || !tagModalNewName.trim()}>
                创建并打标
              </Button>
            </form>
            {tagModalError && <div className="error-text">{tagModalError}</div>}
          </div>
        </Modal>
      )}

      {propsTarget && (
        <Modal
          title={locale === 'zh-CN' ? `属性「${propsTarget.name}」` : `Properties of “${propsTarget.name}”`}
          onClose={() => setPropsTarget(null)}
        >
          {propsError ? (
            <div className="error-text">{propsError}</div>
          ) : (
            <div className="props-list">
              <div className="props-row">
                <span className="muted">{locale === 'zh-CN' ? '名称' : 'Name'}</span>
                <span className="props-value" title={propsTarget.name}>{propsTarget.name}</span>
              </div>
              <div className="props-row">
                <span className="muted">{locale === 'zh-CN' ? '类型' : 'Type'}</span>
                <span className="props-value">
                  {propsTarget.type === 'folder'
                    ? (locale === 'zh-CN' ? '目录' : 'Folder')
                    : (locale === 'zh-CN' ? '文件' : 'File')}
                  {propsTarget.has_index_web ? (locale === 'zh-CN' ? '（网页）' : ' (web)') : ''}
                </span>
              </div>
              <div className="props-row">
                <span className="muted">{locale === 'zh-CN' ? '大小' : 'Size'}</span>
                <span className="props-value">
                  {propsLoading
                    ? '…'
                    : propsTarget.type === 'file' && propsMeta?.current_version
                      ? formatSize(propsMeta.current_version.size)
                      : '—'}
                </span>
              </div>
              <div className="props-row">
                <span className="muted">{locale === 'zh-CN' ? '创建时间' : 'Created'}</span>
                <span className="props-value">{formatTime(propsMeta?.created_at ?? propsTarget.created_at)}</span>
              </div>
              <div className="props-row">
                <span className="muted">{locale === 'zh-CN' ? '修改时间' : 'Modified'}</span>
                <span className="props-value">{formatTime(propsMeta?.updated_at ?? propsTarget.updated_at)}</span>
              </div>
              <div className="props-row">
                <span className="muted">{locale === 'zh-CN' ? '收藏' : 'Starred'}</span>
                <span className="props-value">
                  {propsTarget.is_starred ? (locale === 'zh-CN' ? '已收藏' : 'Yes') : (locale === 'zh-CN' ? '未收藏' : 'No')}
                </span>
              </div>
              <div className="props-row">
                <span className="muted">ID</span>
                <span className="props-value props-id" title={propsTarget.id}>{propsTarget.id}</span>
              </div>
            </div>
          )}
          <div className="modal-actions">
            <Button onClick={() => setPropsTarget(null)}>{msg('close')}</Button>
          </div>
        </Modal>
      )}

      {previewTarget && (
        <Modal
          wide
          className="modal-viewer"
          title={previewTarget.type === 'folder' ? `网页目录「${previewTarget.name}」` : `查看「${previewTarget.name}」`}
          onClose={closePreview}
          headExtra={previewHeadExtra(previewTarget)}
        >
          {/* 弹窗内容：网页目录 = sandbox iframe（raw_url）；其余类型（含
              office，内嵌 OnlyOffice 只读视图）统一经 FileViewerDispatch
              就地内嵌渲染，与独立查看页完全一致。 */}
          <div className="preview-embed">
            {previewTarget.type === 'folder' ? (
              webPreviewUrl ? (
                <iframe
                  className="standalone-viewer-frame"
                  sandbox="allow-scripts allow-forms allow-popups allow-modals"
                  src={webPreviewUrl}
                  title={previewTarget.name}
                />
              ) : (
                <div className="text-editor-state">正在加载网页…</div>
              )
            ) : (
              <FileViewerDispatch
                fileId={previewTarget.id}
                name={previewTarget.name}
                resolveRawUrl={async () => {
                  try {
                    const r = await resolveFileById(previewTarget.id, { mode: 'view' })
                    return r.raw_url
                  } catch {
                    return null
                  }
                }}
              />
            )}
          </div>
        </Modal>
      )}

      {/* 右键 / 列表行「⋯」菜单：视口定位浮层保留（初始按点击坐标，渲染后经
          clampFixedMenu 按实测尺寸收缩进视口），菜单面板换 antd Menu 视觉。 */}
      {ctxMenu && (
        <div
          ref={ctxMenuRef}
          className="ctx-menu"
          role="menu"
          style={{ left: `${ctxMenu.x}px`, top: `${ctxMenu.y}px` }}
          onClick={() => setCtxMenu(null)}
        >
          {renderItemMenu(ctxMenu.item)}
        </div>
      )}

      {/* 回收站弹窗（恢复/彻底删除/清空；操作成功后刷新当前目录）。 */}
      <TrashModal
        open={trashOpen}
        onClose={() => setTrashOpen(false)}
        onChanged={() => void load(currentParent)}
      />
    </div>
  )
}
