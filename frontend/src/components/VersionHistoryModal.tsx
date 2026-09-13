// 版本历史对话框：列出文件全部版本（版本号/大小/时间/sha/status/当前标记），
// 支持回滚到既有版本与「上传新版本」（POST /uploads 携带 file_id 覆盖）。
// 个人空间与团队空间共用；写权限由后端强制（403 统一提示「无写权限」）。
import { useEffect, useRef, useState } from 'react'
import {
  ApiError,
  FileItem,
  FileVersionDetail,
  UploadPhase,
  VersionStatus,
  getFileMeta,
  listFileVersions,
  restoreVersion,
  uploadFileVersion,
} from '../api'
import { Modal, formatTime, phaseText } from './FileBrowser'

const statusText: Record<VersionStatus, string> = {
  created: '已创建',
  scanning: '扫描中',
  available: '可用',
  quarantined: '已隔离',
  failed: '失败',
  deleting: '待回收',
}

export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`
}

/** 写操作错误文案：403 统一为「无写权限」（团队 viewer 等），其余透传后端消息。 */
function writeErrorText(err: unknown, fallback: string): string {
  if (err instanceof ApiError && err.status === 403) return '无写权限'
  return err instanceof Error ? err.message : fallback
}

interface Props {
  file: FileItem
  onClose: () => void
  /** 回滚 / 上传新版本成功后回调（调用方刷新文件列表）。 */
  onChanged: () => void
}

export default function VersionHistoryModal({ file, onClose, onChanged }: Props) {
  const [versions, setVersions] = useState<FileVersionDetail[]>([])
  const [currentId, setCurrentId] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busyId, setBusyId] = useState<string | null>(null)
  const [uploadPhase, setUploadPhase] = useState<UploadPhase | 'error' | null>(null)
  const [uploadError, setUploadError] = useState('')
  const fileInputRef = useRef<HTMLInputElement>(null)

  const load = async () => {
    setError('')
    try {
      // 版本列表不含「当前」标记，用文件元数据的 current_version.id 比对（元数据
      // 拉取失败不阻断列表展示，仅退化为无当前标记）。
      const [list, meta] = await Promise.all([
        listFileVersions(file.id),
        getFileMeta(file.id).catch(() => null),
      ])
      setVersions(list)
      setCurrentId(meta?.current_version?.id ?? null)
    } catch (err) {
      setError(err instanceof Error ? err.message : '版本加载失败')
      setVersions([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [file.id])

  const handleRestore = async (v: FileVersionDetail) => {
    if (!window.confirm(`确定将「${file.name}」回滚到版本 v${v.version}？当前版本将指向该历史版本。`)) return
    setBusyId(v.id)
    setError('')
    setNotice('')
    try {
      await restoreVersion(file.id, v.id)
      setNotice(`已回滚到版本 v${v.version}`)
      await load()
      onChanged()
    } catch (err) {
      setError(writeErrorText(err, '回滚失败'))
    } finally {
      setBusyId(null)
    }
  }

  const handleFilePicked = async (files: FileList | null) => {
    const picked = files?.[0]
    if (!picked) return
    setUploadError('')
    setNotice('')
    setUploadPhase('creating')
    try {
      await uploadFileVersion(picked, file.id, (phase) => setUploadPhase(phase))
      setNotice('新版本已上传')
      await load()
      onChanged()
    } catch (err) {
      setUploadPhase('error')
      setUploadError(writeErrorText(err, '上传失败'))
    } finally {
      if (fileInputRef.current) fileInputRef.current.value = ''
    }
  }

  return (
    <Modal wide title={`版本历史「${file.name}」`} onClose={onClose}>
      <div className="version-toolbar">
        <button className="btn primary small" onClick={() => fileInputRef.current?.click()}>
          ⬆ 上传新版本
        </button>
        <input
          ref={fileInputRef}
          type="file"
          hidden
          onChange={(e) => void handleFilePicked(e.target.files)}
        />
        {uploadPhase && <span className={`badge ${uploadPhase}`}>{phaseText[uploadPhase]}</span>}
        {uploadError && <span className="error-text">{uploadError}</span>}
      </div>
      <p className="hint version-hint">
        上传新版本将保留历史（按系统设置的上限裁剪）；回滚仅移动「当前版本」指针，不修改任何历史版本内容。
      </p>

      {notice && <div className="banner ok">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && <p className="hint">加载版本…</p>}
      {!loading && !error && versions.length === 0 && <div className="empty">该文件暂无版本</div>}

      {versions.length > 0 && (
        <table className="file-table version-table">
          <thead>
            <tr>
              <th>版本</th>
              <th>大小</th>
              <th>时间</th>
              <th>SHA-256</th>
              <th>状态</th>
              <th className="col-actions">操作</th>
            </tr>
          </thead>
          <tbody>
            {versions.map((v) => {
              const current = v.id === currentId
              return (
                <tr key={v.id}>
                  <td>
                    <span className="version-no">v{v.version}</span>
                    {current && <span className="badge current">当前</span>}
                  </td>
                  <td className="muted">{formatSize(v.size)}</td>
                  <td className="muted">{formatTime(v.created_at)}</td>
                  <td className="version-sha muted" title={v.sha256}>{v.sha256.slice(0, 12)}</td>
                  <td><span className={`badge ${v.status}`}>{statusText[v.status]}</span></td>
                  <td className="col-actions">
                    {current ? (
                      <span className="muted">—</span>
                    ) : (
                      <button
                        className="btn small"
                        disabled={busyId !== null}
                        onClick={() => void handleRestore(v)}
                      >
                        {busyId === v.id ? '回滚中…' : '回滚到此版本'}
                      </button>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
    </Modal>
  )
}
