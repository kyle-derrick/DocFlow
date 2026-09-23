// 编辑器 AI（AI 能力第一版）：工具栏「AI」下拉——续写/润色/翻译(中/英)/
// 自定义指令，作用于选区或全文（选区为空时作用于全文），流式预览后
// 「插入 / 替换 / 放弃」三按钮。Monaco（md/text/code）与 Tiptap（.dfdoc）
// 编辑页共用：宿主页面提供选区读取与结果落盘回调。
import { useState } from 'react'
import { Button, Dropdown, Input } from 'antd'
import type { MenuProps } from 'antd'
import { Sparkles } from 'lucide-react'
import { aiChat } from '../api'
import { useAIEnabled } from '../aiFeature'
import { Modal } from './FileBrowser'
import { t, useLocale } from '../i18n'

/** 选区描述（无选区 = 全文模式）。 */
export interface AIEditTarget {
  text: string
  hasSelection: boolean
}

type AIAction = 'summary' | 'continue' | 'polish' | 'translate-zh' | 'translate-en' | 'custom'

/** 输入文本送入模型的最大长度（超限截取尾部——续写关注结尾语境）。 */
const MAX_INPUT_CHARS = 6000

const actionPrompt = (locale: string, action: AIAction, instruction: string): { system: string; user: string } => {
  const zh = locale === 'zh-CN'
  switch (action) {
    case 'summary':
      return {
        system: zh ? '你是文档摘要助手。请摘要给定文本：先一句话概括，再列 3-6 条要点，保持原文语言。只输出摘要。' : 'You are a summarizer. Summarize the given text: one sentence overview then 3-6 bullet points, in the original language. Output only the summary.',
        user: zh ? '请摘要以下文本：' : 'Summarize the following text:',
      }
    case 'continue':
      return {
        system: zh ? '你是写作助手。请顺着给定文本自然续写，长度 100-300 字，只输出续写的新内容本身，不要重复原文，不要任何解释。' : 'You are a writing assistant. Continue the given text naturally for 100-300 words. Output only the new continuation, without repeating the original or explanations.',
        user: zh ? '请续写以下文本：' : 'Continue the following text:',
      }
    case 'polish':
      return {
        system: zh ? '你是文字编辑。请润色给定文本：保持原意、语言与大体结构，使其更通顺、简洁、专业。只输出润色后的完整文本，不要任何解释。' : 'You are a copy editor. Polish the given text while keeping its meaning, language and structure. Output only the polished text.',
        user: zh ? '请润色以下文本：' : 'Polish the following text:',
      }
    case 'translate-zh':
      return {
        system: '你是翻译。把给定文本翻译成简体中文，保持原有格式（markdown/换行）。只输出译文。',
        user: '请翻译为中文：',
      }
    case 'translate-en':
      return {
        system: 'You are a translator. Translate the given text into English, preserving formatting (markdown/line breaks). Output only the translation.',
        user: 'Translate into English:',
      }
    default:
      return {
        system: zh ? `你是文本处理助手。按用户指令处理给定文本，只输出处理结果，不要任何解释。指令：${instruction}` : `You are a text processing assistant. Process the given text per the user instruction and output only the result. Instruction: ${instruction}`,
        user: zh ? '请处理以下文本：' : 'Process the following text:',
      }
  }
}

/**
 * 编辑器 AI 菜单 + 预览弹窗。
 * onApply(mode)：insert = 在选区末尾/光标处插入；replace = 替换选区
 *（无选区时替换全文）。
 */
export default function AIEditMenu({
  getTarget,
  getAllText,
  onApply,
  disabled,
}: {
  /** 当前选区（text 为选中文本；无选区时返回全文与 hasSelection=false）。 */
  getTarget: () => AIEditTarget
  /** 全文（选区为空时的输入与替换目标）。 */
  getAllText: () => string
  /** 应用结果（宿主页面负责编辑器落盘）。 */
  onApply: (mode: 'insert' | 'replace', output: string) => void
  disabled?: boolean
}) {
  const locale = useLocale()
  const aiOn = useAIEnabled()
  const [open, setOpen] = useState(false)
  const [customOpen, setCustomOpen] = useState(false)
  const [instruction, setInstruction] = useState('')
  const [output, setOutput] = useState('')
  const [running, setRunning] = useState(false)
  const [error, setError] = useState('')
  const [target, setTarget] = useState<AIEditTarget>({ text: '', hasSelection: false })
  // 最近一次执行的动作（重新生成用）。
  const [lastRun, setLastRun] = useState<{ action: AIAction; instruction: string } | null>(null)

  const runAction = async (action: AIAction, customInstruction: string) => {
    const tgt = getTarget()
    const full = tgt.hasSelection ? tgt.text : getAllText()
    const input = full.length > MAX_INPUT_CHARS ? full.slice(full.length - MAX_INPUT_CHARS) : full
    if (!input.trim()) {
      setError(locale === 'zh-CN' ? '文本为空' : 'Empty input')
      return
    }
    setTarget({ text: full, hasSelection: tgt.hasSelection })
    setLastRun({ action, instruction: customInstruction })
    setOpen(true)
    setOutput('')
    setError('')
    setRunning(true)
    const { system, user } = actionPrompt(locale, action, customInstruction)
    try {
      await aiChat(
        {
          messages: [
            { role: 'system', content: system },
            { role: 'user', content: `${user}\n\n${input}` },
          ],
        },
        {
          onDelta: (chunk) => setOutput((prev) => prev + chunk),
        },
      )
    } catch (err) {
      setError(err instanceof Error ? err.message : t(locale, 'aiAssistantErr'))
    } finally {
      setRunning(false)
    }
  }

  const items: MenuProps['items'] = [
    { key: 'summary', label: t(locale, 'aiEditSummary') },
    { key: 'continue', label: `${t(locale, 'aiEditContinue')}` },
    { key: 'polish', label: t(locale, 'aiEditPolish') },
    { key: 'translate-zh', label: t(locale, 'aiEditTranslateZh') },
    { key: 'translate-en', label: t(locale, 'aiEditTranslateEn') },
    { type: 'divider' },
    { key: 'custom', label: t(locale, 'aiEditCustom') },
  ]
  const onMenuClick: MenuProps['onClick'] = ({ key }) => {
    if (key === 'custom') {
      setInstruction('')
      setCustomOpen(true)
      return
    }
    void runAction(key as AIAction, '')
  }

  const apply = (mode: 'insert' | 'replace') => {
    if (!output || running) return
    onApply(mode, output)
    setOpen(false)
  }

  // AI 未启用（无可用 Provider / 总开关关闭）：不渲染菜单入口。
  if (!aiOn) return null

  return (
    <>
      <Dropdown menu={{ items, onClick: onMenuClick }} trigger={['click']} disabled={disabled}>
        <Button size="small" disabled={disabled}>
          <Sparkles size={13} strokeWidth={2} aria-hidden="true" />
          <span>{t(locale, 'aiEdit')}</span>
        </Button>
      </Dropdown>
      {/* 自定义指令输入。 */}
      {customOpen && (
        <Modal
          title={t(locale, 'aiEditCustomLabel')}
          onClose={() => setCustomOpen(false)}
        >
          <Input.TextArea
            autoSize={{ minRows: 2, maxRows: 5 }}
            value={instruction}
            placeholder={locale === 'zh-CN' ? '如：把这段改写成正式邮件语气' : 'e.g. rewrite in a formal email tone'}
            onChange={(e) => setInstruction(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey && instruction.trim()) {
                e.preventDefault()
                setCustomOpen(false)
                void runAction('custom', instruction.trim())
              }
            }}
          />
          <div className="modal-actions">
            <Button type="primary" disabled={!instruction.trim()} onClick={() => { setCustomOpen(false); void runAction('custom', instruction.trim()) }}>
              {locale === 'zh-CN' ? '执行' : 'Run'}
            </Button>
            <Button onClick={() => setCustomOpen(false)}>{t(locale, 'cancel')}</Button>
          </div>
        </Modal>
      )}
      {/* 流式预览 + 插入/替换/放弃。 */}
      {open && (
        <Modal
          title={`${t(locale, 'aiEditPreview')}${target.hasSelection ? t(locale, 'aiEditSelection') : t(locale, 'aiEditWhole')}`}
          onClose={() => setOpen(false)}
          wide
        >
          <div className="ai-edit-output-wrap">
            {running && !output && <div className="muted">{t(locale, 'aiEditRunning')}</div>}
            {output && <pre className="ai-edit-output">{output}</pre>}
            {running && output && <span className="ai-caret" />}
            {error && <div className="error-text">{error}</div>}
          </div>
          <div className="modal-actions">
            <Button type="primary" disabled={!output || running} onClick={() => apply('insert')}>
              {t(locale, 'aiEditInsert')}
            </Button>
            <Button disabled={!output || running} onClick={() => apply('replace')}>
              {t(locale, 'aiEditReplace')}
            </Button>
            <Button disabled={running || !lastRun} onClick={() => lastRun && void runAction(lastRun.action, lastRun.instruction)}>
              {t(locale, 'aiEditRegenerate')}
            </Button>
            <Button onClick={() => setOpen(false)}>{t(locale, 'aiEditDiscard')}</Button>
          </div>
        </Modal>
      )}
    </>
  )
}
