// DocFlow 前端 API 封装。
// - access_token 仅存内存（模块级变量），不写 localStorage；刷新页面后依赖 refresh cookie 恢复会话。
// - 统一处理 JSON 错误与 401 自动刷新重放；refresh 失败广播 SESSION_EXPIRED_EVENT 通知路由回登录页。

let accessToken: string | null = null

export function setAccessToken(token: string | null): void {
  accessToken = token
}

export function hasAccessToken(): boolean {
  return accessToken !== null
}

/** 会话过期事件：refresh 失败时广播，App 监听后跳转 /login。 */
export const SESSION_EXPIRED_EVENT = 'docflow:session-expired'

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
    this.name = 'ApiError'
  }
}

let refreshing: Promise<boolean> | null = null

/** 用 HttpOnly refresh cookie 换新 access token；并发调用共享同一次请求。 */
export async function refreshSession(): Promise<boolean> {
  if (refreshing) return refreshing
  const p = (async (): Promise<boolean> => {
    try {
      const res = await fetch('/api/v1/auth/refresh', { method: 'POST', credentials: 'same-origin' })
      if (!res.ok) return false
      const data = (await res.json()) as { access_token?: string }
      accessToken = data.access_token ?? null
      return accessToken !== null
    } catch {
      return false
    }
  })()
  refreshing = p
  try {
    return await p
  } finally {
    if (refreshing === p) refreshing = null
  }
}

async function rawFetch(path: string, init: RequestInit): Promise<Response> {
  const headers = new Headers(init.headers)
  if (accessToken) headers.set('Authorization', `Bearer ${accessToken}`)
  return fetch(path, { ...init, headers, credentials: 'same-origin' })
}

/** 认证请求：401 时自动 refresh 一次并重放原请求（body 需可重放：File/Blob/string）。 */
export async function authFetch(path: string, init: RequestInit = {}): Promise<Response> {
  let res = await rawFetch(path, init)
  if (res.status === 401) {
    const ok = await refreshSession()
    if (!ok) {
      accessToken = null
      window.dispatchEvent(new Event(SESSION_EXPIRED_EVENT))
      throw new ApiError(401, '会话已过期，请重新登录')
    }
    res = await rawFetch(path, init)
  }
  return res
}

/** UUID 校验（user_id / 团队目录等路径与表单输入共用）。 */
export const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/

/**
 * 解码 access token 的 sub（当前用户 UUID）。
 * 仅用于 UI 展示判断（如团队 owner 标识），权限校验始终由后端强制。
 */
export function currentUserId(): string | null {
  if (!accessToken) return null
  const payload = accessToken.split('.')[1]
  if (!payload) return null
  try {
    const claims = JSON.parse(atob(payload.replace(/-/g, '+').replace(/_/g, '/'))) as { sub?: string }
    return claims.sub ?? null
  } catch {
    return null
  }
}

async function parseBody(res: Response): Promise<unknown> {
  if (res.status === 204) return null
  const text = await res.text()
  if (!text) return null
  try {
    return JSON.parse(text)
  } catch {
    throw new ApiError(res.status, '响应解析失败')
  }
}

/** JSON API 请求：统一把 {error} 转成 ApiError 抛出。 */
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await authFetch(path, init)
  const data = await parseBody(res)
  if (!res.ok) {
    const obj = data as { error?: string; message?: string } | null
    throw new ApiError(res.status, obj?.error || obj?.message || `请求失败（${res.status}）`)
  }
  return data as T
}

function jsonInit(method: string, body: unknown): RequestInit {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }
}

// ---------- 类型 ----------

export interface FileItem {
  id: string
  name: string
  parent_id: string | null
  type: 'folder' | 'file'
  is_root: boolean
  description: string
  is_public: boolean
  view_count: number
  download_count: number
  created_at: string
  updated_at: string
  deleted_at?: string
}

// ---------- 文件版本 ----------

export type VersionStatus = 'created' | 'scanning' | 'available' | 'quarantined' | 'failed' | 'deleting'

/** GET /files/{id} 与回滚响应附带的当前版本摘要。 */
export interface FileVersionSummary {
  id: string
  version: number
  size: number
  sha256: string
  user_id?: string
  created_at?: string
}

export interface FileWithVersion extends FileItem {
  current_version: FileVersionSummary | null
}

/** GET /files/{id}/versions 的版本条目（按版本号倒序，内容不可变）。 */
export interface FileVersionDetail {
  id: string
  version: number
  size: number
  sha256: string
  mime_type: string
  status: VersionStatus
  comment: string | null
  user_id: string
  created_at: string
}

export type UploadStatus = 'uploading' | 'verifying' | 'scanning' | 'available' | 'quarantined' | 'failed'

export interface UploadSession {
  id: string
  parent_id: string
  name: string
  size: number
  offset: number
  status: UploadStatus
  /** 覆盖为新版本会话的目标文件 ID（仅 file_id 创建的会话返回）。 */
  file_id?: string | null
  expires_at: string
  created_at: string
  completed_at?: string | null
}

export interface ShareItem {
  id: string
  file_id: string
  permission: 'view' | 'download'
  expires_at: string | null
  max_downloads: number | null
  download_count: number
  revoked_at: string | null
  created_at: string
}

export interface CreatedShare extends ShareItem {
  /** 一次性明文 token（仅公开分享创建响应返回；私有分享为 null）。 */
  token: string | null
  share_url: string | null
}

// ---------- 认证 ----------

/** admin 角色探测缓存（见 isAdmin）；登录/登出后失效。 */
let adminProbe: Promise<boolean> | null = null

export async function login(email: string, password: string): Promise<void> {
  const data = await api<{ access_token: string }>('/api/v1/auth/login', jsonInit('POST', { email, password }))
  accessToken = data.access_token
  adminProbe = null
}

export async function logout(): Promise<void> {
  try {
    await authFetch('/api/v1/auth/logout', { method: 'POST' })
  } catch {
    // 登出失败也清空本地令牌
  } finally {
    accessToken = null
    adminProbe = null
  }
}

// ---------- 文件与回收站 ----------

export async function listFiles(parentId: string | null): Promise<FileItem[]> {
  const query = parentId ? `?parent_id=${encodeURIComponent(parentId)}` : ''
  const data = await api<{ files: FileItem[] }>(`/api/v1/files${query}`)
  return data.files ?? []
}

export async function createFolder(name: string, parentId: string | null): Promise<FileItem> {
  return api<FileItem>('/api/v1/folders', jsonInit('POST', { name, parent_id: parentId ?? '' }))
}

export async function renameFile(id: string, name: string): Promise<FileItem> {
  return api<FileItem>(`/api/v1/files/${id}`, jsonInit('PATCH', { name }))
}

export async function deleteFile(id: string): Promise<void> {
  await api(`/api/v1/files/${id}`, { method: 'DELETE' })
}

export async function listTrash(): Promise<FileItem[]> {
  const data = await api<{ files: FileItem[] }>('/api/v1/trash')
  return data.files ?? []
}

export async function restoreFile(id: string): Promise<FileItem> {
  return api<FileItem>(`/api/v1/files/${id}/restore`, { method: 'POST' })
}

export async function purgeFile(id: string): Promise<void> {
  await api(`/api/v1/trash/${id}`, { method: 'DELETE' })
}

// ---------- 下载（需带 Authorization，故用 fetch 转 blob 触发保存） ----------

/** 把响应 blob 以给定文件名触发浏览器保存。 */
function saveBlob(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

async function fetchBlob(path: string, failText: string): Promise<Blob> {
  const res = await authFetch(path)
  if (!res.ok) {
    const data = (await res.json().catch(() => null)) as { error?: string } | null
    throw new ApiError(res.status, data?.error ?? failText)
  }
  return res.blob()
}

export async function downloadFile(item: FileItem): Promise<void> {
  const blob = await fetchBlob(`/api/v1/files/${item.id}/download`, '下载失败')
  saveBlob(blob, item.name)
}

/** 私有分享下载（需 download 权限，成功计入分享下载次数）。 */
export async function downloadShareFile(shareId: string, fileId: string, name: string): Promise<void> {
  const blob = await fetchBlob(`/api/v1/shares/${shareId}/files/${fileId}/download`, '下载失败')
  saveBlob(blob, name)
}

// ---------- 预览（与后端白名单一致：image/* 除 SVG、pdf、text/plain、json、网页包） ----------

export type PreviewKind = 'image' | 'pdf' | 'text' | 'webpkg' | 'unsupported'

/** 按响应/元数据 MIME 判定前端渲染方式，规则与后端 previewResponse 白名单保持一致。 */
export function previewKind(mime: string): PreviewKind {
  const base = (mime || '').split(';')[0].trim().toLowerCase()
  if (base.startsWith('image/') && !base.startsWith('image/svg')) return 'image'
  if (base === 'application/pdf') return 'pdf'
  if (base === 'text/plain' || base === 'application/json') return 'text'
  return 'unsupported'
}

export interface PreviewContent {
  kind: PreviewKind
  mime: string
  /** image/pdf 的 blob URL（调用方负责 revoke）、text 的文本内容，或 webpkg 的内容入口 URL。 */
  url?: string
  text?: string
}

/** 识别网页包预览指引 JSON：{kind:"webpkg", url:"/content/<pid>/index.html"}。 */
function webpkgFromBody(text: string): string | null {
  try {
    const obj = JSON.parse(text) as { kind?: unknown; url?: unknown }
    if (obj.kind === 'webpkg' && typeof obj.url === 'string') return obj.url
  } catch {
    // 非对象 JSON（如普通 .json 文件的文本预览）：按文本处理
  }
  return null
}

/** 按预览响应构造渲染内容：webpkg JSON → 指引；415 → unsupported；其余按 Content-Type 分流。 */
async function previewFromResponse(res: Response): Promise<PreviewContent> {
  if (res.status === 415) {
    return { kind: 'unsupported', mime: '' }
  }
  if (!res.ok) {
    const data = (await res.json().catch(() => null)) as { error?: string } | null
    throw new ApiError(res.status, data?.error ?? '预览加载失败')
  }
  const mime = res.headers.get('Content-Type') ?? ''
  if (mime.startsWith('application/json')) {
    const text = await res.text()
    const webpkgUrl = webpkgFromBody(text)
    if (webpkgUrl) return { kind: 'webpkg', mime, url: webpkgUrl }
    return { kind: 'text', mime, text }
  }
  const kind = previewKind(mime)
  if (kind === 'text') {
    return { kind, mime, text: await res.text() }
  }
  const blob = await res.blob()
  return { kind, mime, url: URL.createObjectURL(blob) }
}

/** 认证预览：请求 /files/:id/preview，按响应 Content-Type 决定渲染方式；415 → unsupported。 */
export async function fetchPreview(fileId: string): Promise<PreviewContent> {
  const res = await authFetch(`/api/v1/files/${fileId}/preview`)
  return previewFromResponse(res)
}

/** 私有分享预览（view/download 权限均可，不消耗下载次数）。 */
export async function fetchSharePreview(shareId: string, fileId: string): Promise<PreviewContent> {
  const res = await authFetch(`/api/v1/shares/${shareId}/files/${fileId}/preview`)
  return previewFromResponse(res)
}

// ---------- 公开分享页 ----------

export interface PublicShareInfo {
  name: string
  size: number
  mime_type: string
  version: number
  version_status: string
  permission: 'view' | 'download'
  expires_at: string | null
  max_downloads: number | null
  download_count: number
}

/** 公开分享元数据（无认证）。 */
export async function getPublicShare(token: string): Promise<PublicShareInfo> {
  return api<PublicShareInfo>(`/api/v1/public/shares/${encodeURIComponent(token)}`)
}

/** 公开文本预览：直接抓取 /preview 响应体（text/plain、application/json）。 */
export async function fetchPublicPreviewText(token: string): Promise<string> {
  const res = await fetch(`/api/v1/public/shares/${encodeURIComponent(token)}/preview`)
  if (!res.ok) {
    throw new ApiError(res.status, '预览内容加载失败')
  }
  return res.text()
}

/**
 * 公开分享网页包预览：zip 文件存在 ready 解包结果时，/preview 返回
 * {kind:"webpkg", url:"/content/<pid>/index.html"}，此处取内容入口 URL；
 * 无 ready 包（415 或普通内容）时抛 ApiError，由调用方回退普通预览。
 */
export async function fetchPublicWebpkgPreview(token: string): Promise<string> {
  const res = await fetch(`/api/v1/public/shares/${encodeURIComponent(token)}/preview`)
  const data = (await res.json().catch(() => null)) as { kind?: string; url?: string } | null
  if (res.ok && data?.kind === 'webpkg' && typeof data.url === 'string') {
    return data.url
  }
  throw new ApiError(res.status || 415, '网页包预览不可用')
}

// ---------- 分享 ----------

export interface CreateShareOptions {
  fileId: string
  permission: 'view' | 'download'
  visibility: 'public' | 'private'
  /** 有效期（小时）；缺省或 0 表示永久。 */
  expiresInHours?: number
  /** 最大下载次数；缺省表示不限。 */
  maxDownloads?: number
  /** 私有分享：显式授权的用户列表（public 时必须为空）。 */
  userIds?: string[]
  /** 私有分享：授权的团队列表（public 时必须为空）。 */
  teamIds?: string[]
}

/**
 * 分享可见性/token 的前端内存态：契约约定列表与私有分享响应不回传 token 与
 * visibility（明文 token 仅公开分享创建时返回一次），故创建时暂存于内存 Map，
 * 供「我的分享」页展示链接与可见性；刷新页面后丢失（等价于“创建时已展示”）。
 */
export interface ShareMeta {
  visibility: 'public' | 'private'
  token?: string
}

const shareMetaById = new Map<string, ShareMeta>()

function rememberShareMeta(id: string, meta: ShareMeta): void {
  shareMetaById.set(id, meta)
}

export function getShareMeta(id: string): ShareMeta | null {
  return shareMetaById.get(id) ?? null
}

export async function createShare(opts: CreateShareOptions): Promise<CreatedShare> {
  const body: Record<string, unknown> = { file_id: opts.fileId, permission: opts.permission, visibility: opts.visibility }
  if (opts.expiresInHours && opts.expiresInHours > 0) body.expires_in = opts.expiresInHours * 3600 // 后端单位为秒
  if (opts.maxDownloads && opts.maxDownloads > 0) body.max_downloads = opts.maxDownloads
  if (opts.visibility === 'private') {
    if (opts.userIds?.length) body.user_ids = opts.userIds
    if (opts.teamIds?.length) body.team_ids = opts.teamIds
  }
  const created = await api<CreatedShare>('/api/v1/shares', jsonInit('POST', body))
  rememberShareMeta(created.id, { visibility: opts.visibility, token: created.token ?? undefined })
  return created
}

/** 我的分享列表（公开与私有，created_at 倒序）。 */
export async function listShares(): Promise<ShareItem[]> {
  const data = await api<{ shares: ShareItem[] }>('/api/v1/shares')
  return data.shares ?? []
}

/** 撤销分享（幂等，仅创建者）。 */
export async function revokeShare(id: string): Promise<void> {
  await api(`/api/v1/shares/${id}`, { method: 'DELETE' })
}

/** 单文件元数据（含当前版本摘要）；用于把分享记录的 file_id 解析为文件名，及版本历史的「当前版本」判定。 */
export async function getFileMeta(id: string): Promise<FileWithVersion> {
  return api<FileWithVersion>(`/api/v1/files/${id}`)
}

// ---------- 私有分享访问（登录用户，按显式授权） ----------

export interface ShareFileMeta {
  name: string
  size: number
  mime_type: string
  version: number
  version_status: string
  permission: 'view' | 'download'
  expires_at: string | null
  max_downloads: number | null
  download_count: number
}

/** 私有分享文件元数据（分享 owner 与被授权用户可访问）。 */
export async function getShareFileMeta(shareId: string, fileId: string): Promise<ShareFileMeta> {
  return api<ShareFileMeta>(`/api/v1/shares/${shareId}/files/${fileId}`)
}

// ---------- 团队 ----------

export interface Team {
  id: string
  name: string
  description: string
  owner_id: string
  created_at: string
}

export interface CreatedTeam extends Team {
  root_folder_id: string
}

export type TeamRole = 'owner' | 'editor' | 'viewer'

export interface TeamMember {
  user_id: string
  role: TeamRole
  created_at: string
  team_id?: string
}

/** 创建团队：创建者自动成为 owner 成员并生成团队根目录。 */
export async function createTeam(name: string, description: string): Promise<CreatedTeam> {
  return api<CreatedTeam>('/api/v1/teams', jsonInit('POST', { name, description }))
}

/** 我所在（成员或 owner）的团队列表。 */
export async function listTeams(): Promise<Team[]> {
  const data = await api<{ teams: Team[] }>('/api/v1/teams')
  return data.teams ?? []
}

export async function listTeamMembers(teamId: string): Promise<TeamMember[]> {
  const data = await api<{ members: TeamMember[] }>(`/api/v1/teams/${teamId}/members`)
  return data.members ?? []
}

/** 添加成员（仅团队 owner）。 */
export async function addTeamMember(teamId: string, userId: string, role: 'editor' | 'viewer'): Promise<TeamMember> {
  return api<TeamMember>(`/api/v1/teams/${teamId}/members`, jsonInit('POST', { user_id: userId, role }))
}

/** 移除成员（仅团队 owner；owner 成员不可移除）。 */
export async function removeTeamMember(teamId: string, userId: string): Promise<void> {
  await api(`/api/v1/teams/${teamId}/members/${userId}`, { method: 'DELETE' })
}

export interface TeamFileListing {
  files: FileItem[]
  /** 当前列出的目录 ID；parent_id 缺省查询时即团队根目录 ID。 */
  parent_id: string
}

/** 团队空间文件列表（parentId 为 null 表示团队根目录）。 */
export async function listTeamFiles(teamId: string, parentId: string | null): Promise<TeamFileListing> {
  const query = parentId ? `?parent_id=${encodeURIComponent(parentId)}` : ''
  return api<TeamFileListing>(`/api/v1/teams/${teamId}/files${query}`)
}

/** 在团队根目录（parentId 为 null）或指定团队目录下创建目录（editor 及以上角色）。 */
export async function createTeamFolder(teamId: string, name: string, parentId: string | null): Promise<FileItem> {
  return api<FileItem>(`/api/v1/teams/${teamId}/folders`, jsonInit('POST', { name, parent_id: parentId ?? '' }))
}

// ---------- 上传 ----------

export type UploadPhase = 'creating' | 'completing' | UploadStatus

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

/**
 * 会话后续流程：PATCH /uploads/:id（Upload-Offset 头 + 原始字节）→ POST complete
 * → 轮询状态至终态；available 之外抛 ApiError。uploadFile / uploadFileVersion 共用。
 */
async function runUploadSession(
  session: UploadSession,
  file: File,
  onProgress: (phase: UploadPhase) => void,
): Promise<UploadSession> {
  onProgress('uploading')
  const res = await authFetch(`/api/v1/uploads/${session.id}`, {
    method: 'PATCH',
    headers: { 'Upload-Offset': String(session.offset ?? 0), 'Content-Type': 'application/octet-stream' },
    body: file,
  })
  if (!res.ok) {
    const data = (await res.json().catch(() => null)) as { error?: string } | null
    throw new ApiError(res.status, data?.error ?? '上传数据失败')
  }

  onProgress('completing')
  let done = await api<UploadSession>(`/api/v1/uploads/${session.id}/complete`, { method: 'POST' })
  while (done.status === 'uploading' || done.status === 'verifying' || done.status === 'scanning') {
    onProgress(done.status)
    await sleep(800)
    done = await api<UploadSession>(`/api/v1/uploads/${session.id}`)
  }
  onProgress(done.status)
  if (done.status !== 'available') {
    throw new ApiError(409, done.status === 'quarantined' ? '文件未通过安全扫描，已被隔离' : `上传处理失败（${done.status}）`)
  }
  return done
}

/**
 * 一次性整文件上传（MVP 不做分块/哈希）：
 * POST /uploads 建会话 → PATCH /uploads/:id → POST complete → 轮询状态至终态。
 */
export async function uploadFile(
  file: File,
  parentId: string | null,
  onProgress: (phase: UploadPhase) => void,
): Promise<UploadSession> {
  onProgress('creating')
  const session = await api<UploadSession>('/api/v1/uploads', jsonInit('POST', { name: file.name, size: file.size, parent_id: parentId ?? '' }))
  return runUploadSession(session, file, onProgress)
}

/**
 * 覆盖为新版本：POST /uploads 携带 file_id（name/parent_id 被服务端忽略，沿用目标
 * 文件现有名称与父目录），Complete 后成为新版本并按 MAX_VERSIONS_PER_FILE 裁剪历史。
 */
export async function uploadFileVersion(
  file: File,
  fileId: string,
  onProgress: (phase: UploadPhase) => void,
): Promise<UploadSession> {
  onProgress('creating')
  const session = await api<UploadSession>('/api/v1/uploads', jsonInit('POST', { name: file.name, size: file.size, file_id: fileId }))
  return runUploadSession(session, file, onProgress)
}

// ---------- 文件版本 ----------

/** 版本列表（按版本号倒序；读权限同文件元数据：个人 owner、团队任意在册成员）。 */
export async function listFileVersions(fileId: string): Promise<FileVersionDetail[]> {
  const data = await api<{ versions: FileVersionDetail[] }>(`/api/v1/files/${fileId}/versions`)
  return data.versions ?? []
}

/** 回滚当前版本指针到既有版本（个人 owner、团队 editor+；版本内容不可变）。 */
export async function restoreVersion(fileId: string, versionId: string): Promise<FileWithVersion> {
  return api<FileWithVersion>(`/api/v1/files/${fileId}/versions/${versionId}/restore`, { method: 'POST' })
}

// ---------- ONLYOFFICE 编辑器 ----------

/** GET /onlyoffice/config：集成可用性与 DocumentServer 基地址。 */
export interface OnlyOfficeStatus {
  enabled: boolean
  /** DocumentServer 基地址（加载 api.js 脚本）；禁用时为 null。 */
  server_url: string | null
}

/** 编辑器配置（POST /onlyoffice/session 响应，可直接传 DocsAPI.DocEditor）。 */
export type OnlyOfficeEditorConfig = Record<string, unknown>

/** 仅 office 文档类型显示「编辑」入口（zip/图片等不支持在线编辑）。 */
const OFFICE_EXTS = new Set(['doc', 'docx', 'xls', 'xlsx', 'ppt', 'pptx', 'odt', 'ods', 'odp', 'csv', 'txt'])

export function isOfficeFile(name: string): boolean {
  const i = name.lastIndexOf('.')
  return i >= 0 && OFFICE_EXTS.has(name.slice(i + 1).toLowerCase())
}

/** 生成编辑会话配置（含 5 分钟有效的 document.url 签名地址与整体 JWT token）。 */
export async function createOnlyOfficeSession(fileId: string): Promise<OnlyOfficeEditorConfig> {
  return api<OnlyOfficeEditorConfig>('/api/v1/onlyoffice/session', jsonInit('POST', { file_id: fileId }))
}

// 集成可用性探测缓存（见 onlyOfficeStatus）：文件列表行「编辑」按钮与
// EditorPage 共用，按会话缓存一次；失败（含旧后端 404）保守视为未启用。
let onlyOfficeProbe: Promise<OnlyOfficeStatus> | null = null

/** 集成可用性探测：结果按会话缓存；请求失败时视为 {enabled:false}。 */
export function onlyOfficeStatus(): Promise<OnlyOfficeStatus> {
  if (!onlyOfficeProbe) {
    onlyOfficeProbe = api<OnlyOfficeStatus>('/api/v1/onlyoffice/config').catch(() => ({
      enabled: false,
      server_url: null,
    }))
  }
  return onlyOfficeProbe
}

// ---------- 管理端（仅 admin 角色） ----------

export type SettingType = 'bool' | 'int' | 'string'

export type SettingValue = boolean | number | string

/** GET /admin/settings 条目；value 为按 type 解析后的当前生效值（未设置时为默认值）。 */
export interface SettingItem {
  key: string
  value: SettingValue
  type: SettingType
  description: string
  default: SettingValue
  updated_at?: string
  updated_by?: string | null
}

export interface AdminStats {
  users: number
  files: number
  /** upload_sessions 表行数。 */
  uploads: number
  sessions: number
  shares: number
}

export async function adminGetSettings(): Promise<SettingItem[]> {
  const data = await api<{ settings: SettingItem[] }>('/api/v1/admin/settings')
  return data.settings ?? []
}

/** 更新单个设置（未知键 404、类型/范围不符 400、非 admin 403）；返回归一化后的值。 */
export async function adminPutSetting(key: string, value: SettingValue): Promise<SettingValue> {
  const data = await api<{ key: string; value: SettingValue }>(
    `/api/v1/admin/settings/${encodeURIComponent(key)}`,
    jsonInit('PUT', { value }),
  )
  return data.value
}

export async function adminGetStats(): Promise<AdminStats> {
  return api<AdminStats>('/api/v1/admin/stats')
}

/**
 * admin 角色探测：access token 的 JWT 仅含 sub/iat/exp（无 role 声明，后端
 * auth.Service.AccessToken 不注入角色），故降级为请求 GET /admin/stats 判定——
 * 200 视为 admin；403（非 admin）与其余错误（会话/网络）保守视为非 admin，
 * 不显示导航入口；直接访问 /admin 时页面内另行提示真实错误。
 * 结果按会话缓存，登录/登出后失效。
 */
export function isAdmin(): Promise<boolean> {
  if (!adminProbe) {
    adminProbe = adminGetStats()
      .then(() => true)
      .catch(() => false)
  }
  return adminProbe
}
