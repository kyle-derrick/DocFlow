# AGENTS.md — DocFlow 开发指南

## 项目概览

自托管的企业级文档与文件管理平台（Go 1.22 + React 18/TS）。

- 后端：`cmd/server`，Gin + GORM + pgx，PostgreSQL 16，增量 SQL 迁移
- 前端：`frontend/`，React 18 + TypeScript + Vite（PWA），构建产物并入 Caddy 镜像（无独立前端容器）
- 部署：docker compose，caddy 为唯一宿主端口（80/443）

## 常用命令（以 Makefile 为准）

| 任务 | 命令 |
|---|---|
| 启动后端（:8080） | `make run`（需先备 PostgreSQL + `cp .env.example .env`） |
| 单元测试 | `make test`（`go test ./...`） |
| 格式化 | `make fmt`（gofmt -w） |
| 静态检查 | `go vet ./...` |
| 迁移 / 种子 | `make migrate` / `make seed`（需 `DATABASE_URL`） |
| 前端开发（:5173） | `cd frontend && npm install && npm run dev` |
| 前端构建+类型检查 | `cd frontend && npm run build`（须 `NODE_OPTIONS=--max-old-space-size=6144`，否则 OOM） |
| E2E（Playwright） | `make e2e`（需真实 PostgreSQL + `E2E_DATABASE_URL`） |
| Compose 校验 | `make validate-compose` |
| 一键部署 | `make deploy-minimal` / `make deploy-full`（内部调用 `scripts/deploy.ps1`） |
| 备份 | `make backup` / `make backup-windows` |

本地开发 DB：`postgres://docflow:change-this-password@localhost:5432/docflow`

## CI 必须通过（.github/workflows/ci.yml）

1. `gofmt -l .` 无输出
2. `go vet ./...`
3. `go test -count=1 ./...`
4. 前端：`cd frontend && npm ci && npm run build`（tsc --noEmit + vite build）
5. E2E（Playwright，核心链路 auth/files/share）

提交代码前确保以上全部通过。

## 目录结构

- `cmd/`：server / migrate / seed / backup-verify / wscheck
- `internal/`：业务模块（acl/auth/files/space/share/upload/search/ai/...），handler 集中在 `internal/http/`
- `migrations/`：增量 SQL（`NNN_*.sql` 按文件名序执行、幂等、单文件失败即退出）
- `frontend/`：React SPA 与 Playwright E2E
- `deploy/`：Caddyfile（TLS/反代/子路径）+ `env/` 场景模板
- `scripts/`：备份恢复、集成验证、冒烟（.sh / .ps1 双版本）
- `docs/`：架构总览、配置参考、MCP、OpenAPI 契约

## 开发约定

- 后端新增模块放 `internal/<module>/`，路由在 `internal/http/` 注册；handler 只做参数/鉴权/响应，业务放 service
- 数据库改动必须走增量迁移：`migrations/NNN_<描述>.sql` 按文件名序执行、幂等（`IF NOT EXISTS` / `IF EXISTS`）、单文件失败即退出，**不改写已入库的历史迁移文件**；本地验证 `make migrate`（需 `DATABASE_URL`），改完再跑一遍确认幂等
- 新增/变更 HTTP 接口必须同步 `docs/openapi.yaml` 契约（前后端以契约为准），禁止实现与契约漂移
- 权限链复用 `internal/acl`；队列任务用 asynq（`internal/tasks`）；配置读取走 `internal/config` + 环境变量，新配置须同步 `.env.example`（生产模板同步 `.env.production.example`）
- 前端对齐 antd 6 + TS 既有代码风格
- 安全：不得把密钥/Token 写入代码或仓库；敏感值一律走 `.env`（已 gitignore）
- 本项目运行于 Windows：脚本同时维护 `.sh` 与 `.ps1`，make 目标内部调用 ps1；终端命令默认 PowerShell
- 提交信息风格参考 `git log`：中文，`<type>(<scope>): 描述`

## AI agent 配置约定

- agent 私有目录与配置（`.opencode/`、`.trae/`、`.cursor/`、`opencode.json`、`.mcp.json` 等）**一律不进 git**（已 gitignore）——目前不存在跨 agent 通用的 MCP 客户端配置格式，各 agent 读各自的文件，统一在本机维护
- 跨 agent 通用的仓库约定只走两处：本文件（AGENTS.md 是各家 agent 共识的指令标准）与平台自身对外暴露的 MCP Server（见 `docs/mcp.md`，客户端连接信息以文档为准）
- 通用技能（TDD/调试/docx/pdf 等）放用户级 `~/.claude/skills/`，仓库内不放

## 文档

- 架构 / 数据模型 / 设计取舍：`docs/architecture.md`
- 全量配置：`docs/configuration.md` + `.env.example`
- API 契约：`docs/openapi.yaml`
- MCP 能力：`docs/mcp.md`
