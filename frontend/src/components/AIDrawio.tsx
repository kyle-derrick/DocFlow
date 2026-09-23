// drawio AI 生成（AI 能力第一版）：自然语言描述 → AI 产出 mermaid 代码
//（系统提示约束语法）→ 前端 mermaid 预览 → 复制代码 / 保存为 .mmd 文件
//（同目录新文件，可在 draw.io「排列 → 插入 → 高级 → Mermaid…」粘贴插入
// 画布——embed iframe 协议不支持程序化触发 Mermaid 插入对话框，故按可行
// 性落为 .mmd 文件 + 操作提示）。
import { useState } from 'react'
import { Button, Input, Tooltip } from 'antd'
import { Sparkles, Copy, Save } from 'lucide-react'
import { aiChat, getFileMeta, uploadFile } from '../api'
import { useAIEnabled } from '../aiFeature'
import { Modal } from './FileBrowser'
import MermaidDiagram from './MermaidDiagram'
import { t, useLocale } from '../i18n'
import { useColorMode } from '../theme'

const MERMAID_SYSTEM_PROMPT = '你是图表生成助手。根据用户的自然语言描述输出一段 mermaid 代码（graph TD/flowchart/sequenceDiagram 等）。要求：只输出 mermaid 代码本身，不要 markdown 代码围栏，不要任何解释；节点标签使用用户的原始语言；保持语法严格合法（括号、引号正确闭合）。'

/** drawio 编辑页「AI 生成」入口（AI 未启用不渲染）。 */
export default function AIDrawio({ fileId }: { fileId: string }) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const dark = useColorMode() === 'dark'
  const aiOn = useAIEnabled()
  const [open, setOpen] = useState(false)
  const [prompt, setPrompt] = useState('')
  const [code, setCode] = useState('')
  const [running, setRunning] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [copied, setCopied] = useState(false)
  const [saving, setSaving] = useState(false)

  const generate = async () => {
    const text = prompt.trim()
    if (!text || running) return
    setCode('')
    setError('')
    setNotice('')
    setRunning(true)
    try {
      await aiChat(
        { messages: mermaidMessages(text) },
        {
          onDelta: (chunk) => setCode((prev) => prev + chunk),
        },
      )
    } catch (err) {
      setError(err instanceof Error ? err.message : t(locale, 'aiDrawioFailed'))
    } finally {
      setRunning(false)
    }
  }

  /** AI 对话的系统提示注入（mermaid 语法约束）。 */
  const mermaidMessages = (text: string): Array<{ role: 'system' | 'user'; content: string }> => [
    { role: 'system', content: MERMAID_SYSTEM_PROMPT },
    { role: 'user', content: text },
  ]

  /** 剥掉模型可能带出的 ```mermaid 围栏。 */
  const stripFence = (raw: string): string => {
    let s = raw.trim()
    s = s.replace(/^```(?:mermaid)?\s*\n?/i, '').replace(/\n?```\s*$/i, '')
    return s.trim()
  }

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(stripFence(code))
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch {
      /* 剪贴板权限失败：用户可手动选择复制 */
    }
  }

  /** 保存为 .mmd 文件（当前图表文件同目录）。 */
  const saveAsFile = async () => {
    if (saving) return
    setSaving(true)
    setError('')
    try {
      const meta = await getFileMeta(fileId)
      const base = (meta.name ?? 'diagram').replace(/\.[^.]*$/, '')
      const file = new File([stripFence(code) + '\n'], `${base}-ai.mmd`, { type: 'text/plain' })
      await uploadFile(file, meta.parent_id ?? null, () => {})
      setNotice(t(locale, 'aiDrawioSaved'))
    } catch (err) {
      setError(err instanceof Error ? err.message : t(locale, 'aiDrawioSaveFailed'))
    } finally {
      setSaving(false)
    }
  }

  // AI 未启用（无可用 Provider / 总开关关闭）：不渲染入口。
  if (!aiOn) return null

  return (
    <>
      <Tooltip title={t(locale, 'aiDrawioTitle')} mouseEnterDelay={0.4}>
        <Button size="small" onClick={() => setOpen(true)}>
          <Sparkles size={13} strokeWidth={2} aria-hidden="true" />
          <span>{t(locale, 'aiDrawioGenerate')}</span>
        </Button>
      </Tooltip>
      {open && (
        <Modal title={t(locale, 'aiDrawioTitle')} onClose={() => setOpen(false)} wide>
          <div className="ai-drawio">
            <div className="ai-drawio-input-row">
              <Input.TextArea
                autoSize={{ minRows: 2, maxRows: 4 }}
                value={prompt}
                placeholder={t(locale, 'aiDrawioPrompt')}
                onChange={(e) => setPrompt(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    void generate()
                  }
                }}
              />
              <Button type="primary" loading={running} disabled={!prompt.trim()} onClick={() => void generate()}>
                {t(locale, 'aiDrawioGenerateBtn')}
              </Button>
            </div>
            {running && !code && <div className="muted">{t(locale, 'aiEditRunning')}</div>}
            {error && <div className="error-text">{error}</div>}
            {notice && <div className="banner ok">{notice}</div>}
            {code && (
              <>
                <div className="ai-drawio-preview">
                  <div className="ai-drawio-preview-label muted">{t(locale, 'aiDrawioPreview')}</div>
                  {running ? (
                    <pre className="ai-edit-output">{code}</pre>
                  ) : (
                    <MermaidDiagram source={stripFence(code)} dark={dark} />
                  )}
                </div>
                {!running && (
                  <div className="ai-drawio-code">
                    <div className="ai-drawio-preview-label muted">Mermaid</div>
                    <pre className="ai-edit-output">{stripFence(code)}</pre>
                  </div>
                )}
                <div className="modal-actions">
                  <Button size="small" onClick={() => void copy()}>
                    <Copy size={13} strokeWidth={2} aria-hidden="true" />
                    <span>{copied ? t(locale, 'copied') : t(locale, 'aiDrawioCopy')}</span>
                  </Button>
                  <Button size="small" type="primary" loading={saving} onClick={() => void saveAsFile()}>
                    <Save size={13} strokeWidth={2} aria-hidden="true" />
                    <span>{t(locale, 'aiDrawioSaveFile')}</span>
                  </Button>
                </div>
                <p className="hint">{t(locale, 'aiDrawioInsertHint')}{zh ? '' : ' (menu: Extras → Insert → Advanced → Mermaid…)'}</p>
              </>
            )}
          </div>
        </Modal>
      )}
    </>
  )
}
