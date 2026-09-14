import { createContext, useContext } from 'react'

export type Locale = 'zh-CN' | 'en-US'
export const LocaleContext = createContext<Locale>('zh-CN')
export const localeStorageKey = 'docflow.locale'
export function loadLocale(): Locale {
  return localStorage.getItem(localeStorageKey) === 'en-US' ? 'en-US' : 'zh-CN'
}
export function saveLocale(locale: Locale): void { localStorage.setItem(localeStorageKey, locale) }
export function useLocale(): Locale { return useContext(LocaleContext) }
export const messages = {
  'zh-CN': {
    login: '登录', logout: '退出登录', files: '文件', settings: '设置', upload: '上传文件', empty: '此目录为空',
    language: '语言', switchLanguage: 'English', loading: '加载中…', delete: '删除', undo: '撤销',
    deleteConfirm: '确定删除此项？可在回收站中恢复。', deleteFailed: '删除失败', restoreFailed: '恢复失败',
    noMatch: '没有匹配的文件', close: '关闭', download: '下载', previewFailed: '预览加载失败',
  },
  'en-US': {
    login: 'Log in', logout: 'Log out', files: 'Files', settings: 'Settings', upload: 'Upload file', empty: 'This folder is empty',
    language: 'Language', switchLanguage: '中文', loading: 'Loading…', delete: 'Delete', undo: 'Undo',
    deleteConfirm: 'Delete this item? You can restore it from the trash.', deleteFailed: 'Delete failed', restoreFailed: 'Restore failed',
    noMatch: 'No matching files', close: 'Close', download: 'Download', previewFailed: 'Failed to load preview',
  },
} as const
export function t(locale: Locale, key: keyof typeof messages['zh-CN']): string { return messages[locale][key] }
