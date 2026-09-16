# Changelog

本项目的显著变更记录。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

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
