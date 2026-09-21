// 公开分享渲染上下文：富文本编辑器以 readonly 渲染公开分享的 .dfrt 时，
// 节点视图（文件卡片/图片/嵌入块）无法走认证 API 取内容，改经分享树
// raw/share 端点解析资源 URL（见 publicResource.ts）。null = 认证态渲染。
import { createContext, useContext } from 'react'

export interface RichTextPublicBase {
  /** /raw/share/{token}/{grant}（10 分钟授权基址，SharePage tree API 下发）。 */
  rawBase: string
  /** dfrt 自身相对分享根的路径（解析同级 assets/ 等相对资源）。 */
  docPath: string
  /** 分享 token（v2.7 引用资源 refs 支持：Office 引用走公开 office 会话
   *（/public/shares/:token/office?ref=）；空串 = 未提供（仅 raw 解析）。 */
  token?: string
}

const RichTextPublicContext = createContext<RichTextPublicBase | null>(null)

export const RichTextPublicProvider = RichTextPublicContext.Provider

/** 当前渲染是否处于公开分享态（true 时节点视图走 raw/share 解析）。 */
export function useRichTextPublic(): RichTextPublicBase | null {
  return useContext(RichTextPublicContext)
}
