import { useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { Lock } from 'lucide-react'
import { App as AntdApp, Button, Input, Segmented, Select } from 'antd'
import {
  CreatedShare,
  FileItem,
  Team,
  UserSearchResult,
  copyFile,
  createFolder,
  createShare,
  currentUserId,
  deleteFile,
  getFileMeta,
  listFiles,
  listTeams,
  renameFile,
  restoreFile,
  searchUsers,
  uploadFile,
} from '../api'
import { Modal, formatTime } from '../components/FileBrowser'
import { FileBrowserWithTree } from '../components/FolderTreeNav'
import VersionHistoryModal from '../components/VersionHistoryModal'
import SpaceSwitcher from '../components/SpaceSwitcher'
import { useHotkeys } from '../useHotkeys'
import { t, useLocale } from '../i18n'

/** 个人空间：文件浏览复用 FileBrowser（左侧目录树见 FolderTreeNav），本页仅保留
 * 重命名 / 删除 / 分享 / 版本历史对话框。视图（全部/收藏/最近）状态由本页
 * 持有，经 SpaceSwitcher 切换、透传 FileBrowser 的 activeView 受控入口。 */
export default function FilesPage() {
  const locale = useLocale()
  const { modal: antdModal } = AntdApp.useApp()
  const [reloadKey, setReloadKey] = useState(0)
  const refresh = () => setReloadKey((k) => k + 1)
  // 全局视图（全部 / 收藏 / 最近）：驱动 FileBrowser 的检索模式。
  const [spaceView, setSpaceView] = useState<'all' | 'starred' | 'recent'>('all')
  // 个人空间命名空间（「作为网页打开」resolve 用）：scope = 自己 user UUID（JWT sub）。
  const meId = currentUserId()

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

  const [shareTarget, setShareTarget] = useState<FileItem | null>(null)
  const [shareVisibility, setShareVisibility] = useState<'public' | 'private'>('public')
  const [sharePermission, setSharePermission] = useState<'view' | 'download'>('download')
  const [shareHours, setShareHours] = useState('0')
  const [shareMax, setShareMax] = useState('')
  const [sharePassword, setSharePassword] = useState('')
  const [shareWatermark, setShareWatermark] = useState(true)
  const [shareWatermarkText, setShareWatermarkText] = useState('')
  const [shareUsers, setShareUsers] = useState<string[]>([])
  // 授权用户远程搜索（v1.6：替代手输 UUID；防抖 300ms，≥2 字触发）。
  const [shareUserQuery, setShareUserQuery] = useState('')
  const [shareUserOptions, setShareUserOptions] = useState<UserSearchResult[]>([])
  const [shareUserSearching, setShareUserSearching] = useState(false)
  // 已选用户的显示名缓存（id → 昵称/用户名），供多选框回显。
  const shareUserNameRef = useRef(new Map<string, string>())
  const [shareTeams, setShareTeams] = useState<string[]>([])
  const [teams, setTeams] = useState<Team[]>([])
  const [shareResult, setShareResult] = useState<CreatedShare | null>(null)
  const [shareBusy, setShareBusy] = useState(false)
  const [shareError, setShareError] = useState('')
  const [copied, setCopied] = useState(false)

  // 分享对话框的团队多选数据源（我的团队列表）。
  useEffect(() => {
    void listTeams()
      .then(setTeams)
      .catch(() => {})
  }, [])

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
    setShareWatermarkText('')
    setShareUsers([])
    setShareUserQuery('')
    setShareUserOptions([])
    setShareTeams([])
    setShareResult(null)
    setShareError('')
    setCopied(false)
  }

  const toggleShareTeam = (teamId: string) => {
    setShareTeams((prev) => (prev.includes(teamId) ? prev.filter((t) => t !== teamId) : [...prev, teamId]))
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
        teamIds: shareVisibility === 'private' ? shareTeams : undefined,
        password: shareVisibility === 'public' ? sharePassword.trim() : undefined,
        watermarkEnabled: shareWatermark,
        watermarkText: shareWatermarkText.trim() || undefined,
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

  const privateGrantCount = shareUsers.length + shareTeams.length
  const sharePasswordLen = sharePassword.trim().length
  const submitDisabled =
    shareBusy ||
    (shareVisibility === 'private' && privateGrantCount === 0) ||
    (shareVisibility === 'public' && sharePasswordLen > 0 && (sharePasswordLen < 4 || sharePasswordLen > 64))

  // Escape 依次关本页对话框（版本历史→重命名→分享）；FileBrowser 的选择与
  // 内置弹窗由其自身 Escape 处理（见 FileBrowser）。
  useHotkeys({
    Escape: () => {
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

  return (
    <div className="page wide-page files-page">
      {deleteError && <div className="banner error">{deleteError}</div>}
      {recentlyDeletedId && undoSeconds > 0 && (
        <div className="banner ok">
          {locale === 'zh-CN' ? `已删除，${undoSeconds} 秒内可撤销` : `Deleted. Undo within ${undoSeconds}s.`}
          <Button size="small" onClick={() => void undoDelete()}>{t(locale, 'undo')}</Button>
        </div>
      )}

      <FileBrowserWithTree
        rootLabel="我的文件"
        reloadKey={reloadKey}
        toolbarPrefix={<SpaceSwitcher activeView={spaceView} onViewChange={setSpaceView} />}
        listItems={async (parentId, opts) => ({ items: await listFiles(parentId, opts), folderId: parentId })}
        createFolderFn={createFolder}
        uploadFn={uploadFile}
        activeView={spaceView}
        copyFn={(fileId, parentId) => copyFile(fileId, parentId)}
        fileMetaFn={(fileId) => getFileMeta(fileId).catch(() => null)}
        ns={meId ? { type: 'personal', scope: meId } : undefined}
        shareFn={openShare}
        renameFn={(item) => { setRenameTarget(item); setRenameValue(item.name); setRenameError('') }}
        deleteFn={confirmDelete}
        rowActions={(item) => (
          <div className="row-actions-group">
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

              {shareVisibility === 'public' ? (
                <p className="hint share-vis-hint">公开分享生成链接，任何拿到链接的人无需登录即可访问。</p>
              ) : (
                <>
                  <div className="field">
                    <span>授权用户</span>
                    {/* v1.6：远程搜索多选（昵称/用户名/邮箱 ≥2 字），替代手输 UUID。 */}
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
                    <span>授权团队</span>
                    {teams.length === 0 ? (
                      <p className="hint">你暂未加入任何团队</p>
                    ) : (
                      <div className="check-list">
                        {teams.map((t) => (
                          <label key={t.id} className="check-item">
                            <input
                              type="checkbox"
                              checked={shareTeams.includes(t.id)}
                              onChange={() => toggleShareTeam(t.id)}
                            />
                            <span>{t.name}</span>
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
                    value={shareWatermarkText}
                    onChange={(e) => setShareWatermarkText(e.target.value)}
                    placeholder="默认模板：{date} {name}（占位符：{email} {date} {name}）"
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
              <p className="hint">链接已创建。该令牌仅显示一次，关闭对话框后无法再次查看，请立即复制保存。</p>
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
              <p className="hint">私有分享已创建。被授权的用户与团队成员登录 DocFlow 后即可访问，无公开链接。</p>
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
    </div>
  )
}
