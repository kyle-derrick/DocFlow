import { useEffect, useMemo, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Lock, Trash2 } from 'lucide-react'
import { App as AntdApp, Button, Input, Segmented, Select } from 'antd'
import {
  ACLAction,
  CreatedShare,
  FileItem,
  FileQueryOptions,
  FolderACLEntry,
  SHARE_WATERMARK_DEFAULT,
  Space,
  SpaceRole,
  UserSearchResult,
  copyFile,
  createFolder,
  createShare,
  createSpaceFolder,
  currentUserId,
  deleteFile,
  fetchFileText,
  getFolderACL,
  getFileMeta,
  isDfdocFile,
  listFiles,
  listSpaceFiles,
  listSpaces,
  putFolderACL,
  renameFile,
  restoreFile,
  searchUsers,
  uploadFile,
} from '../api'
import { DirListing, Modal, formatTime } from '../components/FileBrowser'
import { FileBrowserWithTree } from '../components/FolderTreeNav'
import VersionHistoryModal from '../components/VersionHistoryModal'
import SpaceManageModal from '../components/SpaceManageModal'
import SpaceMemberPanel from '../components/SpaceMemberPanel'
import SpaceSwitcher from '../components/SpaceSwitcher'
import { useHotkeys } from '../useHotkeys'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

/** 权限动作 → i18n key（ACL 勾选表单共用）。 */
const PERM_LABEL_KEYS: Record<string, MessageKey> = {
  read: 'permRead',
  write: 'permWrite',
  delete: 'permDelete',
  share: 'permShare',
  admin: 'permAdmin',
}

/**
 * 扫描 .dfrt/.dfdoc（Tiptap JSON）内容收集引用的 file-id 集合：
 * docflowEmbed / docflowImage / docflowFileCard 三类引用节点的 attrs.fileId
 *（创建分享时经 ref_ids 并入 share grants，公开页按文件名解析引用资源）。
 * 解析失败/无引用返回空数组。
 */
function collectDfrtRefIds(text: string): string[] {
  try {
    const doc = JSON.parse(text) as Record<string, unknown> | null
    const ids = new Set<string>()
    const walk = (node: unknown): void => {
      if (!node || typeof node !== 'object') return
      const n = node as { type?: unknown; attrs?: { fileId?: unknown }; content?: unknown[] }
      if (n.type === 'docflowEmbed' || n.type === 'docflowImage' || n.type === 'docflowFileCard') {
        const id = n.attrs?.fileId
        if (typeof id === 'string' && id) ids.add(id)
      }
      if (Array.isArray(n.content)) n.content.forEach(walk)
    }
    walk(doc)
    return Array.from(ids)
  } catch {
    return []
  }
}

/** 路径级 ACL 权限动作清单（不含 admin，与后端 folder ACL 契约一致）。 */
const ACL_ACTIONS: readonly ACLAction[] = ['read', 'write', 'delete', 'share']

/** ACL 主体类型 → i18n key（条目表格与添加行共用；统一空间模型仅 user/space）。 */
const ACL_SUBJECT_KEYS: Record<string, MessageKey> = {
  user: 'aclSubjectUser',
  space: 'aclSubjectTeam',
}

/**
 * 文件页（统一空间模型单页，路由 /files?space=<id>）：
 * - 空间由 ?space= 查询参数选择（缺省 = 默认空间）；SpaceSwitcher 切换；
 * - 权限差异（五级内置角色矩阵）只体现在工具栏/菜单可用性：guest 隐藏
 *   新建/上传/复制（不注入 createFolderFn/uploadFn/copyFn），删除/重命名
 *   越权由后端 403 统一提示；
 * - owner/admin：工具栏「空间管理」综合弹窗（成员/用户组/邀请/空间设置，
 *   其余成员经空间下拉「管理空间」入口只读打开）+ 右侧可折叠成员栏（默认
 *   收起；guest 也可看）；owner：目录路径级 ACL 面板；
 * - 本页保留重命名 / 删除 / 分享 / 版本历史对话框（统一空间模型改造前的
 *   TeamSpacePage 已并入本页）。
 */
export default function FilesPage() {
  const locale = useLocale()
  const { modal: antdModal } = AntdApp.useApp()
  const msg = (key: MessageKey) => t(locale, key)
  const [searchParams] = useSearchParams()
  const spaceIdParam = searchParams.get('space') ?? ''

  const [spaces, setSpaces] = useState<Space[]>([])
  const [reloadKey, setReloadKey] = useState(0)
  const refresh = () => setReloadKey((k) => k + 1)
  // 全局视图（全部 / 收藏 / 最近）：驱动 FileBrowser 的检索模式。
  const [spaceView, setSpaceView] = useState<'all' | 'starred' | 'recent'>('all')

  // 空间加载（列表含 my_role；缺省定位默认空间）。reloadKey 一并联动——
  // 空间管理弹窗 onChanged 后 my_role/isOwner/配额用量即时刷新（转让
  // 所有权后设置类 tab 权限随之收敛）。
  useEffect(() => {
    void listSpaces().then(setSpaces).catch(() => setSpaces([]))
  }, [spaceIdParam, reloadKey])

  // 右侧成员栏（v2.2 起默认常开；工具栏「成员」切换按钮已移除）：
  // <1280px 视口自动收起回两栏。
  const [viewportNarrow, setViewportNarrow] = useState(() => window.matchMedia('(max-width: 1279.98px)').matches)
  useEffect(() => {
    const mq = window.matchMedia('(max-width: 1279.98px)')
    const onChange = () => setViewportNarrow(mq.matches)
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])
  const memberPanelVisible = !viewportNarrow

  // 回收站受控信号（v2.2：回收站按钮移到工具行左端空间切换旁，弹窗本体仍
  // 由 FileBrowser 渲染——seq 变化驱动打开）。
  const [trashSeq, setTrashSeq] = useState(0)

  const activeSpace = useMemo(() => {
    if (spaceIdParam) return spaces.find((s) => s.id === spaceIdParam) ?? null
    return spaces.find((s) => s.is_default) ?? null
  }, [spaces, spaceIdParam])
  const myRole: SpaceRole | null = activeSpace?.my_role ?? null
  const meId = currentUserId()
  const isOwner = activeSpace !== null && meId !== null && activeSpace.owner_id === meId
  // 写权限（矩阵 owner/admin/member_share/member）：guest 不注入写能力。
  const canWrite = myRole === null || myRole === 'owner' || myRole === 'admin' || myRole === 'member_share' || myRole === 'member'
  // 成员/用户组管理（owner/admin）。
  const canManage = myRole === null || myRole === 'owner' || myRole === 'admin'

  // 「空间管理」综合弹窗（owner/admin 全功能；其余成员经空间下拉入口只读）。
  const [manageOpen, setManageOpen] = useState(false)

  const [historyTarget, setHistoryTarget] = useState<FileItem | null>(null)

  const [renameTarget, setRenameTarget] = useState<FileItem | null>(null)
  const [renameValue, setRenameValue] = useState('')
  const [renameError, setRenameError] = useState('')
  const [deleteError, setDeleteError] = useState('')
  const [recentlyDeletedId, setRecentlyDeletedId] = useState<string | null>(null)
  const [undoSeconds, setUndoSeconds] = useState(0)
  useEffect(() => {
    if (!recentlyDeletedId) return
    setUndoSeconds(5)
    const timer = window.setInterval(() => setUndoSeconds((s) => Math.max(0, s - 1)), 1000)
    const expiry = window.setTimeout(() => setRecentlyDeletedId(null), 5000)
    return () => { window.clearInterval(timer); window.clearTimeout(expiry) }
  }, [recentlyDeletedId])

  // ---- 路径级 ACL（文件夹行「权限」按钮，owner 可见；保存整体 PUT 覆盖） ----
  const [aclTarget, setAclTarget] = useState<FileItem | null>(null)
  const [aclEntries, setAclEntries] = useState<FolderACLEntry[]>([])
  const [aclLoading, setAclLoading] = useState(false)
  const [aclBusy, setAclBusy] = useState(false)
  const [aclError, setAclError] = useState('')
  const [aclNotice, setAclNotice] = useState('')
  const [aclNewType, setAclNewType] = useState<FolderACLEntry['subject_type']>('user')
  const [aclNewEffect, setAclNewEffect] = useState<FolderACLEntry['effect']>('allow')
  const [aclNewPerms, setAclNewPerms] = useState<Record<string, boolean>>({ read: true })
  // ACL 主体选择：user = 远程搜索用户；space = 我的空间列表。
  const [aclSubjectUser, setAclSubjectUser] = useState('')
  const [aclSubjectQuery, setAclSubjectQuery] = useState('')
  const [aclSubjectOptions, setAclSubjectOptions] = useState<UserSearchResult[]>([])
  const [aclSubjectSearching, setAclSubjectSearching] = useState(false)
  const [aclSubjectSpace, setAclSubjectSpace] = useState('')

  useEffect(() => {
    const q = aclSubjectQuery.trim()
    if (q.length < 2 || aclSubjectUser) {
      setAclSubjectOptions([])
      return
    }
    const timer = window.setTimeout(() => {
      setAclSubjectSearching(true)
      void searchUsers(q)
        .then(setAclSubjectOptions)
        .catch(() => setAclSubjectOptions([]))
        .finally(() => setAclSubjectSearching(false))
    }, 300)
    return () => window.clearTimeout(timer)
  }, [aclSubjectQuery, aclSubjectUser])

  // 保存成功 toast：4 秒自动消失。
  useEffect(() => {
    if (!aclNotice) return
    const timer = window.setTimeout(() => setAclNotice(''), 4000)
    return () => window.clearTimeout(timer)
  }, [aclNotice])

  const openAcl = async (folder: FileItem) => {
    setAclTarget(folder)
    setAclEntries([])
    setAclError('')
    setAclLoading(true)
    setAclNewType('user')
    setAclSubjectUser('')
    setAclSubjectQuery('')
    setAclSubjectOptions([])
    setAclSubjectSpace('')
    setAclNewEffect('allow')
    setAclNewPerms({ read: true })
    try {
      setAclEntries(await getFolderACL(folder.id))
    } catch (err) {
      setAclError(err instanceof Error ? err.message : msg('loadFailed'))
    } finally {
      setAclLoading(false)
    }
  }

  const closeAcl = () => {
    if (aclBusy) return
    setAclTarget(null)
  }

  const addAclEntry = (e: FormEvent) => {
    e.preventDefault()
    const subjectId = aclNewType === 'user' ? aclSubjectUser : aclSubjectSpace
    if (!subjectId) {
      setAclError(aclNewType === 'user' ? (locale === 'zh-CN' ? '请先搜索并选择用户' : 'Search and select a user first') : msg('aclNoPerms'))
      return
    }
    const permissions = ACL_ACTIONS.filter((a) => aclNewPerms[a])
    if (permissions.length === 0) {
      setAclError(msg('aclNoPerms'))
      return
    }
    setAclEntries((prev) => [
      ...prev,
      { subject_type: aclNewType, subject_id: subjectId, effect: aclNewEffect, permissions: [...permissions] },
    ])
    setAclSubjectUser('')
    setAclSubjectQuery('')
    setAclSubjectOptions([])
    setAclSubjectSpace('')
    setAclError('')
  }

  const removeAclEntry = (index: number) => {
    setAclEntries((prev) => prev.filter((_, i) => i !== index))
  }

  const setAclEntryEffect = (index: number, effect: FolderACLEntry['effect']) => {
    setAclEntries((prev) => prev.map((entry, i) => (i === index ? { ...entry, effect } : entry)))
  }

  const toggleAclEntryPerm = (index: number, action: ACLAction) => {
    setAclEntries((prev) =>
      prev.map((entry, i) =>
        i === index
          ? {
              ...entry,
              permissions: entry.permissions.includes(action)
                ? entry.permissions.filter((a) => a !== action)
                : [...entry.permissions, action],
            }
          : entry,
      ),
    )
  }

  const handleSaveAcl = async () => {
    if (!aclTarget) return
    setAclBusy(true)
    setAclError('')
    try {
      await putFolderACL(aclTarget.id, aclEntries)
      setAclTarget(null)
      setAclNotice(msg('aclSaved'))
    } catch (err) {
      setAclError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setAclBusy(false)
    }
  }

  // ---- 分享 ----
  const [shareTarget, setShareTarget] = useState<FileItem | null>(null)
  const [shareVisibility, setShareVisibility] = useState<'public' | 'private'>('public')
  const [sharePermission, setSharePermission] = useState<'view' | 'download'>('download')
  const [shareHours, setShareHours] = useState('0')
  const [shareMax, setShareMax] = useState('')
  const [sharePassword, setSharePassword] = useState('')
  const [shareWatermark, setShareWatermark] = useState(true)
  const [shareWatermarkText, setShareWatermarkText] = useState('')
  const [shareUsers, setShareUsers] = useState<string[]>([])
  // 授权用户远程搜索（防抖 300ms，≥2 字触发）。
  const [shareUserQuery, setShareUserQuery] = useState('')
  const [shareUserOptions, setShareUserOptions] = useState<UserSearchResult[]>([])
  const [shareUserSearching, setShareUserSearching] = useState(false)
  // 已选用户的显示名缓存（id → 昵称/用户名），供多选框回显。
  const shareUserNameRef = useRef(new Map<string, string>())
  const [shareSpaces, setShareSpaces] = useState<string[]>([])
  const [shareResult, setShareResult] = useState<CreatedShare | null>(null)
  const [shareBusy, setShareBusy] = useState(false)
  const [shareError, setShareError] = useState('')
  const [copied, setCopied] = useState(false)
  // v2.7 引用资源：.dfrt/.dfdoc 分享时扫描出的引用 file-id 列表（随创建
  // 请求并入 share grants；弹窗展示「将包含 N 个引用资源」提示）。
  const [shareRefs, setShareRefs] = useState<string[]>([])

  // 授权用户远程搜索（私有分享）：防抖 + 最少 2 字。
  useEffect(() => {
    const q = shareUserQuery.trim()
    if (q.length < 2) {
      setShareUserOptions([])
      return
    }
    const timer = window.setTimeout(() => {
      setShareUserSearching(true)
      void searchUsers(q)
        .then((users) => {
          setShareUserOptions(users)
          for (const u of users) {
            shareUserNameRef.current.set(u.id, u.nickname ?? u.profile?.nickname ?? u.username)
          }
        })
        .catch(() => setShareUserOptions([]))
        .finally(() => setShareUserSearching(false))
    }, 300)
    return () => window.clearTimeout(timer)
  }, [shareUserQuery])

  const handleRename = async (e: FormEvent) => {
    e.preventDefault()
    if (!renameTarget) return
    const name = renameValue.trim()
    if (!name) return
    setRenameError('')
    try {
      await renameFile(renameTarget.id, name)
      setRenameTarget(null)
      refresh()
    } catch (err) {
      setRenameError(err instanceof Error ? err.message : '重命名失败')
    }
  }

  // 单项删除确认：antd Modal.confirm（回收站可恢复）。
  const confirmDelete = (item: FileItem) => {
    antdModal.confirm({
      title: locale === 'zh-CN' ? '删除' : 'Delete',
      content: locale === 'zh-CN'
        ? `确定删除「${item.name}」？可在回收站中恢复。`
        : `Delete “${item.name}”? You can restore it from the trash.`,
      okText: locale === 'zh-CN' ? '删除' : 'Delete',
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        setDeleteError('')
        try {
          await deleteFile(item.id)
          setRecentlyDeletedId(item.id)
          refresh()
        } catch (err) {
          setDeleteError(err instanceof Error ? err.message : '删除失败')
        }
      },
    })
  }

  const undoDelete = async () => {
    if (!recentlyDeletedId) return
    const id = recentlyDeletedId
    setRecentlyDeletedId(null)
    setUndoSeconds(0)
    try {
      await restoreFile(id)
      refresh()
    } catch (err) {
      setDeleteError(err instanceof Error ? err.message : '恢复失败')
    }
  }

  const openShare = (item: FileItem) => {
    setShareTarget(item)
    setShareVisibility('public')
    setSharePermission('download')
    setShareHours('0')
    setShareMax('')
    setSharePassword('')
    setShareWatermark(true)
    // v2.4：水印内容可编辑，默认模板与后端一致（{user} 访问者 {date} 日期
    // {name} 文件名；服务端渲染时替换占位符）。
    setShareWatermarkText(SHARE_WATERMARK_DEFAULT)
    setShareUsers([])
    setShareUserQuery('')
    setShareUserOptions([])
    setShareSpaces([])
    setShareResult(null)
    setShareError('')
    setCopied(false)
    // v2.7 引用资源：.dfrt/.dfdoc 扫描文档 JSON 收集引用 file-id（弹窗提示
    // 将包含的资源数，创建时并入 share grants）。
    setShareRefs([])
    if (item.type === 'file' && isDfdocFile(item.name)) {
      void fetchFileText(item.id)
        .then((text) => setShareRefs(collectDfrtRefIds(text)))
        .catch(() => setShareRefs([]))
    }
  }

  const toggleShareSpace = (spaceId: string) => {
    setShareSpaces((prev) => (prev.includes(spaceId) ? prev.filter((t) => t !== spaceId) : [...prev, spaceId]))
  }

  const handleCreateShare = async (e: FormEvent) => {
    e.preventDefault()
    if (!shareTarget) return
    setShareBusy(true)
    setShareError('')
    try {
      const hours = Number(shareHours) || 0
      const max = shareMax.trim() === '' ? undefined : Number(shareMax)
      const created = await createShare({
        fileId: shareTarget.id,
        permission: sharePermission,
        visibility: shareVisibility,
        expiresInHours: hours,
        maxDownloads: max,
        userIds: shareVisibility === 'private' ? shareUsers : undefined,
        spaceIds: shareVisibility === 'private' ? shareSpaces : undefined,
        password: shareVisibility === 'public' ? sharePassword.trim() : undefined,
        watermarkEnabled: shareWatermark,
        watermarkText: shareWatermarkText.trim() || undefined,
        refIds: shareRefs.length > 0 ? shareRefs : undefined,
      })
      setShareResult(created)
    } catch (err) {
      setShareError(err instanceof Error ? err.message : '创建分享失败')
    } finally {
      setShareBusy(false)
    }
  }

  // 分享链接指向前端公开分享页 /s/:token（无需登录即可访问）。
  const shareLink = shareResult?.token ? `${window.location.origin}/s/${shareResult.token}` : ''

  const copyLink = async () => {
    try {
      await navigator.clipboard.writeText(shareLink)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      setShareError('复制失败，请手动复制链接')
    }
  }

  const privateGrantCount = shareUsers.length + shareSpaces.length
  const sharePasswordLen = sharePassword.trim().length
  const submitDisabled =
    shareBusy ||
    (shareVisibility === 'private' && privateGrantCount === 0) ||
    (shareVisibility === 'public' && sharePasswordLen > 0 && (sharePasswordLen < 4 || sharePasswordLen > 64))

  // Escape 依次关本页对话框（ACL→版本历史→重命名→分享）；FileBrowser 的
  // 选择与内置弹窗由其自身 Escape 处理（见 FileBrowser）。
  useHotkeys({
    Escape: () => {
      if (aclTarget) {
        setAclTarget(null)
        return
      }
      if (historyTarget) {
        setHistoryTarget(null)
        return
      }
      if (renameTarget) {
        setRenameTarget(null)
        return
      }
      if (shareTarget) setShareTarget(null)
    },
  })

  // 列表/建目录按空间分派：非默认空间走 /spaces/:id 端点；缺省走 /files
  //（后端缺省即默认空间）。v2.6 race 修复：分派依据 spaceIdParam（URL，
  // 挂载时即确定）而非 activeSpace——后者经 listSpaces 异步加载，挂载瞬间
  // 为 null 会走默认空间端点，加载完成后无重载（列表仍是默认空间内容，
  // 再进目录即 folder not found）。
  const listItems = async (parentId: string | null, opts?: FileQueryOptions): Promise<DirListing> => {
    if (spaceIdParam) {
      const res = await listSpaceFiles(spaceIdParam, parentId, opts)
      return { items: res.files ?? [], folderId: res.parent_id }
    }
    return { items: await listFiles(parentId, opts), folderId: parentId }
  }
  const doCreateFolder = (name: string, parentId: string | null) =>
    spaceIdParam ? createSpaceFolder(spaceIdParam, name, parentId) : createFolder(name, parentId)

  return (
    <div className="page wide-page files-page">
      {deleteError && <div className="banner error">{deleteError}</div>}
      {aclNotice && <div className="banner ok">{aclNotice}</div>}
      {recentlyDeletedId && undoSeconds > 0 && (
        <div className="banner ok">
          {locale === 'zh-CN' ? `已删除，${undoSeconds} 秒内可撤销` : `Deleted. Undo within ${undoSeconds}s.`}
          <Button size="small" onClick={() => void undoDelete()}>{t(locale, 'undo')}</Button>
        </div>
      )}

      <FileBrowserWithTree
        /* v2.6 空间切换刷新（bug 修复）：以空间为 key 重挂载浏览器+树。
           此前 ?space= 变化只换 listItems 闭包，FileBrowser 首挂载后不再
           load、FolderTreeNav 的 nodes/expanded 与面包屑 crumbs 均保留旧
           空间内容（进旧目录 → folder not found）。key = URL space 参数
           （缺省 '__default__'，spaces 异步加载不引起二次重挂载）。 */
        key={spaceIdParam || '__default__'}
        rootLabel={activeSpace?.name ?? msg('teamsTitle')}
        /* 面包屑根目录短名（v2.6）：默认/非默认空间统一「根目录」，完整
           空间名由左侧空间切换器表达。 */
        rootCrumbLabel={locale === 'zh-CN' ? '根目录' : 'root'}
        treeRootLabel={activeSpace?.name ?? msg('teamsTitle')}
        reloadKey={reloadKey}
        toolbarPrefix={
          <>
            {/* v2.4：下拉底部「管理空间」入口已移除（空间管理统一入口 =
                右侧成员栏顶部按钮 / /spaces 卡片「管理」）。 */}
            <SpaceSwitcher
              activeView={spaceView}
              onViewChange={setSpaceView}
            />
            {/* 回收站（v2.2 左移到原「空间管理」位置；空间管理入口移到右侧
                成员栏顶部；「成员」切换按钮移除——成员栏默认展开）。 */}
            <Button
              size="small"
              title={msg('trash')}
              icon={<Trash2 size={13} strokeWidth={2} aria-hidden="true" />}
              onClick={() => setTrashSeq((s) => s + 1)}
            >
              {msg('trash')}
            </Button>
            {/* 窄屏无右侧成员栏时保留空间管理入口（owner/admin）。 */}
            {canManage && activeSpace && viewportNarrow && (
              <Button size="small" onClick={() => setManageOpen(true)}>
                {msg('spaceManage')}
              </Button>
            )}
          </>
        }
        aside={memberPanelVisible && activeSpace ? (
          <SpaceMemberPanel
            spaceId={activeSpace.id}
            reloadKey={reloadKey}
            canManage={canManage}
            onManage={() => setManageOpen(true)}
          />
        ) : undefined}
        listItems={listItems}
        createFolderFn={canWrite ? doCreateFolder : undefined}
        uploadFn={canWrite ? uploadFile : undefined}
        activeView={spaceView}
        /* 树点击目录导航退出检索模式（标签/收藏/最近）时把工具栏视图切回
           「全部」，保持 Segmented 高亮与中间列表一致。 */
        onViewReset={() => setSpaceView('all')}
        copyFn={canWrite ? ((fileId, parentId) => copyFile(fileId, parentId)) : undefined}
        fileMetaFn={(fileId) => getFileMeta(fileId).catch(() => null)}
        ns={spaceIdParam ? { type: 'space', scope: spaceIdParam } : undefined}
        /* 回收站入口在工具行左端（toolbarPrefix），弹窗本体经受控信号打开；
           隐藏 FileBrowser 工具行行尾的默认回收站按钮。 */
        hideToolbarTrash
        trashSignal={trashSeq}
        shareFn={openShare}
        renameFn={(item) => { setRenameTarget(item); setRenameValue(item.name); setRenameError('') }}
        deleteFn={confirmDelete}
        rowActions={(item) => (
          <div className="row-actions-group">
            {item.type === 'folder' && isOwner && (
              <Button size="small" onClick={() => void openAcl(item)}>{msg('acl')}</Button>
            )}
            {item.type === 'file' && (
              <Button size="small" onClick={() => setHistoryTarget(item)}>历史</Button>
            )}
          </div>
        )}
      />

      {historyTarget && (
        <VersionHistoryModal
          file={historyTarget}
          onClose={() => setHistoryTarget(null)}
          onChanged={refresh}
        />
      )}

      {renameTarget && (
        <Modal title={`重命名「${renameTarget.name}」`} onClose={() => setRenameTarget(null)}>
          <form onSubmit={handleRename}>
            <label className="field">
              <span>新名称</span>
              <Input autoFocus allowClear value={renameValue} onChange={(e) => setRenameValue(e.target.value)} />
            </label>
            {renameError && <div className="error-text">{renameError}</div>}
            <div className="modal-actions">
              <Button onClick={() => setRenameTarget(null)}>取消</Button>
              <Button type="primary" htmlType="submit" disabled={!renameValue.trim()}>保存</Button>
            </div>
          </form>
        </Modal>
      )}

      {shareTarget && (
        <Modal title={`分享「${shareTarget.name}」`} onClose={() => setShareTarget(null)}>
          {!shareResult ? (
            <form onSubmit={handleCreateShare}>
              <div className="field">
                <span>可见性</span>
                <Segmented
                  value={shareVisibility}
                  onChange={(v) => setShareVisibility(v as 'public' | 'private')}
                  options={[
                    { label: '公开链接', value: 'public' },
                    { label: '私有分享', value: 'private' },
                  ]}
                />
              </div>
              <label className="field">
                <span>权限</span>
                <Select
                  value={sharePermission}
                  onChange={(v) => setSharePermission(v as 'view' | 'download')}
                  options={[
                    { value: 'download', label: '可下载' },
                    { value: 'view', label: '仅查看' },
                  ]}
                />
              </label>
              {shareTarget.type === 'folder' && (
                <p className="hint share-vis-hint">
                  目录分享：访问者可浏览整个子树，在线预览或下载其中的文件（链接 /s/&lt;token&gt;）。
                </p>
              )}
              {shareRefs.length > 0 && (
                <p className="hint share-vis-hint">
                  将包含 {shareRefs.length} 个引用资源（文档内嵌的图片/图表/文件），访问者可直接查看。
                </p>
              )}

              {shareVisibility === 'public' ? (
                <p className="hint share-vis-hint">公开分享生成链接，任何拿到链接的人无需登录即可访问。</p>
              ) : (
                <>
                  <div className="field">
                    <span>授权用户</span>
                    {/* 远程搜索多选（昵称/用户名/邮箱 ≥2 字），替代手输 UUID。 */}
                    <Select
                      mode="multiple"
                      showSearch
                      allowClear
                      filterOption={false}
                      value={shareUsers}
                      loading={shareUserSearching}
                      placeholder="搜索昵称、用户名或邮箱（至少 2 字）"
                      notFoundContent={shareUserSearching ? '搜索中…' : null}
                      onSearch={setShareUserQuery}
                      onChange={setShareUsers}
                      options={[
                        // 选项 = 当前搜索结果 + 已选但不在结果中的用户（回显名）。
                        ...shareUserOptions.map((u) => {
                          const nickname = u.nickname ?? u.profile?.nickname
                          return { value: u.id, label: `${nickname || u.username}（${u.username}）` }
                        }),
                        ...shareUsers
                          .filter((id) => !shareUserOptions.some((u) => u.id === id))
                          .map((id) => ({ value: id, label: shareUserNameRef.current.get(id) ?? id })),
                      ]}
                    />
                  </div>
                  <div className="field">
                    <span>{msg('grantedAccess')}</span>
                    {spaces.length === 0 ? (
                      <p className="hint">{msg('teamsEmpty')}</p>
                    ) : (
                      <div className="check-list">
                        {spaces.map((s) => (
                          <label key={s.id} className="check-item">
                            <input
                              type="checkbox"
                              checked={shareSpaces.includes(s.id)}
                              onChange={() => toggleShareSpace(s.id)}
                            />
                            <span>{s.name}</span>
                          </label>
                        ))}
                      </div>
                    )}
                  </div>
                </>
              )}

              <label className="field">
                <span>有效期</span>
                <Select
                  value={shareHours}
                  onChange={(v) => setShareHours(v)}
                  options={[
                    { value: '0', label: '永久' },
                    { value: '1', label: '1 小时' },
                    { value: '24', label: '24 小时' },
                    { value: '168', label: '7 天' },
                  ]}
                />
              </label>
              <label className="field">
                <span>最大下载次数（留空不限）</span>
                <Input
                  type="number"
                  min={1}
                  allowClear
                  value={shareMax}
                  onChange={(e) => setShareMax(e.target.value)}
                  placeholder="不限"
                />
              </label>
              {shareVisibility === 'public' && (
                <label className="field">
                  <span>访问密码（留空不设密码，4-64 字符）</span>
                  <Input.Password
                    value={sharePassword}
                    onChange={(e) => setSharePassword(e.target.value)}
                    placeholder="可选：访问者须输入密码"
                    autoComplete="new-password"
                  />
                </label>
              )}
              <div className="field">
                <span>水印</span>
                <label className="check-item">
                  <input
                    type="checkbox"
                    checked={shareWatermark}
                    onChange={(e) => setShareWatermark(e.target.checked)}
                  />
                  <span>公开访问页叠加斜排水印（防截屏外传）</span>
                </label>
                {shareWatermark && (
                  <Input
                    allowClear
                    style={{ marginTop: 8 }}
                    maxLength={256}
                    value={shareWatermarkText}
                    onChange={(e) => setShareWatermarkText(e.target.value)}
                    placeholder="水印内容（占位符：{user} 访问者 / {date} 日期 / {name} 文件名）"
                  />
                )}
              </div>
              {shareError && <div className="error-text">{shareError}</div>}
              <div className="modal-actions">
                <Button onClick={() => setShareTarget(null)}>取消</Button>
                <Button type="primary" htmlType="submit" disabled={submitDisabled}>
                  {shareBusy ? '创建中…' : shareVisibility === 'public' ? '创建链接' : '创建私有分享'}
                </Button>
              </div>
            </form>
          ) : shareVisibility === 'public' ? (
            <div>
              <p className="hint">链接已创建。可立即复制，也可随时在「我的分享」中再次查看链接与密码。</p>
              <div className="share-link">
                <Input readOnly value={shareLink} onFocus={(e) => e.currentTarget.select()} />
                <Button type="primary" onClick={() => void copyLink()}>{copied ? '已复制 ✓' : '复制'}</Button>
              </div>
              {shareResult.has_password && (
                <p className="hint"><Lock size={14} strokeWidth={2} aria-hidden="true" /> 已启用密码保护：访问者须输入密码解锁（1 小时会话）。</p>
              )}
              {shareResult.watermark_enabled !== false && (
                <p className="hint">水印已开启{shareResult.watermark_text ? `（模板：${shareResult.watermark_text}）` : ''}。</p>
              )}
              {shareResult.expires_at && (
                <p className="hint">过期时间：{formatTime(shareResult.expires_at)}</p>
              )}
              {shareResult.max_downloads !== null && (
                <p className="hint">最大下载次数：{shareResult.max_downloads}</p>
              )}
              <div className="modal-actions">
                <Button onClick={() => setShareTarget(null)}>关闭</Button>
              </div>
            </div>
          ) : (
            <div>
              <p className="hint">私有分享已创建。被授权的用户与空间成员登录 DocFlow 后即可访问，无公开链接。</p>
              {shareResult.expires_at && (
                <p className="hint">过期时间：{formatTime(shareResult.expires_at)}</p>
              )}
              {shareResult.max_downloads !== null && (
                <p className="hint">最大下载次数：{shareResult.max_downloads}</p>
              )}
              <div className="modal-actions">
                <Button onClick={() => setShareTarget(null)}>关闭</Button>
              </div>
            </div>
          )}
        </Modal>
      )}

      {/* 空间管理综合弹窗（成员/用户组/邀请/空间设置）：owner/admin 全功能
          （邀请 / 改派 / 移除 / 用户组 / 配额 / 转让 / 解散），其余成员只读。 */}
      {manageOpen && activeSpace && (
        <SpaceManageModal
          space={activeSpace}
          myRole={myRole}
          isOwner={isOwner}
          onClose={() => setManageOpen(false)}
          onChanged={refresh}
        />
      )}

      {/* 路径级 ACL 管理：条目列表（主体/效果/权限勾选可改、删除行）+ 添加行，
          保存时整体 PUT 覆盖全部条目（主体仅 user/space）。 */}
      {aclTarget && (
        <Modal wide title={formatMessage(msg('aclTitle'), { name: aclTarget.name })} onClose={closeAcl}>
          <p className="hint">{msg('aclHint')}</p>
          {aclLoading ? (
            <p className="hint">{msg('loading')}</p>
          ) : (
            <>
              <div className="acl-list">
                <div className="acl-row acl-head">
                  <span>{msg('aclSubjectType')}</span>
                  <span>{msg('aclSubjectId')}</span>
                  <span>{msg('aclEffect')}</span>
                  <span>{msg('permission')}</span>
                  <span />
                </div>
                {aclEntries.length === 0 && <p className="hint">{msg('aclEmpty')}</p>}
                {aclEntries.map((entry, index) => (
                  <div key={index} className="acl-row">
                    <span className="acl-type">{msg(ACL_SUBJECT_KEYS[entry.subject_type] ?? 'aclSubjectUser')}</span>
                    <Input className="acl-uuid" readOnly value={entry.subject_id} title={entry.subject_id} />
                    <Select
                      className="acl-effect-select"
                      value={entry.effect}
                      disabled={aclBusy}
                      onChange={(v) => setAclEntryEffect(index, v as FolderACLEntry['effect'])}
                      options={[
                        { value: 'allow', label: msg('aclAllow') },
                        { value: 'deny', label: msg('aclDeny') },
                      ]}
                    />
                    <span className="acl-perms">
                      {ACL_ACTIONS.map((a) => (
                        <label key={a} className="check-item">
                          <input
                            type="checkbox"
                            checked={entry.permissions.includes(a)}
                            onChange={() => toggleAclEntryPerm(index, a)}
                          />
                          {msg(PERM_LABEL_KEYS[a])}
                        </label>
                      ))}
                    </span>
                    <Button size="small" danger disabled={aclBusy} onClick={() => removeAclEntry(index)}>
                      {msg('delete')}
                    </Button>
                  </div>
                ))}
              </div>
              <form className="acl-add" onSubmit={addAclEntry}>
                <Select
                  className="acl-type-select"
                  value={aclNewType}
                  onChange={(v) => setAclNewType(v as FolderACLEntry['subject_type'])}
                  options={[
                    { value: 'user', label: msg('aclSubjectUser') },
                    { value: 'space', label: msg('aclSubjectTeam') },
                  ]}
                />
                {aclNewType === 'user' ? (
                  <Select
                    className="acl-subject-select"
                    showSearch
                    allowClear
                    filterOption={false}
                    value={aclSubjectUser || undefined}
                    searchValue={aclSubjectQuery}
                    loading={aclSubjectSearching}
                    placeholder={locale === 'zh-CN' ? '搜索用户（昵称/用户名/邮箱，至少 2 字）' : 'Search users (2+ chars)'}
                    notFoundContent={aclSubjectSearching ? (locale === 'zh-CN' ? '搜索中…' : 'Searching…') : null}
                    onSearch={(v) => {
                      // antd 选中 option 后派发 onSearch('')：不清除已选中
                      // 主体；仅主动输入非空文本时重置。
                      setAclSubjectQuery(v)
                      if (v !== '') setAclSubjectUser('')
                    }}
                    onClear={() => {
                      setAclSubjectQuery('')
                      setAclSubjectUser('')
                    }}
                    onChange={(value, option) => {
                      if (value === undefined) return
                      setAclSubjectUser(String(value))
                      setAclSubjectQuery(String((option as { searchText?: string })?.searchText ?? ''))
                    }}
                    options={aclSubjectOptions.map((u) => {
                      const nickname = u.nickname ?? u.profile?.nickname
                      return {
                        value: u.id,
                        searchText: nickname || u.username,
                        label: (
                          <span className="user-search-option">
                            <strong>{nickname || u.username}</strong>
                            <span className="muted">{u.username} · {u.email}</span>
                          </span>
                        ),
                      }
                    })}
                  />
                ) : (
                  <Select
                    className="acl-subject-select"
                    value={aclSubjectSpace || undefined}
                    placeholder={locale === 'zh-CN' ? '选择空间' : 'Select space'}
                    onChange={(v) => setAclSubjectSpace(v)}
                    options={spaces.map((s) => ({ value: s.id, label: s.name }))}
                  />
                )}
                <Select
                  className="acl-effect-select"
                  value={aclNewEffect}
                  onChange={(v) => setAclNewEffect(v as FolderACLEntry['effect'])}
                  options={[
                    { value: 'allow', label: msg('aclAllow') },
                    { value: 'deny', label: msg('aclDeny') },
                  ]}
                />
                <span className="acl-perms">
                  {ACL_ACTIONS.map((a) => (
                    <label key={a} className="check-item">
                      <input
                        type="checkbox"
                        checked={Boolean(aclNewPerms[a])}
                        onChange={(e) => setAclNewPerms({ ...aclNewPerms, [a]: e.target.checked })}
                      />
                      {msg(PERM_LABEL_KEYS[a])}
                    </label>
                  ))}
                </span>
                <Button size="small" htmlType="submit" disabled={aclBusy || (aclNewType === 'user' ? !aclSubjectUser : !aclSubjectSpace)}>
                  {msg('aclAdd')}
                </Button>
              </form>
              {aclError && <div className="error-text">{aclError}</div>}
              <div className="modal-actions">
                <Button disabled={aclBusy} onClick={closeAcl}>{msg('close')}</Button>
                <Button type="primary" disabled={aclBusy} onClick={() => void handleSaveAcl()}>
                  {aclBusy ? msg('loading') : msg('save')}
                </Button>
              </div>
            </>
          )}
        </Modal>
      )}
    </div>
  )
}
