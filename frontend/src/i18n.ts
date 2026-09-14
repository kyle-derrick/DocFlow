import { createContext, useContext } from 'react'

export type Locale = 'zh-CN' | 'en-US'
export const LocaleContext = createContext<Locale>('zh-CN')
export const localeStorageKey = 'docflow.locale'
export function loadLocale(): Locale {
  return localStorage.getItem(localeStorageKey) === 'en-US' ? 'en-US' : 'zh-CN'
}
export function saveLocale(locale: Locale): void { localStorage.setItem(localeStorageKey, locale) }
export function useLocale(): Locale { return useContext(LocaleContext) }

// 中文为基准字典（默认语言）；英文经类型约束强制逐 key 对齐，缺漏即编译失败。
const zh = {
  // ---- 通用（既有） ----
  login: '登录', logout: '退出登录', files: '文件', settings: '设置', upload: '上传文件', empty: '此目录为空',
  language: '语言', switchLanguage: 'English', loading: '加载中…', delete: '删除', undo: '撤销',
  deleteConfirm: '确定删除此项？可在回收站中恢复。', deleteFailed: '删除失败', restoreFailed: '恢复失败',
  noMatch: '没有匹配的文件', close: '关闭', download: '下载', preview: '预览', previewFailed: '预览加载失败',
  // ---- 通用补充 ----
  cancel: '取消', save: '保存', create: '创建', edit: '编辑', refresh: '刷新', actions: '操作',
  name: '名称', time: '时间', all: '全部', status: '状态', loadFailed: '加载失败', unknownError: '未知错误', saveFailed: '保存失败',
  noPermission: '无写权限', uuidInvalid: 'UUID 格式不正确', operationFailed: '操作失败',
  selectedCount: '已选 {n} 项', clearSelection: '取消选择', selectAll: '全选', selectItem: '选择',
  clipboardCopyFailed: '复制失败，请手动复制', copied: '已复制 ✓',
  // ---- 顶部导航 ----
  overview: '概览', teams: '团队', shared: '分享', trash: '回收站', admin: '管理',
  // ---- 文件浏览：视图切换与筛选 ----
  viewAll: '全部', viewStarred: '收藏', viewRecent: '最近', tag: '标签', sort: '排序', direction: '方向',
  sortOrderName: '名称', sortOrderUpdated: '修改时间', sortOrderSize: '大小', orderAsc: '升序', orderDesc: '降序',
  starredOnly: '仅收藏', starredNo: '未收藏', clearFilters: '清除筛选',
  recentHint: '最近访问：按访问时间倒序，跨越全部目录。',
  searchModeHint: '检索模式：结果跨越全部个人与团队目录（标签/收藏过滤）。',
  // ---- 批量操作 ----
  batchMove: '移动到…', batchTrashConfirm: '确定将所选 {n} 项移入回收站？',
  batchDownload: '下载 (zip)', downloadFailed: '下载失败',
  batchShare: '分享', batchShareTitle: '分享所选文件', batchShareDone: '已创建 {n} 条公开分享链接',
  batchSharePartial: '{ok} 条成功，{fail} 条失败', batchShareEmpty: '所选内容不含可分享的文件',
  batchShareLinks: '分享链接（令牌仅显示一次，请立即复制保存）',
  batchTag: '打标签', batchTagTitle: '给所选 {n} 项打标签', batchTagPlaceholder: '选择标签',
  noTagsHint: '你还没有标签，可在文件行的「标签」中创建。', batchTagOk: '已为 {n} 项打标签',
  batchTagPartial: '{ok} 项成功，{fail} 项失败', apply: '应用',
  batchStar: '收藏', batchUnstar: '取消收藏', batchStarOk: '已收藏 {n} 项', batchUnstarOk: '已取消收藏 {n} 项',
  batchStarFailed: '{fail} 项收藏操作失败',
  // ---- 行内复制 ----
  copy: '复制', copyTitle: '复制「{name}」', copyTargetLabel: '目标目录 UUID（留空复制到当前目录）',
  copyOk: '复制成功', fileCopyFailed: '复制失败',
  // ---- 仪表盘 ----
  personalStats: '个人统计', globalStats: '全局统计（管理员）',
  statMyFiles: '我的文件', statStorage: '存储占用', statTeamFiles: '团队空间文件',
  statShares: '有效分享', statUploads7d: '近 7 天上传', statUsers: '用户', statFiles: '文件',
  statUploads: '上传会话', statSessions: '登录会话', statTokens: 'API 令牌',
  recentFiles: '最近文件', dashEmpty: '还没有文件，去文件页上传或新建',
  hotkeys: '快捷键', goFiles: '前往文件页',
  // ---- 回收站 ----
  trashEmpty: '回收站为空', trashLoadFailed: '加载回收站失败',
  restore: '恢复', purge: '彻底删除', restoreSelected: '恢复所选', batchRestoreFailed: '批量恢复失败',
  batchRestorePrefix: '批量恢复：{summary}', purgeConfirm: '彻底删除「{name}」？此操作不可恢复。',
  restoreOk: '已恢复「{name}」', purgeOk: '已彻底删除「{name}」',
  restoreFailedItem: '恢复「{name}」失败', purgeFailedItem: '彻底删除「{name}」失败', deletedAt: '删除时间',
  // ---- 我的分享 ----
  sharedTitle: '我的分享', sharedEmpty: '暂无分享记录', sharedLoadFailed: '加载分享失败',
  fileName: '文件名', permission: '权限', visibility: '可见性', link: '链接', downloadCount: '下载次数',
  expiresAt: '过期时间', publicBadge: '公开', privateBadge: '私有', canDownload: '可下载', viewOnly: '仅查看',
  copyLink: '复制链接', stats: '统计', hideStats: '收起统计', revoke: '撤销',
  revokedBadge: '已撤销', expiredBadge: '已过期', revokeFailed: '撤销失败',
  revokeConfirm: '确定撤销该分享？撤销后立即失效，不可恢复。', shareRevoked: '分享已撤销',
  statsLoading: '统计加载中…', statsFailed: '统计加载失败，请重试', noAccessRecords: '暂无访问记录',
  actionCol: '动作', ipPrefix: 'IP 前缀', totalAccess: '总访问', uniqueVisitors: '独立访客',
  linkShownOnCreate: '链接创建时已展示', grantedAccess: '授权用户/团队访问',
  // ---- 设置 ----
  profileTitle: '个人资料', profileLoadFailed: '个人资料加载失败', profileSaved: '个人资料已保存',
  appearance: '外观', notifPrefsTitle: '通知偏好', notifPrefsLoadFailed: '通知偏好加载失败',
  notifPrefsUpdateFailed: '通知偏好更新失败', totpTitle: '两步验证', totpLoadFailed: '两步验证状态加载失败',
  webhookTitle: 'Webhook', webhookLoadFailed: 'Webhook 列表加载失败', webhookCreateFailed: '创建 Webhook 失败',
  sessionsTitle: '登录会话', sessionsLoadFailed: '会话列表加载失败',
  patTitle: '个人访问令牌', patLoadFailed: '令牌列表加载失败', patCreateFailed: '创建令牌失败',
  noTokens: '暂无令牌', noSessions: '暂无活跃会话', noWebhooks: '暂无 Webhook', createTokenBtn: '创建令牌',
  // ---- 管理端 ----
  adminTitle: '管理设置', adminForbiddenTitle: '无访问权限',
  adminForbiddenBody: '该页面仅系统管理员（admin 角色）可访问。',
  usersTitle: '用户管理', usersLoadFailed: '用户列表加载失败', noUsers: '没有匹配的用户',
  invitesTitle: '邀请管理', invitesLoadFailed: '邀请列表加载失败', inviteCreateFailed: '创建邀请失败',
  noInvites: '暂无邀请记录', auditTitle: '审计日志', auditLoadFailed: '审计日志加载失败',
  backupTitle: '备份', backupLoadFailed: '备份状态加载失败',
  // ---- 团队 ----
  teamsTitle: '团队', createTeamTitle: '创建团队', teamNameLabel: '团队名称',
  teamDescLabel: '描述（可选）', creating: '创建中…',
  teamsEmpty: '还没有团队，创建一个开始协作吧', teamLoadFailed: '加载团队失败',
  teamCreateFailed: '创建团队失败', teamUpdateFailed: '更新团队失败', teamDeleteFailed: '删除团队失败',
  deleteTeamConfirm: '删除团队“{name}”？', noDesc: '暂无描述', createdAt: '创建于',
  backToTeams: '← 返回团队列表',
  // ---- 团队空间 ----
  membersTitle: '成员（{n}）', addMember: '添加成员', userUUID: '用户 UUID', roleLabel: '角色',
  ownerOnlyHint: '仅团队 owner 可管理成员。', membersLoadFailed: '加载成员失败',
  addMemberFailed: '添加成员失败', removeMemberFailed: '移除成员失败', changeRoleFailed: '修改角色失败',
  customRolesTitle: '自定义角色（{n}）', noCustomRoles: '暂无自定义角色。',
  teamNotFound: '未找到该团队：可能已被删除或你不是团队成员',
  teamSpaceEmpty: '团队空间为空，上传文件或新建文件夹开始协作',
  permRead: '读取', permWrite: '写入', permDelete: '删除', permShare: '分享', permAdmin: '管理',
}

const en: { [K in keyof typeof zh]: string } = {
  // ---- Common (existing) ----
  login: 'Log in', logout: 'Log out', files: 'Files', settings: 'Settings', upload: 'Upload file', empty: 'This folder is empty',
  language: 'Language', switchLanguage: '中文', loading: 'Loading…', delete: 'Delete', undo: 'Undo',
  deleteConfirm: 'Delete this item? You can restore it from the trash.', deleteFailed: 'Delete failed', restoreFailed: 'Restore failed',
  noMatch: 'No matching files', close: 'Close', download: 'Download', preview: 'Preview', previewFailed: 'Failed to load preview',
  // ---- Common additions ----
  cancel: 'Cancel', save: 'Save', create: 'Create', edit: 'Edit', refresh: 'Refresh', actions: 'Actions',
  name: 'Name', time: 'Time', all: 'All', status: 'Status', loadFailed: 'Failed to load', unknownError: 'Unknown error', saveFailed: 'Save failed',
  noPermission: 'No write permission', uuidInvalid: 'Invalid UUID format', operationFailed: 'Operation failed',
  selectedCount: '{n} selected', clearSelection: 'Clear selection', selectAll: 'Select all', selectItem: 'Select',
  clipboardCopyFailed: 'Copy failed; please copy manually', copied: 'Copied ✓',
  // ---- Top navigation ----
  overview: 'Overview', teams: 'Teams', shared: 'Shared', trash: 'Trash', admin: 'Admin',
  // ---- File browser: view tabs & filters ----
  viewAll: 'All', viewStarred: 'Starred', viewRecent: 'Recent', tag: 'Tag', sort: 'Sort', direction: 'Order',
  sortOrderName: 'Name', sortOrderUpdated: 'Modified', sortOrderSize: 'Size', orderAsc: 'Ascending', orderDesc: 'Descending',
  starredOnly: 'Starred only', starredNo: 'Not starred', clearFilters: 'Clear filters',
  recentHint: 'Recently accessed files, sorted by access time across all folders.',
  searchModeHint: 'Search mode: results span all personal and team folders (tag/starred filters).',
  // ---- Batch operations ----
  batchMove: 'Move to…', batchTrashConfirm: 'Move {n} selected items to trash?',
  batchDownload: 'Download (zip)', downloadFailed: 'Download failed',
  batchShare: 'Share', batchShareTitle: 'Share selected files', batchShareDone: 'Created {n} public share links',
  batchSharePartial: '{ok} succeeded, {fail} failed', batchShareEmpty: 'No shareable files in the selection',
  batchShareLinks: 'Share links (tokens shown once; copy them now)',
  batchTag: 'Tag', batchTagTitle: 'Tag {n} selected items', batchTagPlaceholder: 'Select a tag',
  noTagsHint: 'You have no tags yet; create one from a file row “Tag” dialog.', batchTagOk: 'Tagged {n} items',
  batchTagPartial: '{ok} succeeded, {fail} failed', apply: 'Apply',
  batchStar: 'Star', batchUnstar: 'Unstar', batchStarOk: 'Starred {n} items', batchUnstarOk: 'Unstarred {n} items',
  batchStarFailed: 'Failed to star {fail} items',
  // ---- Row copy ----
  copy: 'Copy', copyTitle: 'Copy “{name}”', copyTargetLabel: 'Target folder UUID (leave empty for current folder)',
  copyOk: 'Copied', fileCopyFailed: 'Copy failed',
  // ---- Dashboard ----
  personalStats: 'Personal stats', globalStats: 'Global stats (admin)',
  statMyFiles: 'My files', statStorage: 'Storage used', statTeamFiles: 'Team files',
  statShares: 'Active shares', statUploads7d: 'Uploads (7 days)', statUsers: 'Users', statFiles: 'Files',
  statUploads: 'Upload sessions', statSessions: 'Login sessions', statTokens: 'API tokens',
  recentFiles: 'Recent files', dashEmpty: 'No files yet — upload or create on the Files page',
  hotkeys: 'Keyboard shortcuts', goFiles: 'Go to the Files page',
  // ---- Trash ----
  trashEmpty: 'Trash is empty', trashLoadFailed: 'Failed to load trash',
  restore: 'Restore', purge: 'Delete permanently', restoreSelected: 'Restore selected', batchRestoreFailed: 'Batch restore failed',
  batchRestorePrefix: 'Batch restore: {summary}', purgeConfirm: 'Permanently delete “{name}”? This cannot be undone.',
  restoreOk: 'Restored “{name}”', purgeOk: 'Permanently deleted “{name}”',
  restoreFailedItem: 'Failed to restore “{name}”', purgeFailedItem: 'Failed to delete “{name}”', deletedAt: 'Deleted at',
  // ---- My shares ----
  sharedTitle: 'My shares', sharedEmpty: 'No shares yet', sharedLoadFailed: 'Failed to load shares',
  fileName: 'File name', permission: 'Permission', visibility: 'Visibility', link: 'Link', downloadCount: 'Downloads',
  expiresAt: 'Expires at', publicBadge: 'Public', privateBadge: 'Private', canDownload: 'Downloadable', viewOnly: 'View only',
  copyLink: 'Copy link', stats: 'Stats', hideStats: 'Hide stats', revoke: 'Revoke',
  revokedBadge: 'Revoked', expiredBadge: 'Expired', revokeFailed: 'Revoke failed',
  revokeConfirm: 'Revoke this share? It becomes invalid immediately and cannot be recovered.', shareRevoked: 'Share revoked',
  statsLoading: 'Loading stats…', statsFailed: 'Failed to load stats, please retry', noAccessRecords: 'No access records yet',
  actionCol: 'Action', ipPrefix: 'IP prefix', totalAccess: 'Total access', uniqueVisitors: 'Unique visitors',
  linkShownOnCreate: 'Link was shown on creation', grantedAccess: 'Granted users/teams',
  // ---- Settings ----
  profileTitle: 'Profile', profileLoadFailed: 'Failed to load profile', profileSaved: 'Profile saved',
  appearance: 'Appearance', notifPrefsTitle: 'Notification preferences', notifPrefsLoadFailed: 'Failed to load notification preferences',
  notifPrefsUpdateFailed: 'Failed to update notification preference', totpTitle: 'Two-factor authentication', totpLoadFailed: 'Failed to load 2FA status',
  webhookTitle: 'Webhook', webhookLoadFailed: 'Failed to load webhooks', webhookCreateFailed: 'Failed to create webhook',
  sessionsTitle: 'Login sessions', sessionsLoadFailed: 'Failed to load sessions',
  patTitle: 'Personal access tokens', patLoadFailed: 'Failed to load tokens', patCreateFailed: 'Failed to create token',
  noTokens: 'No tokens yet', noSessions: 'No active sessions', noWebhooks: 'No webhooks yet', createTokenBtn: 'Create token',
  // ---- Admin ----
  adminTitle: 'Admin settings', adminForbiddenTitle: 'Access denied',
  adminForbiddenBody: 'This page is available to system administrators (admin role) only.',
  usersTitle: 'Users', usersLoadFailed: 'Failed to load users', noUsers: 'No matching users',
  invitesTitle: 'Invitations', invitesLoadFailed: 'Failed to load invitations', inviteCreateFailed: 'Failed to create invitation',
  noInvites: 'No invitations yet', auditTitle: 'Audit logs', auditLoadFailed: 'Failed to load audit logs',
  backupTitle: 'Backup', backupLoadFailed: 'Failed to load backup status',
  // ---- Teams ----
  teamsTitle: 'Teams', createTeamTitle: 'Create team', teamNameLabel: 'Team name',
  teamDescLabel: 'Description (optional)', creating: 'Creating…',
  teamsEmpty: 'No teams yet — create one to start collaborating', teamLoadFailed: 'Failed to load teams',
  teamCreateFailed: 'Failed to create team', teamUpdateFailed: 'Failed to update team', teamDeleteFailed: 'Failed to delete team',
  deleteTeamConfirm: 'Delete team “{name}”?', noDesc: 'No description', createdAt: 'Created at',
  backToTeams: '← Back to teams',
  // ---- Team space ----
  membersTitle: 'Members ({n})', addMember: 'Add member', userUUID: 'User UUID', roleLabel: 'Role',
  ownerOnlyHint: 'Only the team owner can manage members.', membersLoadFailed: 'Failed to load members',
  addMemberFailed: 'Failed to add member', removeMemberFailed: 'Failed to remove member', changeRoleFailed: 'Failed to change role',
  customRolesTitle: 'Custom roles ({n})', noCustomRoles: 'No custom roles yet.',
  teamNotFound: 'Team not found: it may have been deleted, or you are not a member',
  teamSpaceEmpty: 'Team space is empty. Upload a file or create a folder to start collaborating',
  permRead: 'Read', permWrite: 'Write', permDelete: 'Delete', permShare: 'Share', permAdmin: 'Admin',
}

export const messages = { 'zh-CN': zh, 'en-US': en } as const
export type MessageKey = keyof typeof zh

/** 查表取当前语言文案（zh 为基准字典，两语言 key 由类型强制对齐）。 */
export function t(locale: Locale, key: MessageKey): string {
  return (locale === 'en-US' ? en : zh)[key]
}

/** 模板占位符替换：'已选 {n} 项' + {n: 3} → '已选 3 项'。 */
export function formatMessage(template: string, params: Record<string, string | number>): string {
  return template.replace(/\{(\w+)\}/g, (m, k: string) => (Object.prototype.hasOwnProperty.call(params, k) ? String(params[k]) : m))
}
