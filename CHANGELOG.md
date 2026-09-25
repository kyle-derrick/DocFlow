# Changelog

本项目的显著变更记录。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]

### 文档
- 新增 [docs/architecture.md](docs/architecture.md) 架构总览：总体形态、系统架构与部署拓扑、后端 35 个模块划分、数据模型（内容寻址/空间权限/演进史）、关键链路、前端架构、配置两级模型、设计取舍与已知局限
- 新增 [deploy/env/](deploy/env/) 四套场景化 `.env` 模板（本地开发 / 单机验证 / 单机生产 / 多实例集群），并修正 README 中过时描述（draw.io 已无独立服务、自定义角色已移除、镜像清单与推送示例）

### 修复
- `frontend/Dockerfile` 补 `NODE_OPTIONS=--max-old-space-size=6144`：镜像内 `npm run build` 的 tsc + vite 阶段堆内存溢出（exit 134）导致 caddy 镜像构建失败，与 CI 保持一致

## [1.2.0] - 2026-09-22

AI 全功能体系：多 Provider 网关、RAG 混合检索、联网搜索与思考推理、MCP 双向、AI 创作空间；安全加固与源码一键部署。

### AI Provider 体系
- 多 Provider（平台管理 → AI 设置）：OpenAI 兼容（OpenAI / DeepSeek / Qwen / Ollama / vLLM）、Anthropic、Mock；每 Provider 多模型列表 + 能力勾选（对话 / 向量 / 视觉图片 / 重排序）
- Provider 级限流（次/分钟、日配额）与全局兜底限流、温度 / max_tokens、按用户 / Provider / 模型聚合的用量统计、测试连接
- 场景默认模型（对话 / 摘要 / 编辑器 / 向量，摘要与编辑器可回落对话默认）；ai.enabled 总开关：关闭后全站隐藏入口、AI 网关 404
- 双轨制：个人自备 Provider（设置 → AI 个人配置，Key 掩码不回显、默认模型与人设、「优先使用我的模型」开关，命中个人池跳过平台限流）

### RAG 检索与联网搜索
- 关键词 / 混合（关键词 + 向量）检索模式；向量库 Qdrant 走 ai-vector profile
- embedding 模型从 Provider 池勾选向量能力模型，热切换免重启（collection 按 provider + 模型派生，切换后管理端一键「重建向量索引」）
- rerank 重排（Cohere / Jina 兼容 /rerank 协议，失败静默原序）；chunk / top-k / overlap 可配
- 联网搜索：ai-search profile 启 SearXNG 或配置 Tavily Key；对话可开「联网」，来源以引用展示，8s 超时静默降级
- 图片 OCR：索引管道自动调视觉模型提取图片文字，入全文 + 向量索引（单图上限 1-32MB 可配、热配置）

### 对话体验
- AI 助手 GPT 式抽屉（气泡 / 停止 / 建议 / 模型选择 / 图钉固定）；编辑页对话可直接修改文档（可修改|仅对话模式，自动应用前存版本、消息级撤销）
- 三处对话（助手 / 编辑页 / 创作空间）共享联网·思考·MCP 开关；「思考」开关透传 openai reasoning_effort / anthropic thinking 参数
- AI 记忆：手动增删改 + 自动提取长期偏好（个人开关、去重、上限 100 条）；对话注入最近 20 条（总量 6000 字符截断）
- 人设与技能：平台人设（system 提示模板，全员可选）+ 个人人设；平台技能模板（快捷指令，{selection}/{file} 占位符，助手与编辑页对话可用）

### MCP 双向
- 作为客户端消费外部 MCP 服务器：平台管理配置 ≤8 个 Streamable HTTP 服务 + 鉴权头（支持测试连接）；对话「MCP 工具」开关，工具调用 ≤5 轮，SSE 流式展示工具调用
- 自身作为 MCP Server 对外暴露文档工具（/mcp，21 个 df_* 工具，见 docs/mcp.md）

### AI 创作空间
- 顶部入口：项目绑定空间目录、多会话持久化、目录树 / 任务列表
- Skill 创作模板：建站落地页 / 项目文档 / 接口文档 / 思维导图大纲 / PPT 大纲 / 数据报表
- Agent 任务创建 → 轮询 → Diff 评审 → 应用 / 放弃 / 回滚

### 安全与部署
- 防爆破：登录与 WebDAV Basic 失败锁定（per 用户 + IP）
- WebDAV 挂载 /webdav/{空间名}/{path} 与一次性令牌管理
- 一键源码部署：make deploy-minimal / deploy-full（scripts/deploy.ps1）

## [1.1.1] - 2026-09-18

编辑器升级 + 团队空间关键修复 + 残留清理。

### 编辑器与中文体验
- Monaco（VSCode）编辑器替换文本/源码类编辑与查看（按语言 worker、明暗主题跟随站点、独立懒加载 chunk 不进 PWA 预缓存）
- OnlyOffice 中文字体修复（镜像叠加 fonts-noto-cjk，中文文档渲染不再方块乱码）
- 28 项界面/交互精修（文件列表工具带、目录树导航、弹窗、上传面板等）

### 运维与集成
- 管理页 HTTPS 运行时切换新增 custom 自定义证书模式：上传 PEM 证书/私钥落盘共享卷（tls_certs），经 Caddy admin API 热下发
- 验证栈新增邮件面板（Mailpit，宿主 18025 查看 SMTP 收件箱，配合 SMTP_ENABLED 验证邀请/重置邮件真实投递）
- 用户组管理（另一任务进行中，本版本暂不展开）

### 修复
- 团队空间目录内新建/上传文件报「file not found」：teams 文件列表 handler 的 parent 变量被 := 遮蔽，parent_id 回传全零 UUID（同时导致子目录列表恒为空）；前端回填面包屑后上传指向不存在目录。前后端联动复现并 curl 级验证修复
- /view 页 ?origin_content=1 参数生效：查看页读参透传 resolve，raw_url 按 CONTENT_PUBLIC_BASE_URL 绝对化（跨 origin 内容域场景；未配置回退相对路径）
- 上传轮询（扫描/校验中）保留 complete 响应的 file_id，「创建并打开」不再丢失目标文件；新建流程对 uuid.Nil 防御

### 清理
- 移除已删 Wiki 视图（WorkspaceWikiView）残留：.wiki-* 样式整块、workspace-view-toggle 等
- 死代码清理：api.ts 未用导出（deleteTag/changePassword/updateShare/aiSummarize/adminGetUser 等 9 个函数及随附类型）、无引用 CSS 类、openers.ts 注释遗留
- .tmp/ 与临时诊断脚本加入 .gitignore；E2E files.spec.ts 适配新建菜单弹框交互（文本文件弹框输文件名 + Monaco 断言）

## [1.1.0] - 2026-09-17

信息架构重构 + 知识工作台 + MCP。全栈真实环境验证（26 包单测 + 端到端 API 复验）。

### 信息架构
- 管理后台拆分侧栏子路由（概览/人员/审计/安全/网络/运维/系统）；审计日志独立页：多条件筛选（action/用户/状态/资源/时间）、详情展开、游标上下页、CSV 按筛选完整导出（不再截断 1000）
- 设置中心拆分（资料/外观/安全[含修改密码]/通知/开发者）；右上角用户菜单（头像/昵称/快捷入口）
- 文件页空间切换器（我的文件/各团队/回收站）；回收站支持个人/团队范围；统一表单控件样式与宽屏适配

### 内容与编辑
- Tiptap 富文本编辑器（Markdown 存储、无损往返校验、源码模式切换）；嵌入块：draw.io/Excalidraw 只读内嵌 + 弹窗编辑、Office/网页/文件引用卡片；slash 菜单与图片上传（自动 assets/ 目录）
- 纯查看渲染族：draw.io 官方 viewer 静态渲染、Excalidraw 静态导出、Mermaid/Markmap（md 代码块与 .mmd）、XMind（xmind-embed-viewer，支持一键转 Markdown）
- Wiki 视图（文件/Wiki 双模式，目录树 + 即读即编）；默认打开方式偏好（按扩展名记忆，user_open_with）

### 路径型访问与网页托管
- 路径 URL：/view|/edit/by-path/{personal|team}/{scope}/{path...}；resolve API + 短期 HMAC grant
- /raw/* 受控原始内容（每请求实时鉴权、目录 index.html、Range、沙箱 CSP）——HTML 相对引用 CSS/JS/图片天然生效；zip 一键解包为目录树
- 目录分享：root token + tree 浏览 + 整站预览 + 流式 zip 下载；网页形态"内容决定行为"（index.html 即站点）
- Office：新上传 OOXML 结构校验（拒坏文件）、下载 URL 带真实文件名、回调 MIME 修正；空白 Word/Excel/PowerPoint 模板创建

### 集成与运维
- MCP 服务端（/mcp，21 个 df_* 工具，PAT/Token 鉴权，全部复用现有权限与上传管线）——AI agent 可完全操作 DocFlow
- 目录上传（webkitdirectory 自动建树）、文件夹流式 zip 下载、Caddy /mcp 反代
- 修复：CSRF 双路径 Cookie 兼容清理、多标签页刷新串行化（Web Locks）、OnlyOffice 文档 key 白名单、EnsureRoot 个人根作用域、最近文件有效性过滤

## [1.0.0] - 2026-09-15

首个发布版本：功能全集经真实 docker compose 全栈验证（95 项冒烟 + 8 项外部集成）。

### 核心平台
- 认证：JWT 双 token 轮换、TOTP 两步验证、会话管理、PAT 个人访问令牌、登录限流与锁定、CSRF
- 文件：目录树、tus 断点续传（分片/秒传/SHA-256 去重）、版本链与自动裁剪、回收站、批量操作、配额、标签收藏、预览
- 分享：外链（密码/有效期/水印/访问审计）、团队空间（目录级 ACL、自定义角色与权限范围）
- 运维：审计日志、Prometheus 指标、/ready 就绪探针（db/queue/storage/clamav/onlyoffice/drawio）、janitor 后台清理、备份/恢复与校验和验证

### 集成
- OnlyOffice Document Server：JWT 回调闭环、版本落库、防 SSRF 与幂等
- draw.io 图表编辑、Excalidraw 白板
- ClamAV 病毒扫描（fail closed、拒收与隔离）
- 全文搜索：PostgreSQL 原生 / Meilisearch 可切换
- OIDC 单点登录（授权码 + PKCE、自动开户）
- SMTP 邀请注册与密码重置、Webhook 事件推送、AI 文件摘要（OpenAI 兼容网关）
- 网页包（zip）沙箱预览（独立内容 origin、严格 CSP、解包限制）

### 部署
- docker compose 五 profile 矩阵（minimal/full/antivirus/search/storage），caddy 唯一宿主端口
- 增量 SQL 迁移 + 幂等 seed 引导；多实例横向扩展（Redis 任务队列 + WS Pub/Sub 广播 + S3 存储）
- 前端 React 18 + Vite（PWA）产物并入 caddy 镜像；Playwright E2E 与 95 项运行时冒烟

### 验证
- 真实全栈联调共修复 26 个运行时/编排缺陷（详见 git 历史）；生产 .env 模板与配置校验脚本

[1.0.0]: https://github.com/docflow/docflow/releases/tag/v1.0.0
