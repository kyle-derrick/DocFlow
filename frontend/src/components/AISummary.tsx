// 查看侧 AI 摘要（AI 能力第一版，全文件类型）：
// - AISummaryInline：查看弹窗内嵌（按钮 + 展开式流式摘要卡片）；
// - AISummaryFloating：独立查看页右下角浮动按钮 + 卡片（覆盖 office/
//   drawio/白板/pdf/md 等全部类型的分发查看器）；
// 流式渲染走 /ai/summarize SSE（打字机效果 + markdown）。
import { useEffect, useState } from 'react'
import { Button } from 'antd'
import { Sparkles, X } from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { aiSummarizeFileStream } from '../api'
import { useAIEnabled } from '../aiFeature'
import { t, useLocale } from '../i18n'

/** 摘要生成状态。 */
interface SummaryState {
  loading: boolean
  text: string
  error: string
}

function useAISummary(fileId: string) {
  const [state, setState] = useState<SummaryState>({ loading: false, text: '', error: '' })
  const run = async () => {
    if (state.loading) return
    setState({ loading: true, text: '', error: '' })
    try {
      await aiSummarizeFileStream(fileId, (chunk) => {
        setState((prev) => ({ ...prev, text: prev.text + chunk }))
      })
    } catch (err) {
      setState((prev) => ({ ...prev, error: err instanceof Error ? err.message : t('zh-CN', 'aiSummaryFailed') }))
    } finally {
      setState((prev) => ({ ...prev, loading: false }))
    }
  }
  return { state, run }
}

function SummaryCardBody({ state }: { state: SummaryState }) {
  const locale = useLocale()
  return (
    <>
      {state.loading && !state.text && <div className="muted">{t(locale, 'aiSummaryLoading')}</div>}
      {state.text && (
        <div className="markdown-preview ai-summary-markdown">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{state.text}</ReactMarkdown>
        </div>
      )}
      {state.loading && state.text && <span className="ai-caret" />}
      {state.error && <div className="error-text">{state.error}</div>}
    </>
  )
}

/** 查看弹窗内嵌形态：按钮行 + 展开的摘要卡片（AI 未启用不渲染）。 */
export function AISummaryInline({ fileId }: { fileId: string }) {
  const locale = useLocale()
  const aiOn = useAIEnabled()
  const { state, run } = useAISummary(fileId)
  const [open, setOpen] = useState(false)
  if (!aiOn) return null
  return (
    <div className="ai-summary-inline">
      <Button
        size="small"
        loading={state.loading}
        onClick={() => {
          const next = !open
          setOpen(next)
          if (next && !state.text && !state.error) void run()
        }}
      >
        <Sparkles size={13} strokeWidth={2} aria-hidden="true" />
        <span>{t(locale, 'aiSummary')}</span>
      </Button>
      {open && <div className="ai-summary-card"><SummaryCardBody state={state} /></div>}
    </div>
  )
}

/** 独立查看页浮动形态：右下角固定按钮 + 弹出卡片（AI 未启用不渲染）。 */
export function AISummaryFloating({ fileId, name }: { fileId: string; name: string }) {
  const locale = useLocale()
  const aiOn = useAIEnabled()
  const { state, run } = useAISummary(fileId)
  const [open, setOpen] = useState(false)
  if (!aiOn) return null
  return (
    <div className="ai-summary-floating">
      {open && (
        <div className="ai-summary-card" role="region" aria-label={`${t(locale, 'aiSummary')}：${name}`}>
          <div className="ai-summary-head">
            <span className="ai-summary-title">
              <Sparkles size={13} strokeWidth={2} aria-hidden="true" />
              {t(locale, 'aiSummary')}
              <span className="muted ai-summary-name">{name}</span>
            </span>
            <Button type="text" size="small" aria-label={t(locale, 'close')} onClick={() => setOpen(false)}>
              <X size={14} strokeWidth={2} aria-hidden="true" />
            </Button>
          </div>
          <SummaryCardBody state={state} />
          {!state.loading && state.text && (
            <div className="ai-summary-actions">
              <Button size="small" onClick={() => void run()}>{locale === 'zh-CN' ? '重新生成' : 'Regenerate'}</Button>
            </div>
          )}
        </div>
      )}
      <Button
        type="primary"
        shape="circle"
        className="ai-summary-fab"
        title={t(locale, 'aiSummary')}
        aria-label={`${t(locale, 'aiSummary')}：${name}`}
        loading={state.loading && !open}
        onClick={() => {
          const next = !open
          setOpen(next)
          if (next && !state.text && !state.error) void run()
        }}
      >
        {!state.loading || open ? <Sparkles size={18} strokeWidth={2} aria-hidden="true" /> : undefined}
      </Button>
    </div>
  )
}

/** 右键菜单「AI 摘要」弹窗形态：文件页条目菜单唤起，卡片体复用。 */
export function AISummaryDialog({ fileId, name }: { fileId: string; name: string }) {
  const locale = useLocale()
  const { state, run } = useAISummary(fileId)
  // 打开即生成一次（弹窗形态无展开按钮）。
  useEffect(() => {
    if (!state.text && !state.error && !state.loading) void run()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId])
  return (
    <div className="ai-summary-card ai-summary-dialog">
      <div className="ai-summary-head">
        <span className="ai-summary-title">
          <Sparkles size={13} strokeWidth={2} aria-hidden="true" />
          {t(locale, 'aiSummary')}
          <span className="muted ai-summary-name">{name}</span>
        </span>
      </div>
      <SummaryCardBody state={state} />
      {!state.loading && state.text && (
        <div className="ai-summary-actions">
          <Button size="small" onClick={() => void run()}>{locale === 'zh-CN' ? '重新生成' : 'Regenerate'}</Button>
        </div>
      )}
    </div>
  )
}
