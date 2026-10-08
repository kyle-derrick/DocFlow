// AI 对话共享组件族（@ant-design/x）：三处对话场景（Studio 右栏 / 全局
// AI 助手 / 编辑器侧栏）统一消息流、思考过程、工具调用链与输入区。
export {
  type AIToolStatus, type AIToolCallEntry, type AIChatTurnData, type AIAttachFile, type AIWebSource,
  toolEntryFrom, applyToolResult, dfToolLabel, isWriteTool, normalizeWebSources, thinkingTitle,
} from './turns'
export {
  AIMessageList, AIChatThinking, AIToolChain, AIWebSourcesView, AIAssistantMessageBody, AIUserMessageBody,
} from './AIMessageList'
export { default as AIChatComposer } from './AIChatComposer'
export { detectMention } from './AIChatComposer'
export { CHAT_SEND_ICON, CHAT_STOP_ICON } from './icons'
