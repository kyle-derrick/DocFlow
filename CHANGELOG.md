# Changelog

本项目的显著变更记录。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

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
