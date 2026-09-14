// 版本历史对话框：列出文件全部版本（版本号/大小/时间/sha/status/当前标记），
// 支持回滚到既有版本、「上传新版本」（POST /uploads 携带 file_id 覆盖）与
// 文本版本对比（A/B 两版逐行 LCS diff，行级增/删着色 + 统计）。
// 个人空间与团队空间共用；写权限由后端强制（403 统一提示「无写权限」）。
import { useEffect, useRef, useState } from 'react'
import {
  ApiError,
  FileItem,
  FileVersionDetail,
  UploadPhase,
  VersionStatus,
  fetchVersionText,
  getFileMeta,
  isTextLike,
  listFileVersions,
  restoreVersion,
  uploadFileVersion,
} from '../api'
import { DiffResult, lineDiff } from '../diff'
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
  // 对比模式：A（旧）/ B（新）两个版本 ID，diff 结果与加载态。
  const [compareMode, setCompareMode] = useState(false)
  const [aId, setAId] = useState('')
  const [bId, setBId] = useState('')
  const [diff, setDiff] = useState<DiffResult | null>(null)
  const [diffLoading, setDiffLoading] = useState(false)
  const [diffError, setDiffError] = useState('')

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

  // 文本类判定：MIME（当前版本）命中预览白名单或常见文本扩展名。
  const textLike = isTextLike(file.name, versions[0]?.mime_type ?? '')

  const enterCompare = () => {
    // 默认 A=前一版、B=最新版（versions 按版本号倒序，[0] 为最新）。
    setAId(versions[1]?.id ?? versions[0]?.id ?? '')
    setBId(versions[0]?.id ?? '')
    setDiff(null)
    setDiffError('')
    setCompareMode(true)
  }

  const runCompare = async () => {
    if (!aId || !bId || aId === bId) {
      setDiffError('请选择两个不同的版本')
      return
    }
    setDiffLoading(true)
    setDiffError('')
    setDiff(null)
    try {
      const [aText, bText] = await Promise.all([fetchVersionText(file.id, aId), fetchVersionText(file.id, bId)])
      setDiff(lineDiff(aText, bText))
    } catch (err) {
      setDiffError(err instanceof Error ? err.message : '版本内容读取失败')
    } finally {
      setDiffLoading(false)
    }
  }

  const versionLabel = (v: FileVersionDetail) => `v${v.version}${v.id === currentId ? '（当前）' : ''}`

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
      {compareMode ? (
        <>
          <div className="compare-toolbar">
            <label className="compare-field">
              <span>版本 A（旧）</span>
              <select value={aId} onChange={(e) => setAId(e.target.value)}>
                {versions.map((v) => (
                  <option key={v.id} value={v.id}>
                    {versionLabel(v)}
                  </option>
                ))}
              </select>
            </label>
            <button
              className="btn small ghost"
              type="button"
              title="交换 A/B"
              onClick={() => {
                const next = aId
                setAId(bId)
                setBId(next)
              }}
            >
              ⇄
            </button>
            <label className="compare-field">
              <span>版本 B（新）</span>
              <select value={bId} onChange={(e) => setBId(e.target.value)}>
                {versions.map((v) => (
                  <option key={v.id} value={v.id}>
                    {versionLabel(v)}
                  </option>
                ))}
              </select>
            </label>
            <button className="btn primary small" type="button" disabled={diffLoading} onClick={() => void runCompare()}>
              {diffLoading ? '对比中…' : '对比'}
            </button>
            <button
              className="btn small"
              type="button"
              onClick={() => {
                setCompareMode(false)
                setDiff(null)
                setDiffError('')
              }}
            >
              返回列表
            </button>
          </div>
          {diffError && <div className="error-text">{diffError}</div>}
          {diff && (
            <p className="hint diff-stats">
              共 {diff.rows.length} 行：<span className="diff-stat-add">+{diff.added} 新增</span> ·{' '}
              <span className="diff-stat-del">−{diff.removed} 删除</span> · {diff.rows.length - diff.added - diff.removed} 不变
            </p>
          )}
          {diff && (
            <div className="diff-box">
              {diff.rows.map((row, i) => (
                <div key={i} className={`diff-row ${row.type}`}>
                  <span className="diff-no">{row.aLine ?? ''}</span>
                  <span className="diff-no">{row.bLine ?? ''}</span>
                  <span className="diff-mark">{row.type === 'add' ? '+' : row.type === 'del' ? '−' : ''}</span>
                  <span className="diff-text">{row.text === '' ? ' ' : row.text}</span>
                </div>
              ))}
            </div>
          )}
          {!diff && !diffError && !diffLoading && <div className="empty">选择两个版本后点击「对比」</div>}
        </>
      ) : (
        <>
          <div className="version-toolbar">
            <button className="btn primary small" onClick={() => fileInputRef.current?.click()}>
              ⬆ 上传新版本
            </button>
            <button
              className="btn small"
              disabled={!textLike || versions.length < 2}
              title={!textLike ? '非文本文件暂不支持对比' : versions.length < 2 ? '至少需要两个版本' : '对比两个文本版本'}
              onClick={enterCompare}
            >
              ⇋ 对比版本
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
          {!textLike && versions.length > 0 && <p className="hint version-hint">该文件不是文本类型，暂不支持版本对比。</p>}
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
        </>
      )}
    </Modal>
  )
}
