// AI 能力可用性（全局 gate + 能力标志，AI 能力第一版）：
// 登录后拉取一次 GET /api/v1/ai/status（能力标志对象：总开关 enabled +
// agent 创作舱 / web_search 联网搜索 / mcp 外部工具 / rag 知识库问答）；
// enabled=false 时全站 AI 入口（顶栏助手/搜索「问 AI」/编辑器 AI 菜单/
// 文件 AI 摘要/drawio AI/PAT ai:chat 提示）一律不渲染，各能力标志驱动
// 对应入口显隐（如 Agent 创作舱按 agent 显隐）。无轮询、无后台任务；
// 管理端改配置后经 refreshAIFeature() 通知刷新（如保存 AI 设置后）。
// 兼容旧版裸 bool 响应：true → 各能力全 true、false → enabled=false。
import { useEffect, useState } from 'react'
import { authFetch } from './api'

export interface AIFeatureStatus {
  enabled: boolean
  agent: boolean
  web_search: boolean
  mcp: boolean
  rag: boolean
}

const allOff: AIFeatureStatus = { enabled: false, agent: false, web_search: false, mcp: false, rag: false }

let status: AIFeatureStatus = allOff
let loaded = false
const listeners = new Set<() => void>()

function notify() {
  for (const fn of listeners) fn()
}

function sameStatus(a: AIFeatureStatus, b: AIFeatureStatus): boolean {
  return a.enabled === b.enabled && a.agent === b.agent && a.web_search === b.web_search && a.mcp === b.mcp && a.rag === b.rag
}

/** 解析 /ai/status 响应（能力对象；兼容旧版裸 bool）。enabled=false 时
 *  子能力一律按 false 处理（总开关关闭 = 全站 AI 入口隐藏）。 */
function parseStatus(data: unknown): AIFeatureStatus {
  if (typeof data === 'boolean') {
    return data ? { enabled: true, agent: true, web_search: true, mcp: true, rag: true } : allOff
  }
  const obj = (data ?? {}) as Partial<Record<keyof AIFeatureStatus, unknown>>
  if (!obj.enabled) {
    return allOff
  }
  return {
    enabled: true,
    agent: obj.agent === true,
    web_search: obj.web_search === true,
    mcp: obj.mcp === true,
    rag: obj.rag === true,
  }
}

/** 拉取 AI 可用性（登录后调用一次；重复调用幂等刷新；失败按全关处理）。 */
export async function loadAIFeature(): Promise<void> {
  let next = allOff
  try {
    const res = await authFetch('/api/v1/ai/status')
    if (res.ok) {
      next = parseStatus(await res.json())
    }
  } catch {
    next = allOff
  }
  loaded = true
  if (!sameStatus(next, status)) {
    status = next
    notify()
  }
}

/** 管理端变更 AI 配置后刷新全站入口显隐。 */
export function refreshAIFeature(): void {
  void loadAIFeature()
}

/** 当前 AI 是否可用（同步读；未加载完成前按 false 处理）。 */
export function aiFeatureEnabled(): boolean {
  return status.enabled
}

/** 当前 AI 能力标志（同步读缓存；未加载完成前全 false）。 */
export function aiFeatureStatus(): AIFeatureStatus {
  return status
}

/** AI 可用性 hook：gate 组件渲染（false = 不渲染入口）。 */
export function useAIEnabled(): boolean {
  return useAIFeatures().enabled
}

/** AI 能力标志 hook：load/refresh 时刷新（各能力入口按标志显隐）。 */
export function useAIFeatures(): AIFeatureStatus {
  const [snap, setSnap] = useState(status)
  useEffect(() => {
    const fn = () => setSnap(status)
    listeners.add(fn)
    if (!loaded) void loadAIFeature()
    return () => {
      listeners.delete(fn)
    }
  }, [])
  return snap
}
