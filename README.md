# DocFlow

自托管的企业级文档与文件管理平台：团队空间、细粒度权限、断点续传上传、在线编辑（OnlyOffice / draw.io / Excalidraw）、全文搜索、病毒扫描、OIDC 单点登录与完整审计。

- 后端：Go 1.22（Gin + GORM），PostgreSQL 16，增量 SQL 迁移
- 前端：React 18 + TypeScript + Vite（PWA），构建产物并入 Caddy 镜像，无独立前端容器
- 部署：docker compose 一键起，caddy 为唯一宿主端口（80/443）

## 界面预览

| | |
|---|---|
| ![登录](docs/screenshots/login.png) | ![文件管理](docs/screenshots/files.png) |
| ![目录树文件浏览](docs/screenshots/files.png) | ![富文本编辑](docs/screenshots/richtext.png) |
| ![在线查看](docs/screenshots/viewer.png) | ![管理后台-审计](docs/screenshots/admin-audit.png) |
| ![管理后台-TLS](docs/screenshots/admin-tls.png) | ![设置-安全](docs/screenshots/settings-security.png) |

## 功能特性

**文件管理**
- tus 协议断点续传上传、分片与秒传（SHA-256 去重）
- 文件版本链（上限可配、自动裁剪）、回收站与保留期清理
- 批量操作（移动/删除/下载 zip）、配额管理、标签与收藏
- 网页包（zip）安全预览：严格 CSP 沙箱 + 解包限制 + 独立内容 origin

**协作与分享**
- 团队空间：目录级 ACL、自定义角色与权限范围
- 外链分享：密码保护、有效期、水印、访问事件审计
- WebSocket 实时通知（多实例经 Redis Pub/Sub 广播）

**集成**
- OnlyOffice Document Server（JWT 回调闭环、版本落库、防 SSRF）
- draw.io 图表编辑、Excalidraw 白板、内置 Monaco 文本/源码编辑器（VSCode 同款，明暗主题跟随）
- ClamAV 病毒扫描（fail closed、隔离与拒收）
- 全文搜索：PostgreSQL 原生或 Meilisearch（可切换）
- OIDC 单点登录（授权码 + PKCE，自动开户）
- SMTP 邀请注册 / 密码重置、Webhook 事件推送、AI 文件摘要（OpenAI 兼容）

**安全与运维**
- TOTP 两步验证、会话管理、PAT 个人访问令牌、登录限流与锁定
- 审计日志、Prometheus 指标、/ready 六项就绪探针
- 备份 / 恢复脚本与校验和验证

## 架构与服务矩阵

| Profile | 服务 | 说明 |
|---|---|---|
| minimal | caddy + backend + postgres | 基础栈，`make up-minimal` |
| full | + redis + onlyoffice + drawio | `make up-full`（队列 / WS 广播 / 在线编辑） |
| antivirus | + clamav | 病毒扫描，叠加 `--profile antivirus` |
| search | + meilisearch | 全文搜索，叠加 `--profile search` |
| storage | + minio | S3 对象存储，叠加 `--profile storage` |

## 打包

两个自建镜像，均由 compose 构建（`docker compose build` 或随 `up --build`）：

| 镜像 | 构建上下文 | 内容 |
|---|---|---|
| `docflow/backend:1.0.0` | `./`（Dockerfile） | Go 多阶段构建：`/docflow` 服务、`/migrate`、`/seed`，含 migrations |
| `docflow/caddy:1.0.0` | `./frontend`（frontend/Dockerfile） | 前端 Vite 构建产物 → Caddy 静态托管 + 反代（TLS 自动签发） |

推送到镜像仓库后即可在任意装了 docker compose 的主机部署：

```bash
docker build -t <registry>/docflow-backend:1.0.0 .
docker build -t <registry>/docflow-caddy:1.0.0 ./frontend
docker push <registry>/docflow-backend:1.0.0 <registry>/docflow-caddy:1.0.0
```

## 部署（生产）

三步：

```bash
# 1. 配置：从生产模板生成 .env，替换全部 CHANGE_ME 占位符
cp .env.production.example .env
vi .env   # JWT_SECRET / POSTGRES_PASSWORD / SEED_ADMIN_PASSWORD / APP_DOMAIN 等

# 2. 启动（推荐 full；按需叠加 antivirus/search/storage profile）
make up-full                                  # 或
docker compose --profile full --profile search --profile storage --profile antivirus up -d --build

# 3. 验证
curl -sf https://<你的域名>/ready    # {"status":"ready"} 六项检查全 ok
```

首次启动自动执行：数据库迁移（migrate）→ 初始管理员引导（seed，幂等）→ backend 健康后 caddy 放行流量。

要点：

- `.env.production.example` 内含每个 profile 的联动开关与密钥生成指引，模板可用 `scripts/prod-env-check.sh` 校验
- S3（storage profile）需先建 bucket（模板注释中有 mc 命令）；多实例部署必须 `QUEUE_DRIVER=redis` + `STORAGE_DRIVER=s3`
- OIDC 开启时 backend 与 IdP 强耦合：discovery 失败会拒绝启动（自带 restart 自愈）
- 停止：`make down`（含全部 profile；加 `-v` 清数据卷）
- 备份：`make backup` / `make backup-verify`（Windows 用 `backup-windows` 系）

## 本地开发

```bash
# 后端（:8080）：先自备 PostgreSQL 或临时容器
docker run -d --name docflow-pg -p 5432:5432 \
  -e POSTGRES_DB=docflow -e POSTGRES_USER=docflow \
  -e POSTGRES_PASSWORD=change-me postgres:16-alpine
cp .env.example .env   # 本地默认值开箱可用
make run

# 前端（:5173，/api 代理到 :8080）
cd frontend && npm install && npm run dev
```

常用命令：`make test`（单测）、`make e2e`（Playwright 端到端）、`make fmt`、`make migrate`、`make seed`。

## 配置

- 全量键说明：[.env.example](.env.example)（逐键注释）
- 生产推荐值：[.env.production.example](.env.production.example)（profile 联动矩阵）
- API 契约：[docs/openapi.yaml](docs/openapi.yaml)

## 目录结构

```
cmd/            server / migrate / seed / backup-verify / wscheck
internal/       业务模块（auth/files/share/team/upload/search/oidc/...）
migrations/     增量 SQL（按文件名序执行，幂等）
frontend/       React SPA 与 E2E
deploy/         Caddyfile（TLS / 反代 / 子路径规则）
scripts/        备份恢复、集成验证（meili/s3/smtp/clamav/onlyoffice/oidc）、冒烟
docs/           OpenAPI
```

## License

[Apache-2.0](LICENSE)
