// Monaco（VSCode）编辑器封装：
// - monaco-editor 本地打包（loader.config 注入本包实例）：@monaco-editor/react
//   默认从 jsdelivr CDN 拉取 monaco，自托管/内网部署不可依赖外网，此处改为
//   随构建产物分发；语言服务 worker 由 Vite ?worker 产出（按语言分发）；
// - 本模块仅经 React.lazy 动态加载（monaco 产物 3MB+，独立 chunk，进入
//   文本/代码编辑或查看页时才拉取，不进首包与 SW 预缓存，见 vite.config）。
import * as monaco from 'monaco-editor'
import Editor from '@monaco-editor/react'
import { loader } from '@monaco-editor/react'
import editorWorker from 'monaco-editor/esm/vs/editor/editor.worker?worker'
import jsonWorker from 'monaco-editor/esm/vs/language/json/json.worker?worker'
import cssWorker from 'monaco-editor/esm/vs/language/css/css.worker?worker'
import htmlWorker from 'monaco-editor/esm/vs/language/html/html.worker?worker'
import tsWorker from 'monaco-editor/esm/vs/language/typescript/ts.worker?worker'

self.MonacoEnvironment = {
  getWorker(_workerId: string, label: string): Worker {
    switch (label) {
      case 'json':
        return new jsonWorker()
      case 'css':
      case 'scss':
      case 'less':
        return new cssWorker()
      case 'html':
      case 'handlebars':
      case 'razor':
        return new htmlWorker()
      case 'typescript':
      case 'javascript':
        return new tsWorker()
      default:
        return new editorWorker()
    }
  },
}

loader.config({ monaco })

export { monaco }
export default Editor
