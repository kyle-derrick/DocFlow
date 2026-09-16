import { useEffect, useState } from 'react'
import type { FormEvent, KeyboardEvent } from 'react'
import {
  CreatedShare,
  FileItem,
  Team,
  UUID_RE,
  copyFile,
  createFolder,
  createShare,
  deleteFile,
  getFileMeta,
  listFiles,
  listTeams,
  renameFile,
  uploadFile,
  restoreFile,
} from '../api'
import FileBrowser, { Modal, formatTime } from '../components/FileBrowser'
import VersionHistoryModal from '../components/VersionHistoryModal'
import { useHotkeys } from '../useHotkeys'
import { t, useLocale } from '../i18n'

/** 个人空间：文件浏览复用 FileBrowser，本页仅保留重命名 / 删除 / 分享 / 版本历史对话框。 */
export default function FilesPage() {
  const locale = useLocale()
  const [reloadKey, setReloadKey] = useState(0)
  const refresh = () => setReloadKey((k) => k + 1)

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
  const [shareUserInput, setShareUserInput] = useState('')
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

  const handleDelete = async (item: FileItem) => {
    if (!window.confirm(locale === 'zh-CN' ? `确定删除「${item.name}」？可在回收站中恢复。` : `Delete “${item.name}”? You can restore it from the trash.`)) return
    setDeleteError('')
    try {
      await deleteFile(item.id)
      setRecentlyDeletedId(item.id)
      refresh()
    } catch (err) {
      setDeleteError(err instanceof Error ? err.message : '删除失败')
    }
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
    setShareUserInput('')
    setShareTeams([])
    setShareResult(null)
    setShareError('')
    setCopied(false)
  }

  const addUserChip = () => {
    const value = shareUserInput.trim()
    if (!UUID_RE.test(value) || shareUsers.includes(value)) return
    setShareUsers((prev) => [...prev, value])
    setShareUserInput('')
  }

  const onUserInputKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault()
      addUserChip()
    }
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
    <div className="page">
      {deleteError && <div className="banner error">{deleteError}</div>}
      {recentlyDeletedId && undoSeconds > 0 && (
        <div className="banner ok">
          {locale === 'zh-CN' ? `已删除，${undoSeconds} 秒内可撤销` : `Deleted. Undo within ${undoSeconds}s.`}
          <button className="btn small" onClick={() => void undoDelete()}>{t(locale, 'undo')}</button>
        </div>
      )}

      <FileBrowser
        title="文件"
        rootLabel="我的文件"
        reloadKey={reloadKey}
        listItems={async (parentId, opts) => ({ items: await listFiles(parentId, opts), folderId: parentId })}
        createFolderFn={createFolder}
        uploadFn={uploadFile}
        rootTargetLabel="我的文件（根目录）"
        viewTabs
        copyFn={(fileId, parentId) => copyFile(fileId, parentId)}
        fileMetaFn={(fileId) => getFileMeta(fileId).catch(() => null)}
        rowActions={(item) => (
          <>
            {item.type === 'file' && (
              <button type="button" className="btn small" onClick={() => openShare(item)}>分享</button>
            )}
            {item.type === 'file' && (
              <button type="button" className="btn small" onClick={() => setHistoryTarget(item)}>历史</button>
            )}
            <button
              type="button"
              className="btn small"
              onClick={() => { setRenameTarget(item); setRenameValue(item.name); setRenameError('') }}
            >
              重命名
            </button>
            <button type="button" className="btn small danger" onClick={() => void handleDelete(item)}>删除</button>
          </>
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
              <input autoFocus value={renameValue} onChange={(e) => setRenameValue(e.target.value)} />
            </label>
            {renameError && <div className="error-text">{renameError}</div>}
            <div className="modal-actions">
              <button type="button" className="btn" onClick={() => setRenameTarget(null)}>取消</button>
              <button type="submit" className="btn primary" disabled={!renameValue.trim()}>保存</button>
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
                <div className="seg-group">
                  <button
                    type="button"
                    className={`seg${shareVisibility === 'public' ? ' active' : ''}`}
                    onClick={() => setShareVisibility('public')}
                  >
                    公开链接
                  </button>
                  <button
                    type="button"
                    className={`seg${shareVisibility === 'private' ? ' active' : ''}`}
                    onClick={() => setShareVisibility('private')}
                  >
                    私有分享
                  </button>
                </div>
              </div>
              <label className="field">
                <span>权限</span>
                <select value={sharePermission} onChange={(e) => setSharePermission(e.target.value as 'view' | 'download')}>
                  <option value="download">可下载</option>
                  <option value="view">仅查看</option>
                </select>
              </label>

              {shareVisibility === 'public' ? (
                <p className="hint share-vis-hint">公开分享生成链接，任何拿到链接的人无需登录即可访问。</p>
              ) : (
                <>
                  <div className="field">
                    <span>授权用户（UUID）</span>
                    {shareUsers.length > 0 && (
                      <div className="chips">
                        {shareUsers.map((uid) => (
                          <span key={uid} className="chip">
                            {uid}
                            <button type="button" aria-label="移除" onClick={() => setShareUsers((prev) => prev.filter((u) => u !== uid))}>×</button>
                          </span>
                        ))}
                      </div>
                    )}
                    <div className="chip-input">
                      <input
                        value={shareUserInput}
                        onChange={(e) => setShareUserInput(e.target.value)}
                        onKeyDown={onUserInputKey}
                        placeholder="输入用户 UUID 后添加"
                      />
                      <button
                        type="button"
                        className="btn"
                        disabled={!UUID_RE.test(shareUserInput.trim()) || shareUsers.includes(shareUserInput.trim())}
                        onClick={addUserChip}
                      >
                        添加
                      </button>
                    </div>
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
                <select value={shareHours} onChange={(e) => setShareHours(e.target.value)}>
                  <option value="0">永久</option>
                  <option value="1">1 小时</option>
                  <option value="24">24 小时</option>
                  <option value="168">7 天</option>
                </select>
              </label>
              <label className="field">
                <span>最大下载次数（留空不限）</span>
                <input
                  type="number"
                  min={1}
                  value={shareMax}
                  onChange={(e) => setShareMax(e.target.value)}
                  placeholder="不限"
                />
              </label>
              {shareVisibility === 'public' && (
                <label className="field">
                  <span>访问密码（留空不设密码，4-64 字符）</span>
                  <input
                    type="password"
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
                  <input
                    value={shareWatermarkText}
                    onChange={(e) => setShareWatermarkText(e.target.value)}
                    placeholder="默认模板：{date} {name}（占位符：{email} {date} {name}）"
                  />
                )}
              </div>
              {shareError && <div className="error-text">{shareError}</div>}
              <div className="modal-actions">
                <button type="button" className="btn" onClick={() => setShareTarget(null)}>取消</button>
                <button type="submit" className="btn primary" disabled={submitDisabled}>
                  {shareBusy ? '创建中…' : shareVisibility === 'public' ? '创建链接' : '创建私有分享'}
                </button>
              </div>
            </form>
          ) : shareVisibility === 'public' ? (
            <div>
              <p className="hint">链接已创建。该令牌仅显示一次，关闭对话框后无法再次查看，请立即复制保存。</p>
              <div className="share-link">
                <input readOnly value={shareLink} onFocus={(e) => e.currentTarget.select()} />
                <button className="btn primary" onClick={() => void copyLink()}>{copied ? '已复制 ✓' : '复制'}</button>
              </div>
              {shareResult.has_password && (
                <p className="hint">🔒 已启用密码保护：访问者须输入密码解锁（1 小时会话）。</p>
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
                <button className="btn" onClick={() => setShareTarget(null)}>关闭</button>
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
                <button className="btn" onClick={() => setShareTarget(null)}>关闭</button>
              </div>
            </div>
          )}
        </Modal>
      )}
    </div>
  )
}
