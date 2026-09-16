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

export function websocketToken(): string | null {
  return accessToken
}

/** 会话过期事件：refresh 失败时广播，App 监听后跳转 /login。 */
export const SESSION_EXPIRED_EVENT = 'docflow:session-expired'

export class ApiError extends Error {
  status: number
  /** 机器可读错误码（如 TOTP_REQUIRED），服务端未携带时缺省。 */
  code?: string
  constructor(status: number, message: string, code?: string) {
    super(message)
    this.status = status
    this.code = code
    this.name = 'ApiError'
  }
}

let refreshing: Promise<boolean> | null = null

/** 用 HttpOnly refresh cookie 换新 access token；并发调用共享同一次请求。 */
function csrfToken(): string | null {
  const match = document.cookie.match(/(?:^|; )docflow_csrf=([^;]*)/)
  return match ? decodeURIComponent(match[1]) : null
}

function withCSRF(init: RequestInit): RequestInit {
  const method = (init.method ?? 'GET').toUpperCase()
  if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    const headers = new Headers(init.headers)
    const token = csrfToken()
    if (token) headers.set('X-CSRF-Token', token)
    return { ...init, headers }
  }
  return init
}

export async function refreshSession(): Promise<boolean> {
  if (refreshing) return refreshing
  const p = (async (): Promise<boolean> => {
    try {
      const res = await fetch('/api/v1/auth/refresh', withCSRF({ method: 'POST', credentials: 'same-origin' }))
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
  init = withCSRF(init)
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

/** JSON API 请求：统一把 {error, code} 转成 ApiError 抛出。 */
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await authFetch(path, init)
  const data = await parseBody(res)
  if (!res.ok) {
    const obj = data as { error?: string; message?: string; code?: string } | null
    throw new ApiError(res.status, obj?.error || obj?.message || `请求失败（${res.status}）`, obj?.code)
  }
  return data as T
}

/** 匿名公开请求：不带 Authorization、不做 401 自动刷新（公开分享页专用，
 * 避免未登录访客的 401（如 PASSWORD_REQUIRED）触发会话过期跳转）。 */
export async function publicApi<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, { ...init, credentials: 'same-origin' })
  const data = await parseBody(res)
  if (!res.ok) {
    const obj = data as { error?: string; message?: string; code?: string } | null
    throw new ApiError(res.status, obj?.error || obj?.message || `请求失败（${res.status}）`, obj?.code)
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
  is_starred?: boolean
  view_count: number
  download_count: number
  created_at: string
  updated_at: string
  deleted_at?: string
}

// ---------- 标签与收藏 ----------

export interface Tag {
  id: string
  name: string
  created_at: string
}

/** 我的标签列表（按名称排序）。 */
export async function listTags(): Promise<Tag[]> {
  const data = await api<{ tags: Tag[] }>('/api/v1/tags')
  return data.tags ?? []
}

/** 创建标签（服务端 NFC 归一，≤64 rune，拒绝控制字符；同名 409）。 */
export async function createTag(name: string): Promise<Tag> {
  return api<Tag>('/api/v1/tags', jsonInit('POST', { name }))
}

/** 删除标签并解除其全部文件关联（仅创建者）。 */
export async function deleteTag(id: string): Promise<void> {
  await api(`/api/v1/tags/${id}`, { method: 'DELETE' })
}

/** 文件上属于当前用户的标签。 */
export async function listFileTags(fileId: string): Promise<Tag[]> {
  const data = await api<{ tags: Tag[] }>(`/api/v1/files/${fileId}/tags`)
  return data.tags ?? []
}

/** 给文件打标签（读权限即可；重复打幂等成功）。 */
export async function addFileTag(fileId: string, tagId: string): Promise<void> {
  await api(`/api/v1/files/${fileId}/tags`, jsonInit('POST', { tag_id: tagId }))
}

/** 解除文件标签（幂等）。 */
export async function removeFileTag(fileId: string, tagId: string): Promise<void> {
  await api(`/api/v1/files/${fileId}/tags/${tagId}`, { method: 'DELETE' })
}

/** 切换收藏（读权限即可；团队文件为行级共享星标）。 */
export async function setFileStarred(fileId: string, starred: boolean): Promise<FileItem> {
  return api<FileItem>(`/api/v1/files/${fileId}/starred`, jsonInit('PATCH', { starred }))
}

/** 列表查询选项：标签/收藏过滤（进入跨目录检索模式）、最近访问与排序。 */
export interface FileQueryOptions {
  tagId?: string | null
  starred?: boolean
  /** true 时走 ?recent=true：最近访问文件（last_access_at 倒序，服务端忽略其余过滤与 parent_id）。 */
  recent?: boolean
  sort?: 'name' | 'updated_at' | 'size'
  order?: 'asc' | 'desc'
}

function buildFileQuery(parentId: string | null, opts?: FileQueryOptions): string {
  const params = new URLSearchParams()
  if (opts?.recent) {
    // 最近访问模式优先级最高（服务端语义），忽略 parent_id 与标签/收藏过滤。
    params.set('recent', 'true')
  } else if (opts?.tagId || opts?.starred !== undefined) {
    // 检索模式：忽略 parent_id（服务端语义），跨个人+团队可读文件。
    if (opts?.tagId) params.set('tag_id', opts.tagId)
    if (opts?.starred !== undefined) params.set('starred', String(opts.starred))
  } else if (parentId) {
    params.set('parent_id', parentId)
  }
  if (opts?.sort) params.set('sort', opts.sort)
  if (opts?.order) params.set('order', opts.order)
  const query = params.toString()
  return query ? `?${query}` : ''
}

// ---------- 批量操作（部分成功；幂等键重放由服务端处理） ----------

export interface BatchItemResult {
  id: string
  ok: boolean
  error_code?: string
}

/** 生成批量请求幂等键（每次用户操作一个新 key；同 key 重试由调用方保持）。 */
function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) return crypto.randomUUID()
  return `${Date.now()}-${Math.random().toString(36).slice(2)}`
}

/** JSON 请求 + Idempotency-Key 头。 */
function idemInit(body: unknown): RequestInit {
  const init = jsonInit('POST', body)
  const headers = { ...(init.headers as Record<string, string>), 'Idempotency-Key': newIdempotencyKey() }
  return { ...init, headers }
}

/** 批量移动；targetParentId 为空串表示个人根目录。 */
export async function batchMoveFiles(fileIds: string[], targetParentId: string): Promise<BatchItemResult[]> {
  const data = await api<{ results: BatchItemResult[] }>(
    '/api/v1/files/batch/move',
    idemInit({ file_ids: fileIds, target_parent_id: targetParentId }),
  )
  return data.results ?? []
}

/** 批量移入回收站（软删除）。 */
export async function batchTrashFiles(fileIds: string[]): Promise<BatchItemResult[]> {
  const data = await api<{ results: BatchItemResult[] }>('/api/v1/files/batch/trash', idemInit({ file_ids: fileIds }))
  return data.results ?? []
}

/** 批量从回收站恢复。 */
export async function batchRestoreFiles(fileIds: string[]): Promise<BatchItemResult[]> {
  const data = await api<{ results: BatchItemResult[] }>('/api/v1/files/batch/restore', idemInit({ file_ids: fileIds }))
  return data.results ?? []
}

/** 删除单个历史版本（current 版本由服务端拒绝）。 */
export async function deleteFileVersion(fileId: string, versionId: string): Promise<void> {
  await api(`/api/v1/files/${fileId}/versions/${versionId}`, { method: 'DELETE' })
}

/** 批量下载 ZIP 由服务端流式生成。 */
export async function downloadBatchFiles(fileIds: string[]): Promise<void> {
  const res = await authFetch('/api/v1/files/batch/download', idemInit({ file_ids: fileIds }))
  if (!res.ok) throw new ApiError(res.status, '批量下载失败')
  saveBlob(await res.blob(), 'docflow-files.zip')
}

/** 批量结果的简短摘要文案（成功 N 项 / 失败码统计）。 */
export function summarizeBatchResults(results: BatchItemResult[]): string {
  const okCount = results.filter((r) => r.ok).length
  const failures = results.filter((r) => !r.ok)
  if (failures.length === 0) return `全部 ${okCount} 项成功`
  const codes = [...new Set(failures.map((r) => r.error_code ?? 'UNKNOWN'))].join('、')
  return `${okCount} 项成功，${failures.length} 项失败（${codes}）`
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
  /** 完成后的文件 ID（新文件 = 新建文件 id；版本会话 = 目标文件 id；仅 complete 后返回）。 */
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
  /** 可见性（列表响应返回：public|private；旧后端/创建响应可能缺省）。 */
  visibility?: 'public' | 'private'
  /** 关联文件名（列表响应返回；文件已删除或旧后端时为空/缺省）。 */
  file_name?: string | null
  /** 是否受密码保护（明文与哈希均不回传）。 */
  has_password?: boolean
  /** 水印开关（旧后端缺省时按 true 处理）。 */
  watermark_enabled?: boolean
  /** 自定义水印模板；null/缺省表示使用系统默认模板。 */
  watermark_text?: string | null
}

export interface CreatedShare extends ShareItem {
  /** 一次性明文 token（仅公开分享创建响应返回；私有分享为 null）。 */
  token: string | null
  share_url: string | null
}

// ---------- 认证 ----------

/** admin 角色探测缓存（见 isAdmin）；登录/登出后失效。 */
let adminProbe: Promise<boolean> | null = null

/** 两步验证登录标记：login 命中 401 TOTP_REQUIRED 时抛出（code 随 ApiError 携带）。 */
export const TOTP_REQUIRED_CODE = 'TOTP_REQUIRED'

function applyLoginToken(token: string): void {
  accessToken = token
  adminProbe = null
}

export async function login(identifier: string, password: string): Promise<void> {
  const data = await api<{ access_token: string }>('/api/v1/auth/login', jsonInit('POST', { identifier, password }))
  applyLoginToken(data.access_token)
}

/**
 * 两步验证登录第二段：code 为 6 位 TOTP 码；recoveryCode 为一次性恢复码
 *（二选一，均为空时服务端 400）。成功后 access_token 入内存（同 login）。
 * identifier 为登录标识（email 或 username）。
 */
export async function loginTotp(identifier: string, password: string, code: string, recoveryCode = ''): Promise<void> {
  const data = await api<{ access_token: string }>(
    '/api/v1/auth/login/totp',
    jsonInit('POST', { identifier, password, code, recovery_code: recoveryCode }),
  )
  applyLoginToken(data.access_token)
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

// ---------- 会话管理（多端登录） ----------

/** 活跃登录会话（GET /auth/sessions）；不含 refresh token 任何形态，也不标记当前会话。 */
export interface SessionItem {
  id: string
  created_at: string
  last_active_at: string
  expires_at: string
  ip: string | null
  user_agent: string
}

/** 我的活跃会话列表（未撤销未过期，last_active_at 倒序）。 */
export async function listSessions(): Promise<SessionItem[]> {
  const data = await api<{ sessions: SessionItem[] }>('/api/v1/auth/sessions')
  return data.sessions ?? []
}

/** 撤销自己的指定会话（非属主/不存在/已撤销 404）。 */
export async function revokeSession(id: string): Promise<void> {
  await api(`/api/v1/auth/sessions/${id}`, { method: 'DELETE' })
}

/**
 * 撤销全部会话（含当前）：服务端无法凭 Bearer 反推当前 session，一律撤销
 * （含发起请求的会话）并清除 refresh cookie；成功后调用方应清空本地令牌
 * 并跳转登录页。
 */
export async function revokeAllSessions(): Promise<void> {
  await api('/api/v1/auth/sessions', { method: 'DELETE' })
}

// ---------- 个人访问令牌（PAT） ----------

/** PAT 条目（列表与创建响应共用字段；明文仅创建时附带）。 */
export interface ApiTokenItem {
  id: string
  name: string
  scopes: string[]
  /** 明文前 14 字符（dfpat_ + 8 字符），UI 展示用。 */
  prefix: string
  last_used_at: string | null
  expires_at: string | null
  revoked_at: string | null
  created_at: string
}

export interface CreatedApiToken extends ApiTokenItem {
  /** 一次性明文 token（dfpat_ 前缀；仅创建响应返回，之后不可再取）。 */
  token: string
}

/** 我的 PAT 列表（未撤销，含已过期；不含明文）。 */
export async function listTokens(): Promise<ApiTokenItem[]> {
  const data = await api<{ tokens: ApiTokenItem[] }>('/api/v1/tokens')
  return data.tokens ?? []
}

/** 创建 PAT；expiresInDays 为 0 表示永久（1-3650）。 */
export async function createToken(name: string, expiresInDays: number): Promise<CreatedApiToken> {
  return api<CreatedApiToken>('/api/v1/tokens', jsonInit('POST', { name, expires_in_days: expiresInDays }))
}

/** 撤销自己的 PAT（立即失效；非属主/不存在/已撤销 404）。 */
export async function updateToken(id: string, body: { name?: string; scopes?: string[] }): Promise<ApiTokenItem> {
  return api<ApiTokenItem>(`/api/v1/tokens/${id}`, jsonInit('PATCH', body))
}

export async function revokeToken(id: string): Promise<void> {
  await api(`/api/v1/tokens/${id}`, { method: 'DELETE' })
}

// ---------- 两步验证（TOTP） ----------

/** GET /auth/totp 状态视图。 */
export interface TotpStatus {
  enabled: boolean
  /** 启用确认时间（RFC3339）；未 setup/未确认时为 null。 */
  confirmed_at: string | null
}

/** POST /auth/totp/setup 响应：secret 与 otpauth URL（认证器手动添加用）。 */
export interface TotpSetup {
  secret: string
  otpauth_url: string
}

/** 两步验证状态；未 setup 时 enabled=false。 */
export async function getTotpStatus(): Promise<TotpStatus> {
  return api<TotpStatus>('/api/v1/auth/totp')
}

/**
 * 开始设置：生成新 secret（作废未完成的旧 setup）。客户端不渲染二维码，
 * 以文本展示 secret/otpauth URL 供认证器手动录入或导入。
 */
export async function beginTotpSetup(): Promise<TotpSetup> {
  return api<TotpSetup>('/api/v1/auth/totp/setup', jsonInit('POST', {}))
}

/**
 * 确认启用：校验 6 位码后启用并生成 10 个恢复码；明文（xxxx-xxxx）仅此
 * 一次返回（服务端只存哈希），调用方须立即提示用户保存。
 */
export async function confirmTotpSetup(code: string): Promise<string[]> {
  const data = await api<{ recovery_codes: string[] }>('/api/v1/auth/totp/confirm', jsonInit('POST', { code }))
  return data.recovery_codes ?? []
}

/** 禁用两步验证：密码或当前有效 6 位码二选一（均错 403；未启用 404）。 */
export async function disableTotp(password: string, code = ''): Promise<void> {
  await api('/api/v1/auth/totp', jsonInit('DELETE', { password, code }))
}

// ---------- OIDC 单点登录（公开端点） ----------

/** GET /auth/oidc/config：SSO 是否启用（登录页按钮门控）。 */
export interface OIDCStatus {
  enabled: boolean
}

/** SSO 可用性探测：无认证请求；失败（含旧后端 404）保守视为未启用。 */
export async function getOIDCStatus(): Promise<OIDCStatus> {
  try {
    const res = await fetch('/api/v1/auth/oidc/config')
    if (!res.ok) return { enabled: false }
    const data = (await res.json()) as { enabled?: boolean }
    return { enabled: data.enabled === true }
  } catch {
    return { enabled: false }
  }
}

/** SSO 登录入口（302 跳 IdP；回调后落地 /sso#access_token=...）。 */
export const OIDC_LOGIN_PATH = '/api/v1/auth/oidc/login'

// ---------- 站内通知与通知偏好 ----------

/** 通知事件类型（v1.0 范围 + v1.1 配额警告 + G6 版本删除通知）。 */
export type NotificationEventType =
  | 'upload.completed'
  | 'upload.quarantined'
  | 'share.accessed'
  | 'file.updated'
  | 'file.version.deleted'
  | 'quota.warning'

/** 通知条目（GET /notifications）。 */
export interface NotificationItem {
  id: string
  type: NotificationEventType
  title: string
  body: string
  /** 关联资源 ID（通常为文件 ID）；无关联时为 null。 */
  resource_id: string | null
  is_read: boolean
  created_at: string
  read_at: string | null
}

/** 通知列表响应：本页条目 + 下一页游标（空串=无更多）+ 未读总数。 */
export interface NotificationListResult {
  items: NotificationItem[]
  next_cursor: string
  unread_count: number
}

/** 通知列表查询选项。 */
export interface NotificationQueryOptions {
  unreadOnly?: boolean
  limit?: number
  cursor?: string
}

/** 我的站内通知列表（created_at 倒序，created_at 游标分页）。 */
export async function listNotifications(opts?: NotificationQueryOptions): Promise<NotificationListResult> {
  const params = new URLSearchParams()
  if (opts?.unreadOnly) params.set('unread_only', 'true')
  if (opts?.limit) params.set('limit', String(opts.limit))
  if (opts?.cursor) params.set('cursor', opts.cursor)
  const query = params.toString()
  return api<NotificationListResult>(`/api/v1/notifications${query ? `?${query}` : ''}`)
}

/** 标记自己的单条通知已读（幂等；非属主/不存在 404）。 */
export async function markNotificationRead(id: string): Promise<void> {
  await api(`/api/v1/notifications/${id}/read`, { method: 'POST' })
}

/** 我的全部未读通知标记已读。 */
export async function markAllNotificationsRead(): Promise<void> {
  await api('/api/v1/notifications/read-all', { method: 'POST' })
}

/** 通知偏好条目：事件类型的生效开关（无记录 = 默认开启）。 */
export interface NotificationPreference {
  event_type: NotificationEventType
  enabled: boolean
}

/** 我的全部通知偏好（各事件类型）。 */
export async function listNotificationPreferences(): Promise<NotificationPreference[]> {
  const data = await api<{ preferences: NotificationPreference[] }>('/api/v1/notification-preferences')
  return data.preferences ?? []
}

/** 更新单个事件类型开关；返回更新后的条目（未知类型 400）。 */
export async function updateNotificationPreference(
  eventType: NotificationEventType,
  enabled: boolean,
): Promise<NotificationPreference> {
  return api<NotificationPreference>(
    `/api/v1/notification-preferences/${encodeURIComponent(eventType)}`,
    jsonInit('PUT', { enabled }),
  )
}

// ---------- Webhook 通知渠道（v1.1） ----------

/** Webhook 条目（GET /webhooks；secret 仅创建响应返回一次）。 */
export interface WebhookItem {
  id: string
  url: string
  events: NotificationEventType[]
  enabled: boolean
  /** 连续失败计数（成功清零；连续 10 次失败服务端自动停用）。 */
  failure_count: number
  /** 最近一次投递 HTTP 状态码；0=传输层失败，null=从未投递。 */
  last_status: number | null
  last_delivered_at: string | null
  created_at: string
  updated_at: string
}

export interface CreatedWebhook extends WebhookItem {
  /** 一次性签名 secret（whsec_ 前缀；仅创建响应返回，用于校验 X-DocFlow-Signature）。 */
  secret: string
}

/** 我的 webhook 列表（created_at 倒序，含已停用；不含 secret）。 */
export async function listWebhooks(): Promise<WebhookItem[]> {
  const data = await api<{ webhooks: WebhookItem[] }>('/api/v1/webhooks')
  return data.webhooks ?? []
}

/** 创建 webhook；返回一次性 secret（接收方以其复算 HMAC 验签）。 */
export async function createWebhook(url: string, events: NotificationEventType[]): Promise<CreatedWebhook> {
  return api<CreatedWebhook>('/api/v1/webhooks', jsonInit('POST', { url, events }))
}

/** 启用/停用自己的 webhook（自动停用后可经此恢复）；返回更新后的条目。 */
export async function updateWebhook(id: string, enabled: boolean): Promise<WebhookItem> {
  return api<WebhookItem>(`/api/v1/webhooks/${id}`, jsonInit('PATCH', { enabled }))
}

/** 删除自己的 webhook（立即停止投递；非属主/不存在 404）。 */
export async function deleteWebhook(id: string): Promise<void> {
  await api(`/api/v1/webhooks/${id}`, { method: 'DELETE' })
}

// ---------- 邀请注册与密码找回（公开端点） ----------

/** 凭一次性邀请 token 注册；成功即登录（响应同 login，access_token 入内存）。 */
export async function register(token: string, username: string, password: string): Promise<void> {
  const data = await api<{ access_token: string }>('/api/v1/auth/register', jsonInit('POST', { token, username, password }))
  accessToken = data.access_token
  adminProbe = null
}

/** 请求密码重置邮件：无论邮箱是否存在一律 202（不泄露账号存在性）。 */
export async function forgotPassword(email: string): Promise<void> {
  await api('/api/v1/auth/forgot-password', jsonInit('POST', { email }))
}

/** 凭一次性重置 token 重置密码（成功后全部会话失效，需重新登录）。 */
export async function resetPassword(token: string, password: string): Promise<void> {
  await api('/api/v1/auth/reset-password', jsonInit('POST', { token, password }))
}

// ---------- 文件与回收站 ----------

export async function listFiles(parentId: string | null, opts?: FileQueryOptions): Promise<FileItem[]> {
  const data = await api<{ files: FileItem[] }>(`/api/v1/files${buildFileQuery(parentId, opts)}`)
  return data.files ?? []
}

// ---------- 全文检索（文件名 + 文本内容） ----------

/** GET /search 结果条目；name/type/parent_id/updated_at 取 files 行实时值。 */
export interface SearchResultItem {
  id: string
  name: string
  type: 'folder' | 'file'
  parent_id?: string | null
  updated_at: string
  /**
   * 命中上下文：名称命中时为名称本身；内容命中时为 ts_headline 片段，
   * 高亮标记 [[..]]（纯文本，前端渲染高亮）。仅名称索引或无片段时缺省。
   */
  snippet?: string
}

/**
 * 全文检索当前用户可读文件（个人 owner + 团队在册成员；软删排除）。
 * 名称子串（大小写不敏感，中文友好）或内容词命中（内容索引仅文本类
 * 且 ≤2MB；二进制仅名称匹配）。索引在上传完成后异步构建，最新内容
 * 可能有短暂延迟。
 */
export async function searchFiles(q: string, limit?: number): Promise<SearchResultItem[]> {
  const params = new URLSearchParams({ q })
  if (limit) params.set('limit', String(limit))
  const data = await api<{ results: SearchResultItem[] }>(`/api/v1/search?${params.toString()}`)
  return data.results ?? []
}

export async function createFolder(name: string, parentId: string | null): Promise<FileItem> {
  return api<FileItem>('/api/v1/folders', jsonInit('POST', { name, parent_id: parentId ?? '' }))
}

export async function renameFile(id: string, name: string): Promise<FileItem> {
  return api<FileItem>(`/api/v1/files/${id}`, jsonInit('PATCH', { name }))
}

/**
 * 复制文件（仅文件，文件夹 400）：默认副本名「<原名> copy」，name 可覆盖；
 * parentId 为目标目录 UUID（须对目标目录有写权限）。返回新文件元数据。
 */
export async function copyFile(id: string, parentId: string, name?: string): Promise<FileItem> {
  const body: Record<string, unknown> = { parent_id: parentId }
  if (name) body.name = name
  return api<FileItem>(`/api/v1/files/${id}/copy`, jsonInit('POST', body))
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

/** 认证读取文件当前版本内容为文本（draw.io 编辑器加载 XML 用；走下载端点）。 */
export async function fetchFileText(fileId: string): Promise<string> {
  const res = await authFetch(`/api/v1/files/${fileId}/download`)
  if (!res.ok) {
    const data = (await res.json().catch(() => null)) as { error?: string } | null
    throw new ApiError(res.status, data?.error ?? '文件读取失败')
  }
  return res.text()
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
  /** 水印开关；关闭时 watermark_text 为 null。 */
  watermark_enabled: boolean
  /** 按访问者渲染后的水印文案（占位符已替换）。 */
  watermark_text: string | null
}

/** 分享密码保护错误码：公开接口 401 且 code=PASSWORD_REQUIRED 时展示密码表单。 */
export const PASSWORD_REQUIRED_CODE = 'PASSWORD_REQUIRED'

/** 公开分享元数据（无认证；401 PASSWORD_REQUIRED 经 ApiError.code 抛出）。 */
export async function getPublicShare(token: string): Promise<PublicShareInfo> {
  return publicApi<PublicShareInfo>(`/api/v1/public/shares/${encodeURIComponent(token)}`)
}

/** 校验公开分享密码：成功后服务端经 HttpOnly cookie 下发 1 小时访问会话。 */
export async function verifyPublicShare(token: string, password: string): Promise<void> {
  await publicApi<{ ok: boolean }>(
    `/api/v1/public/shares/${encodeURIComponent(token)}/verify`,
    jsonInit('POST', { password }),
  )
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
  /** 公开分享访问密码（4-64 字符；明文仅本次请求，服务端存加盐哈希）。 */
  password?: string
  /** 水印开关；缺省用系统设置 share.default_watermark。 */
  watermarkEnabled?: boolean
  /** 自定义水印模板；缺省用系统设置 share.watermark_text。 */
  watermarkText?: string
}

/**
 * 分享明文 token 的前端内存态：契约约定列表与私有分享响应不回传明文
 * token（仅公开分享创建时返回一次），故创建时暂存于内存 Map，供「我的
 * 分享」页展示复制链接；刷新页面后丢失（等价于“创建时已展示”）。
 * 可见性/文件名已由列表响应后端字段提供（ShareItem.visibility/file_name），
 * 不再依赖内存态。
 */
export interface ShareMeta {
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
  } else if (opts.password) {
    body.password = opts.password
  }
  if (opts.watermarkEnabled !== undefined) body.watermark_enabled = opts.watermarkEnabled
  if (opts.watermarkText) body.watermark_text = opts.watermarkText
  const created = await api<CreatedShare>('/api/v1/shares', jsonInit('POST', body))
  rememberShareMeta(created.id, { token: created.token ?? undefined })
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

// ---------- 分享详情与访问统计 ----------

/** 最近访问记录条目（脱敏：IP 前缀 + UA 摘要，不含哈希）。 */
export interface ShareAccessRecord {
  time: string
  action: 'download' | 'preview'
  ip_prefix: string
  user_agent: string
}

/** 分享访问统计聚合。 */
export interface ShareStats {
  total_access: number
  unique_visitors: number
  recent: ShareAccessRecord[]
}

/** GET /shares/{id} 响应：分享详情 + 访问统计（仅创建者）。 */
export interface ShareDetail extends ShareItem {
  stats: ShareStats
}

/** 分享详情与访问统计（仅创建者）。 */
export async function getShareDetail(id: string): Promise<ShareDetail> {
  return api<ShareDetail>(`/api/v1/shares/${id}`)
}

/** PATCH /shares/{id} 可更新字段；0 表示清除限制（永久 / 不限），空串恢复默认模板。 */
export interface UpdateShareOptions {
  expiresInHours?: number
  maxDownloads?: number
  watermarkEnabled?: boolean
  watermarkText?: string
}

/** 修改分享（有效期/下载上限/水印；permission 等不可改），返回更新后的记录。 */
export async function updateShare(id: string, opts: UpdateShareOptions): Promise<ShareItem> {
  const body: Record<string, unknown> = {}
  if (opts.expiresInHours !== undefined) body.expires_in = Math.round(opts.expiresInHours * 3600)
  if (opts.maxDownloads !== undefined) body.max_downloads = opts.maxDownloads
  if (opts.watermarkEnabled !== undefined) body.watermark_enabled = opts.watermarkEnabled
  if (opts.watermarkText !== undefined) body.watermark_text = opts.watermarkText
  return api<ShareItem>(`/api/v1/shares/${id}`, jsonInit('PATCH', body))
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
export async function updateTeam(id: string, name: string, description: string): Promise<Team> {
  return api<Team>(`/api/v1/teams/${id}`, jsonInit('PATCH', { name, description }))
}
export async function deleteTeam(id: string): Promise<void> { await api(`/api/v1/teams/${id}`, { method: 'DELETE' }) }

export interface CreatedTeam extends Team {
  root_folder_id: string
}

export type TeamRole = 'owner' | 'editor' | 'viewer' | 'custom'

export interface TeamMember {
  user_id: string
  role: TeamRole
  /** 自定义角色 ID（role=custom 时存在）。 */
  role_id?: string
  /** 自定义角色名（role=custom 时存在）。 */
  role_name?: string
  created_at: string
  team_id?: string
}

/** 角色细粒度权限（设计 6.5.2）；deny 中的动作显式拒绝且优先于 allow。 */
export interface RolePermissions {
  read?: boolean
  write?: boolean
  delete?: boolean
  share?: boolean
  admin?: boolean
  deny?: string[]
}

export interface TeamRoleDef {
  id: string
  team_id: string
  name: string
  permissions: RolePermissions
  member_count: number
  created_at: string
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

/** 添加成员（仅团队 owner）。roleId 非空时绑定自定义角色（忽略 role）。 */
export async function addTeamMember(
  teamId: string,
  userId: string,
  role: 'editor' | 'viewer',
  roleId?: string,
): Promise<TeamMember> {
  const body = roleId
    ? { user_id: userId, role_id: roleId }
    : { user_id: userId, role }
  return api<TeamMember>(`/api/v1/teams/${teamId}/members`, jsonInit('POST', body))
}

/** 修改成员角色（仅团队 owner；owner 成员不可改）。roleId 非空时绑定自定义角色。 */
export async function updateTeamMemberRole(
  teamId: string,
  userId: string,
  role: 'editor' | 'viewer',
  roleId?: string,
): Promise<TeamMember> {
  const body = roleId
    ? { role_id: roleId }
    : { role }
  return api<TeamMember>(`/api/v1/teams/${teamId}/members/${userId}`, jsonInit('PATCH', body))
}

/** 移除成员（仅团队 owner；owner 成员不可移除）。 */
export async function removeTeamMember(teamId: string, userId: string): Promise<void> {
  await api(`/api/v1/teams/${teamId}/members/${userId}`, { method: 'DELETE' })
}

// ---------- 团队自定义角色（设计 6.5.2） ----------

/** 自定义角色列表（仅团队 owner；含 member_count 引用统计）。 */
export async function listTeamRoles(teamId: string): Promise<TeamRoleDef[]> {
  const data = await api<{ roles: TeamRoleDef[] }>(`/api/v1/teams/${teamId}/roles`)
  return data.roles ?? []
}

/** 创建自定义角色（permissions 勾选 + deny 显式拒绝）。 */
export async function createTeamRole(
  teamId: string,
  name: string,
  permissions: RolePermissions,
): Promise<TeamRoleDef> {
  return api<TeamRoleDef>(`/api/v1/teams/${teamId}/roles`, jsonInit('POST', { name, permissions }))
}

/** 更新自定义角色。 */
export async function updateTeamRole(
  teamId: string,
  roleId: string,
  name: string,
  permissions: RolePermissions,
): Promise<void> {
  await api(`/api/v1/teams/${teamId}/roles/${roleId}`, jsonInit('PATCH', { name, permissions }))
}

/** 删除自定义角色；仍有成员引用时 409。 */
export async function deleteTeamRole(teamId: string, roleId: string): Promise<void> {
  await api(`/api/v1/teams/${teamId}/roles/${roleId}`, { method: 'DELETE' })
}

export interface TeamFileListing {
  files: FileItem[]
  /** 当前列出的目录 ID；parent_id 缺省查询时即团队根目录 ID。 */
  parent_id: string
}

/** 团队空间文件列表（parentId 为 null 表示团队根目录；tag/starred/排序同 /files 语义，目录范围内过滤）。 */
export async function listTeamFiles(
  teamId: string,
  parentId: string | null,
  opts?: FileQueryOptions,
): Promise<TeamFileListing> {
  const params = new URLSearchParams()
  if (parentId) params.set('parent_id', parentId)
  if (opts?.tagId) params.set('tag_id', opts.tagId)
  if (opts?.starred !== undefined) params.set('starred', String(opts.starred))
  if (opts?.sort) params.set('sort', opts.sort)
  if (opts?.order) params.set('order', opts.order)
  const query = params.toString()
  return api<TeamFileListing>(`/api/v1/teams/${teamId}/files${query ? `?${query}` : ''}`)
}

/** 在团队根目录（parentId 为 null）或指定团队目录下创建目录（editor 及以上角色）。 */
export async function createTeamFolder(teamId: string, name: string, parentId: string | null): Promise<FileItem> {
  return api<FileItem>(`/api/v1/teams/${teamId}/folders`, jsonInit('POST', { name, parent_id: parentId ?? '' }))
}

// ---------- 上传 ----------

export type UploadPhase = 'creating' | 'completing' | UploadStatus

/** 存储配额超限错误码（POST /uploads 403；前端转为中文提示）。 */
export const QUOTA_EXCEEDED_CODE = 'QUOTA_EXCEEDED'

/** 建上传会话（新建/覆盖共用）：配额超限转为中文提示后抛出。 */
async function startUploadSession(body: Record<string, unknown>): Promise<UploadSession> {
  try {
    return await api<UploadSession>('/api/v1/uploads', jsonInit('POST', body))
  } catch (err) {
    if (err instanceof ApiError && err.code === QUOTA_EXCEEDED_CODE) {
      throw new ApiError(err.status, '存储配额已超出，无法上传；可清理回收站（彻底删除后才释放配额）或联系管理员调整配额', QUOTA_EXCEEDED_CODE)
    }
    throw err
  }
}

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
  // /complete 对隔离终态直接回 409 {"error":"scan rejected"}（不进入轮询），
  // 映射为与轮询路径一致的中文隔离文案；其余错误原样抛出。
  let done: UploadSession
  try {
    done = await api<UploadSession>(`/api/v1/uploads/${session.id}/complete`, { method: 'POST' })
  } catch (err) {
    if (err instanceof ApiError && err.status === 409 && err.message.includes('scan rejected')) {
      throw new ApiError(409, '文件未通过安全扫描，已被隔离')
    }
    throw err
  }
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
  const session = await startUploadSession({ name: file.name, size: file.size, parent_id: parentId ?? '' })
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
  const session = await startUploadSession({ name: file.name, size: file.size, file_id: fileId })
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

/**
 * 认证读取指定版本原始内容为文本（版本对比用）：读权限同版本列表
 * （个人 owner、团队任意在册成员）；blob 非 available 时 403。
 */
export async function fetchVersionText(fileId: string, versionId: string): Promise<string> {
  const res = await authFetch(`/api/v1/files/${fileId}/versions/${versionId}/content`)
  if (!res.ok) {
    const data = (await res.json().catch(() => null)) as { error?: string } | null
    throw new ApiError(res.status, data?.error ?? '版本内容读取失败')
  }
  return res.text()
}

/** 版本对比可用的文本判定：MIME 命中预览白名单（text/json），或常见文本扩展名。 */
const TEXT_DIFF_EXTS = new Set([
  'txt', 'md', 'markdown', 'json', 'csv', 'log', 'xml', 'yml', 'yaml', 'ini', 'conf', 'toml', 'env',
  'sql', 'js', 'jsx', 'ts', 'tsx', 'go', 'py', 'rb', 'java', 'c', 'h', 'cpp', 'hpp', 'cs', 'php',
  'sh', 'bat', 'ps1', 'css', 'scss', 'html', 'htm', 'svg', 'drawio',
])

export function isTextLike(name: string, mime: string): boolean {
  if (previewKind(mime) === 'text') return true
  const i = name.lastIndexOf('.')
  return i >= 0 && TEXT_DIFF_EXTS.has(name.slice(i + 1).toLowerCase())
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

// ---------- draw.io 图表编辑器 ----------

/** GET /drawio/config：draw.io 集成可用性与编辑器基地址。 */
export interface DrawioStatus {
  enabled: boolean
  /** 浏览器可达的 draw.io 编辑器基地址（iframe embed 加载用）；禁用时为 null。 */
  url: string | null
}

/**
 * 仅 .drawio 扩展名进入图表编辑。取舍：.xml 也可能是合法的 drawio 图表，
 * 但无法与普通 XML 文件区分（避免误判不做「打开方式」选择），v1.0 不识别；
 * 用户可手动把扩展名改为 .drawio 后编辑。
 */
export function isDrawioFile(name: string): boolean {
  return name.toLowerCase().endsWith('.drawio')
}

/** 空图表初始模板（新建 .drawio 文件与空内容容错共用）。 */
export const EMPTY_DRAWIO_XML = '<mxfile><diagram/></mxfile>'

// 集成可用性探测缓存（见 drawioStatus）：文件列表行「图表」按钮与 DrawioPage
// 共用，按会话缓存一次；失败（含旧后端 404）保守视为未启用。
let drawioProbe: Promise<DrawioStatus> | null = null

/** draw.io 集成可用性探测：结果按会话缓存；请求失败时视为 {enabled:false}。 */
export function drawioStatus(): Promise<DrawioStatus> {
  if (!drawioProbe) {
    drawioProbe = api<DrawioStatus>('/api/v1/drawio/config').catch(() => ({
      enabled: false,
      url: null,
    }))
  }
  return drawioProbe
}

// ---------- Excalidraw 白板编辑器（前端内置，无需后端集成配置） ----------

/** 仅 .excalidraw 扩展名进入白板编辑；编辑器由前端懒加载，按钮恒可用。 */
export function isExcalidrawFile(name: string): boolean {
  return name.toLowerCase().endsWith('.excalidraw')
}

/** 空白板初始场景（新建 .excalidraw 文件与空/损坏内容容错共用）。 */
export const EMPTY_EXCALIDRAW_JSON = '{"type":"excalidraw","version":2,"elements":[],"appState":{}}'

// ---------- AI 摘要（文本类文件） ----------

/** POST /files/{id}/ai/summary 响应：生成的摘要文本。 */
export interface AiSummaryResult {
  summary: string
}

/**
 * 生成文件 AI 摘要（仅文本类文件）：后端未配置 AI 时 503（AI_UNAVAILABLE），
 * 非文本类 400；均以 ApiError 抛出，由调用方映射为提示文案。
 */
export async function aiSummarize(fileId: string): Promise<AiSummaryResult> {
  return api<AiSummaryResult>(`/api/v1/files/${fileId}/ai/summary`, jsonInit('POST', {}))
}

// ---------- 路径级 ACL（团队空间文件夹） ----------

/** ACL 主体类型：用户 / 团队 / 团队自定义角色。 */
export type ACLSubjectType = 'user' | 'team' | 'role'

/** 路径级 ACL 权限动作（read/write/delete/share，不含 admin）。 */
export type ACLAction = 'read' | 'write' | 'delete' | 'share'

/**
 * 路径级 ACL 条目：主体（类型 + UUID）+ 效果（allow 授予 / deny 显式拒绝，
 * deny 优先）+ 权限动作集合。保存为整体覆盖（PUT 全量条目）。
 */
export interface FolderACLEntry {
  subject_type: ACLSubjectType
  subject_id: string
  effect: 'allow' | 'deny'
  permissions: ACLAction[]
}

/** 查询文件夹路径级 ACL（仅团队 owner；无条目时为空数组）。 */
export async function getFolderACL(folderId: string): Promise<FolderACLEntry[]> {
  const data = await api<{ entries?: FolderACLEntry[] } | FolderACLEntry[]>(
    `/api/v1/folders/${folderId}/acl`,
  )
  return Array.isArray(data) ? data : (data.entries ?? [])
}

/** 整体覆盖保存文件夹路径级 ACL（仅团队 owner；空数组即清空全部条目）。 */
export async function putFolderACL(folderId: string, entries: FolderACLEntry[]): Promise<void> {
  await api(`/api/v1/folders/${folderId}/acl`, jsonInit('PUT', { entries }))
}

// ---------- 管理端（仅 admin 角色） ----------

export type SettingType = 'bool' | 'int' | 'string'

export type SettingValue = boolean | number | string

/** 设置变更的生效方式（G6）：立即 / 新会话 / 需重启。 */
export type SettingEffect = 'immediate' | 'new_session' | 'restart'

/** GET /admin/settings 条目；value 为按 type 解析后的当前生效值（未设置时为默认值）。 */
export interface SettingItem {
  key: string
  value: SettingValue
  type: SettingType
  description: string
  default: SettingValue
  /** 变更生效方式徽章数据源（立即/新会话/需重启）。 */
  effect: SettingEffect
  updated_at?: string
  updated_by?: string | null
}

/** GET /admin/settings 响应：设置列表 + 密钥类配置的只读状态（只报 configured，不回显值）。 */
export interface AdminSettingsResult {
  settings: SettingItem[]
  secrets: Record<string, boolean>
}

export interface AdminStats {
  users: number
  files: number
  /** upload_sessions 表行数。 */
  uploads: number
  sessions: number
  shares: number
  /** api_tokens 表行数（v1.1 起返回）。 */
  tokens: number
}

export async function adminGetSettings(): Promise<AdminSettingsResult> {
  return api<AdminSettingsResult>('/api/v1/admin/settings')
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

// ---------- HTTPS 运行时切换（仅 admin；经 Caddy admin API 热下发） ----------

/** TLS 模式：http 明文 / auto 域名+ACME 自动签发 / internal 域名或 IP+自签。 */
export type TlsMode = 'http' | 'auto' | 'internal'

/** GET /admin/tls 响应；managed=false 表示未配置 CADDY_ADMIN_ADDR（不可切换）。 */
export interface TlsStatus {
  mode: TlsMode
  domain: string
  managed: boolean
}

export async function adminGetTls(): Promise<TlsStatus> {
  return api<TlsStatus>('/api/v1/admin/tls')
}

/** 切换 HTTPS 模式（caddy 拒绝或不可达时 400，旧配置保持）；返回生效状态。 */
export async function adminPutTls(mode: TlsMode, domain: string): Promise<TlsStatus> {
  return api<TlsStatus>('/api/v1/admin/tls', jsonInit('PUT', { mode, domain }))
}

// ---------- 隔离区管理（仅 admin；G6） ----------

/** 隔离 blob 条目（GET /admin/quarantine）；file_* 经 file_versions join，孤儿 blob 为 null。 */
export interface QuarantineItem {
  sha256: string
  size: number
  mime_type: string
  ref_count: number
  file_id?: string | null
  file_name?: string | null
  created_at: string
}

export async function adminListQuarantine(limit = 100): Promise<QuarantineItem[]> {
  const data = await api<{ items: QuarantineItem[] }>(`/api/v1/admin/quarantine?limit=${limit}`)
  return data.items ?? []
}

export type QuarantineAction = 'rescan' | 'release' | 'delete'

/**
 * 处置隔离 blob（POST /admin/quarantine/:sha256/action）：
 * rescan 返回处置后状态；release 须 confirm=true（缺省 400）；delete 成功 204
 * （返回 null）。全部动作服务端写审计。
 */
export async function adminQuarantineAction(
  sha256: string,
  action: QuarantineAction,
  opts: { confirm?: boolean } = {},
): Promise<{ status: string } | null> {
  if (action === 'delete') {
    await api(`/api/v1/admin/quarantine/${encodeURIComponent(sha256)}/action`, jsonInit('POST', { action }))
    return null
  }
  return api<{ sha256: string; status: string }>(
    `/api/v1/admin/quarantine/${encodeURIComponent(sha256)}/action`,
    jsonInit('POST', { action, confirm: opts.confirm ?? false }),
  )
}

export interface AuditEntry {
  id: number
  user_id: string | null
  action: string
  resource_type: string
  resource_id: string
  status: string
  created_at: string
}
export interface AuditListResult { items: AuditEntry[]; next_cursor: string; total: number }
export async function adminListAuditLogs(action = '', userId = '', cursor = ''): Promise<AuditListResult> {
  const params = new URLSearchParams()
  if (action) params.set('action', action)
  if (userId) params.set('user_id', userId)
  if (cursor) params.set('cursor', cursor)
  const query = params.toString()
  return api<AuditListResult>(`/api/v1/admin/audit-logs${query ? `?${query}` : ''}`)
}
export async function adminDownloadAuditCSV(action = ''): Promise<void> {
  const query = action ? `?action=${encodeURIComponent(action)}` : ''
  const res = await authFetch(`/api/v1/admin/audit-logs/export.csv${query}`)
  if (!res.ok) throw new ApiError(res.status, '审计日志导出失败')
  saveBlob(await res.blob(), 'audit-logs.csv')
}
// ---------- 管理端备份（仅 admin；执行由 scripts/backup 在服务进程外完成） ----------

/** 备份清单内的单文件条目（GET /admin/backups/status 返回）。 */
export interface BackupFileEntry {
  path: string
  type: string
  sha256: string
  size: number
}

/** 最近备份摘要（组件 / 对象存储模式 / 总大小 / 时间）。 */
export interface BackupLastInfo {
  name: string
  manifest: string
  timestamp?: string
  modified_at: string
  size: number
  components?: string[]
  object_store?: string
  encryption?: string
}

/** GET /admin/backups/status：enabled=BACKUP_DIR 已配置；verified 为 null 表示从未校验。 */
export interface BackupStatus {
  enabled: boolean
  last_backup: BackupLastInfo | null
  verified: boolean | null
  verified_at?: string
  files: BackupFileEntry[]
}

/** POST /admin/backups/verify：对最近备份的只读 sha256 复核结果。 */
export interface BackupVerifyResult {
  manifest: string
  backup: string
  timestamp?: string
  files: number
  verified: boolean
  errors?: string[]
}

export async function adminGetBackupStatus(): Promise<BackupStatus> {
  return api<BackupStatus>('/api/v1/admin/backups/status')
}

/**
 * 校验最近备份（只读 sha256 复核，服务端不执行脚本）。校验未通过时服务端
 * 返回 422 且响应体为复核明细——此处同样解析为结果对象（verified=false，
 * errors 为失败原因），仅网络/权限等错误抛 ApiError。
 */
export async function adminVerifyBackup(): Promise<BackupVerifyResult> {
  const res = await authFetch('/api/v1/admin/backups/verify', { method: 'POST' })
  const data = (await parseBody(res)) as (BackupVerifyResult & { error?: string }) | null
  if (res.status === 200 || res.status === 422) {
    return data as BackupVerifyResult
  }
  throw new ApiError(res.status, data?.error ?? '备份校验失败')
}

export async function adminRunBackup(): Promise<void> {
  await api('/api/v1/admin/backups/run', { method: 'POST' })
}

// ---------- 仪表盘（v1.1） ----------

/** 仪表盘最近文件条目（个人空间 updated_at 倒序前 5）。 */
export interface DashboardRecentFile {
  id: string
  name: string
  updated_at: string
}

/** GET /dashboard 响应：个人统计 + admin 全局统计（仅 admin 角色）。 */
export interface DashboardData {
  /** 我的文件数（个人空间未软删文件，不含目录）。 */
  files: number
  /** 存储占用（当前版本大小之和，字节）。 */
  storage_bytes: number
  /** 我可访问团队空间的文件数。 */
  team_files: number
  /** 有效分享数（公开+私有：未撤销未过期）。 */
  shares: number
  /** 近 7 天上传会话数。 */
  uploads_7d: number
  recent_files: DashboardRecentFile[]
  /** 全局统计（仅 admin 返回）。 */
  admin?: AdminStats
}

/** 个人仪表盘概览统计（admin 附全局统计）。 */
export async function getDashboard(): Promise<DashboardData> {
  return api<DashboardData>('/api/v1/dashboard')
}

// ---------- 邀请管理（仅 admin 角色） ----------

/** 邀请派生状态（后端按 accepted_at/expires_at 计算）。 */
export type InvitationStatus = 'pending' | 'accepted' | 'expired'

export interface Invitation {
  id: string
  email: string
  invited_by: string | null
  role: 'user' | 'admin'
  status: InvitationStatus
  expires_at: string
  accepted_at: string | null
  created_at: string
}

export interface CreatedInvitation extends Invitation {
  /** 一次性注册链接（/register/<token>；仅新建响应返回一次，幂等命中既有邀请时缺省）。 */
  accept_url?: string | null
}

/** 创建邀请；返回一次性 accept_url（明文 token 仅此一次可见）。 */
export async function adminCreateInvitation(email: string, role: 'user' | 'admin'): Promise<CreatedInvitation> {
  return api<CreatedInvitation>('/api/v1/admin/invitations', jsonInit('POST', { email, role }))
}

/** 邀请列表（created_at 倒序，含派生状态）。 */
export async function adminListInvitations(): Promise<Invitation[]> {
  const data = await api<{ invitations: Invitation[] }>('/api/v1/admin/invitations')
  return data.invitations ?? []
}

/** 撤销邀请（删行，token 立即失效；不存在 404）。 */
export async function adminRevokeInvitation(id: string): Promise<void> {
  await api(`/api/v1/admin/invitations/${id}`, { method: 'DELETE' })
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

// ---------- 个人档案与配额（/me） ----------

/** 界面语言选项（与后端白名单一致）。 */
export const PROFILE_LANGUAGES: Array<{ value: 'zh-CN' | 'en-US'; label: string }> = [
  { value: 'zh-CN', label: '简体中文' },
  { value: 'en-US', label: 'English' },
]

/** 用户档案字段（可空文本为 null；头像按 username/昵称首字母计算）。 */
export interface UserProfile {
  nickname: string | null
  department: string | null
  position: string | null
  phone: string | null
  bio: string | null
  language: string
  timezone: string
}

/** GET /me 响应：档案 + 存储用量/配额。 */
export interface MeData {
  id: string
  username: string
  email: string
  role: 'user' | 'admin'
  status: 'active' | 'disabled' | 'locked'
  /** 存储用量（软删/回收站文件计入；字节）。 */
  storage: { used: number; quota: number }
  profile: UserProfile
  created_at: string
}

/** 当前用户档案与存储用量。 */
export async function getMe(): Promise<MeData> {
  return api<MeData>('/api/v1/me')
}

/** PATCH /me 可更新字段（undefined 表示不更新；空串清空文本字段）。 */
export interface UpdateMeOptions {
  nickname?: string
  department?: string
  position?: string
  phone?: string
  bio?: string
  language?: string
  timezone?: string
}

/** 更新个人档案（language ∈ zh-CN|en-US）；返回更新后的完整 /me 视图。 */
export async function updateMe(opts: UpdateMeOptions): Promise<MeData> {
  const body: Record<string, unknown> = {}
  for (const key of ['nickname', 'department', 'position', 'phone', 'bio', 'language', 'timezone'] as const) {
    if (opts[key] !== undefined) body[key] = opts[key]
  }
  return api<MeData>('/api/v1/me', jsonInit('PATCH', body))
}

// ---------- 管理端用户管理（仅 admin） ----------

/** 管理端用户条目（脱敏：不含密码哈希）。 */
export interface AdminUser {
  id: string
  username: string
  email: string
  role: 'user' | 'admin'
  status: 'active' | 'disabled' | 'locked'
  storage_quota: number
  failed_login_count: number
  locked_until: string | null
  profile: UserProfile
  created_at: string
  updated_at: string
}

/** GET /admin/users 分页响应。 */
export interface AdminUserListResult {
  users: AdminUser[]
  total: number
  limit: number
  offset: number
}

/** 用户列表（q 为 username/email 前缀检索；分页）。 */
export async function adminListUsers(q = '', limit = 50, offset = 0): Promise<AdminUserListResult> {
  const params = new URLSearchParams()
  if (q) params.set('q', q)
  if (limit) params.set('limit', String(limit))
  if (offset) params.set('offset', String(offset))
  const query = params.toString()
  return api<AdminUserListResult>(`/api/v1/admin/users${query ? `?${query}` : ''}`)
}

/** PATCH /admin/users/{id} 可更新字段。 */
export interface AdminUpdateUserOptions {
  /** active|disabled；禁用立即撤销其全部会话；不可禁用自己。 */
  status?: 'active' | 'disabled'
  /** 存储配额（字节）。 */
  storageQuota?: number
  role?: 'user' | 'admin'
}

/** 更新用户（禁用/启用/改配额/改角色）；返回更新后的条目。 */
export async function adminGetUser(id: string): Promise<AdminUser> {
  return api<AdminUser>(`/api/v1/admin/users/${id}`)
}

export async function adminDeleteUser(id: string): Promise<void> {
  await api(`/api/v1/admin/users/${id}`, { method: 'DELETE' })
}

export async function adminUpdateUser(id: string, opts: AdminUpdateUserOptions): Promise<AdminUser> {
  const body: Record<string, unknown> = {}
  if (opts.status !== undefined) body.status = opts.status
  if (opts.storageQuota !== undefined) body.storage_quota = opts.storageQuota
  if (opts.role !== undefined) body.role = opts.role
  return api<AdminUser>(`/api/v1/admin/users/${id}`, jsonInit('PATCH', body))
}

/** 重置用户密码（强度同自助改密；成功后其全部会话失效）。 */
export async function adminResetUserPassword(id: string, newPassword: string): Promise<void> {
  await api(`/api/v1/admin/users/${id}/reset-password`, jsonInit('POST', { new_password: newPassword }))
}
