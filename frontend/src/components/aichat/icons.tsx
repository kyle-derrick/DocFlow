// AI 对话共享图标（发送/停止按钮专用，各对话场景同款）：
// AIAssistant（助理）/ AIEditChat（编辑页）/ ViewerAIWidget（查看页）
// 的工具栏最右端发送⇄停止按钮共用，避免内联 SVG 多处漂移。
import type { ReactElement } from 'react'

/** 发送按钮图标（纸飞机，chat-send-btn 用）。 */
export const CHAT_SEND_ICON: ReactElement = (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="m22 2-7 20-4-9-9-4Z" /><path d="M22 2 11 13" /></svg>
)

/** 停止按钮图标（实心方块，chat-stop-btn 用）。 */
export const CHAT_STOP_ICON: ReactElement = (
  <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><rect x="6" y="6" width="12" height="12" rx="2" /></svg>
)
