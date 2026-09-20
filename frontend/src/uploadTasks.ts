// 全局上传任务 store（v2.6 传输入口移顶栏）：
// - 任务状态提升到模块级单例（React 外普通 state + 订阅广播），跨页面/
//   跨 FileBrowser 实例（含空间切换 key 重挂载）不丢；
// - FileBrowser（文件页/弹窗）写入：addTask/setTaskPhase；顶部栏
//   UploadTasksBell 读取渲染（Badge 进行中数 + Modal 任务面板）；
// - 取消语义与原 FileBrowser 内实现一致：排队中标记跳过（轮到时跳过），
//   进行中 abort 传输（AbortController 注册表）。
import { useSyncExternalStore } from 'react'
import type { UploadPhase } from './api'

/** 上传任务行 phase：UploadPhase + 本地终态（error=失败 / canceled=用户取消）。 */
export type UploadRowPhase = UploadPhase | 'error' | 'canceled'

/** phase → 中文文案（面板行展示）。 */
export const phaseText: Record<UploadRowPhase, string> = {
  creating: '创建会话…',
  uploading: '上传中…',
  completing: '提交处理…',
  verifying: '校验中…',
  scanning: '安全扫描中…',
  available: '已完成',
  quarantined: '已隔离',
  failed: '失败',
  error: '失败',
  canceled: '已取消',
}

/** 上传任务是否仍在进行（Badge 计数与「清空已完成」口径：非进行中即可清）。 */
export function uploadPhaseActive(p: UploadRowPhase): boolean {
  return p !== 'available' && p !== 'error' && p !== 'canceled' && p !== 'quarantined' && p !== 'failed'
}

export interface UploadTask {
  key: number
  name: string
  phase: UploadRowPhase
  error?: string
}

// ---- 模块级单例 state（数组引用仅在变更时替换，useSyncExternalStore 快照稳定） ----

let tasks: UploadTask[] = []
let nextKey = 1
const listeners = new Set<() => void>()

const emit = () => listeners.forEach((fn) => fn())

function subscribe(fn: () => void): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

function snapshot(): UploadTask[] {
  return tasks
}

/** 读取全部上传任务（React hook）。 */
export function useUploadTasks(): UploadTask[] {
  return useSyncExternalStore(subscribe, snapshot, snapshot)
}

/** 登记新任务（phase=creating），返回任务 key。 */
export function addUploadTask(name: string): number {
  const key = nextKey++
  tasks = [...tasks, { key, name, phase: 'creating' }]
  emit()
  return key
}

/** 更新任务 phase（可选错误文案；error 终态时展示）。 */
export function setUploadTaskPhase(key: number, phase: UploadRowPhase, error?: string): void {
  tasks = tasks.map((r) => (r.key === key ? { ...r, phase, error } : r))
  emit()
}

/** 清空已完成任务（保留进行中）。 */
export function clearFinishedUploadTasks(): void {
  tasks = tasks.filter((r) => uploadPhaseActive(r.phase))
  emit()
}

/** 全局任务总数（顶栏入口显隐参考）。 */
export function uploadTaskCount(): number {
  return tasks.length
}

// ---- 取消（排队中标记跳过 / 进行中 abort） ----

const abortControllers = new Map<number, AbortController>()
const cancelRequests = new Set<number>()

/** 登记任务进行中的 AbortController（开始传输时调用）。 */
export function registerUploadAbort(key: number, ctrl: AbortController): void {
  abortControllers.set(key, ctrl)
}

/** 取消上传任务：排队中直接标记「已取消」（轮到时跳过）；进行中 abort。 */
export function requestUploadCancel(key: number): void {
  cancelRequests.add(key)
  const ctrl = abortControllers.get(key)
  if (ctrl) {
    ctrl.abort()
    return
  }
  tasks = tasks.map((r) => (r.key === key && uploadPhaseActive(r.phase) ? { ...r, phase: 'canceled' as const, error: undefined } : r))
  emit()
}

/** 排队任务轮到时消费取消请求：命中则标记「已取消」并返回 true（跳过执行）。 */
export function consumeUploadCancel(key: number): boolean {
  if (!cancelRequests.has(key)) return false
  cancelRequests.delete(key)
  tasks = tasks.map((r) => (r.key === key ? { ...r, phase: 'canceled' as const, error: undefined } : r))
  emit()
  return true
}

/** 任务结束清理（成功/失败/取消后调用；幂等）。 */
export function finishUploadTracking(key: number): void {
  abortControllers.delete(key)
  cancelRequests.delete(key)
}

/** 该 key 是否已被请求取消（错误归类 canceled 用）。 */
export function isUploadCancelRequested(key: number): boolean {
  return cancelRequests.has(key)
}
