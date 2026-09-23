# 配置参考

DocFlow 采用**两级配置模型**：

| 级别 | 载体 | 修改方式 | 生效方式 |
| --- | --- | --- | --- |
| 启动级 | 环境变量（`.env` → docker compose 注入，`internal/config`） | 编辑 `.env` / 编排文件 | **改后需重启** backend（`docker compose up -d` 重建） |
| 运行时 | 数据库表 `system_settings`（`internal/settings`） | 平台管理各面板在线修改 | 多数**即时生效**（消费方每次请求热读取）；个别键标注「须重启」 |

原则与边界：

- **非密钥原则**：密钥类配置（JWT / S3 / OIDC 凭据等）只走环境变量，不入库、不暴露给管理 API。例外：SMTP 密码、AI Provider `api_key`、Tavily Key、MCP `auth_header` 允许经管理端入库，但**只写不读**（留空 = 保持现值，任何读路径均以掩码 `******` 回显）。
- 与 env 重叠的运行时键（如 `upload.max_file_size`、`security.*`）优先取库值，读取失败回退 env 值。
- 全部设置修改均写 `settings.update` 审计（密钥打码）。
- 逐键注释模板见 [.env.example](../.env.example)（本地）与 [.env.production.example](../.env.production.example)（生产联动矩阵）。

---

## 启动级配置（环境变量）

以下为 backend 进程（`internal/config.Load`）读取的全部变量，按功能分组。除特殊说明外：改后**需重启**；布尔值接受 `true/false`；时长接受 Go duration（`15m`、`24h`、`168h`）。

### 应用与网络

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `PORT` | `8080` | backend 监听端口 | compose 中固定 8080，仅内网暴露 |
| `APP_ENV` | `development` | 运行环境标识（透传给 WebSocket 实时组件） | 一般无需调整 |
| `JWT_SECRET` | （必填） | HS256 会话签名密钥 | **≥32 字节**，`openssl rand -hex 32`；安全敏感 |
| `ACCESS_TOKEN_TTL` | `15m` | 访问令牌有效期 | |
| `REFRESH_TOKEN_TTL` | `168h`（7 天） | 刷新令牌 / 会话有效期 | |
| `COOKIE_SECURE` | `true` | 会话 Cookie 仅经 HTTPS 传输 | 本地 HTTP 访问必须显式设 `false`，否则无法保持登录 |
| `COOKIE_DOMAIN` | 空（当前域） | Cookie 域 | 多子域共享会话时填 `.example.com` |
| `TRUSTED_PROXIES` | 空 = 不信任任何代理 | 可信代理 CIDR/IP（逗号分隔）；控制是否采信 `X-Forwarded-For` | 经 caddy/nginx 反代部署须设代理网段（compose 内通常 `172.16.0.0/12`），否则按 IP 限流取直连地址 |
| `ALLOWED_ORIGINS` | 空 | WebSocket 允许的跨域 Origin 列表（逗号分隔） | |
| `CSRF_STRICT` | `true` | refresh/logout 同源严格校验（缺失 Origin/Referer 一律拒绝） | `false` 供 curl 等非浏览器客户端使用 |
| `PUBLIC_BASE_URL` | 空 | 站点对外基地址（如 `https://docflow.example.com`）：邮件邀请/重置链接、分享链接、OIDC 回调的前缀 | 为空时邮件/日志输出相对路径 |
| `CONTENT_PUBLIC_BASE_URL` | 空 | 受控原始内容（`/raw/*`）的对外基地址（跨 origin 内容域场景） | 设置时须为绝对 http(s) URL |
| `METRICS_ENABLED` | `true` | 启用 `GET /metrics`（Prometheus） | 端点无认证且不经 caddy 暴露，仅 compose 内网抓取 |
| `BACKUP_DIR` | 空 | 备份目录（`cmd/backup-verify` 校验工具读取） | 配合 `make backup` / `backup-verify` |

### 数据库与缓存（PostgreSQL / Redis / 后台任务）

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `DATABASE_URL` | （必填） | PostgreSQL 连接串（`postgres://user:pass@host:5432/db?sslmode=disable`） | compose 默认 `postgres://docflow:...@postgres:5432/docflow` |
| `QUEUE_DRIVER` | `inprocess` | 后台任务队列驱动：`inprocess`（进程内 goroutine，零依赖）\| `redis`（asynq，多实例横向扩展） | **多实例部署必须 `redis`**；redis 同时承载 WebSocket 跨实例通知广播 |
| `REDIS_ADDR` | `localhost:6379` | Redis 地址（`QUEUE_DRIVER=redis` 时使用；compose 内为 `redis:6379`） | redis 驱动下启动 ping 失败即退出 |
| `REDIS_PASSWORD` | 空 | Redis 密码 | 无认证留空 |
| `QUEUE_CONCURRENCY` | `5` | redis 驱动下每实例并行处理任务数 | ≥1 |
| `JANITOR_ENABLED` | `true` | 启用后台清理任务（janitor：过期上传会话、孤儿 blob、回收站、审计/访问事件） | 兼容旧拼写 `JANIOR_ENABLED` 作为别名 |
| `JANITOR_INTERVAL` | `10m` | janitor 清理周期（启动即先跑一轮） | |

### 存储与上传（本地 / MinIO·S3 / 网页包）

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `STORAGE_DRIVER` | `local` | 存储驱动：`local` \| `s3` | 多实例部署必须 `s3`（local 为单机实现） |
| `STORAGE_ROOT` | `./storage` | local 驱动的数据目录 | compose 中固定 `/data/storage`（`backend_storage` 卷） |
| `S3_ENDPOINT` | 空 = AWS 默认端点 | S3 兼容端点 | 自建 MinIO 用 `http://minio:9000`；`STORAGE_DRIVER=s3` 时建议显式配置 |
| `S3_BUCKET` | 空 | 存储桶名 | `STORAGE_DRIVER=s3` 时**必填**，需提前创建 |
| `S3_REGION` | `us-east-1` | 区域 | |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | 空 | 访问凭据 | 安全敏感；compose 中同时作为 MinIO 的 root 账号注入 |
| `S3_PATH_STYLE` | `true` | 路径风格寻址 | 自建服务（MinIO 等）保持 `true` |
| `MAX_FILE_SIZE` | `2147483648`（2GiB） | 单文件上传大小上限（字节，启动回退值） | 运行时优先读 `upload.max_file_size`（默认 1GiB，热生效） |
| `PATCH_MAX_BYTES` | `67108864`（64MiB） | 单次上传 PATCH 请求体上限 | 约束单请求存储写入量与连接占用时长 |
| `UPLOAD_SESSION_TTL` | `24h` | 上传会话有效期 | |
| `MAX_VERSIONS_PER_FILE` | `5` | 每文件版本数上限（启动回退值） | 运行时优先读 `upload.max_versions_per_file`（热生效） |
| `WEBPKG_ENABLED` | `true` | 网页包（zip）上传后自动解包预览 | 关闭后仍可经 `POST /api/v1/files/:id/webpkg/extract` 手动解包 |
| `WEBPKG_MAX_ENTRIES` | `500` | 解包条目数上限 | |
| `WEBPKG_MAX_FILE_SIZE` | `33554432`（32MiB） | 解包单文件展开大小上限 | |
| `WEBPKG_MAX_TOTAL_SIZE` | `268435456`（256MiB） | 解包展开总大小上限 | 须 ≥ 单文件上限 |
| `WEBPKG_MAX_DEPTH` | `10` | 解包目录深度上限 | |
| `WEBPKG_RATE_LIMIT_PER_MIN` | `120` | `/content` 内容端点独立按 IP 轻限流（次/分钟） | |

### 全文与向量检索（Meilisearch / Qdrant）

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `SEARCH_DRIVER` | `pg` | 全文检索引擎：`pg`（PostgreSQL ILIKE+tsvector）\| `meili` | meili 时需 `--profile search` 启动 meilisearch 服务 |
| `MEILI_URL` | 空 | Meilisearch 基地址（compose 内 `http://meilisearch:7700`） | `SEARCH_DRIVER=meili` 时**必填**；启动 EnsureIndex 失败即退出 |
| `MEILI_API_KEY` | 空 | Meilisearch API Key | 与实例 `MEILI_MASTER_KEY` 同值；无认证本地实例可留空 |
| `AI_RAG_MODE` | `keyword` | RAG 检索模式引导值：`keyword` \| `hybrid` | 运行时可被 `ai.rag.mode` 覆盖（AI 设置面板，热生效） |
| `AI_RAG_VECTOR_ENABLED` | `false` | 启用向量 RAG（hybrid 模式） | 运行时键 `ai.rag.vector_enabled`；需 `--profile ai-vector` 的 Qdrant |
| `AI_RAG_QDRANT_URL` | `http://qdrant:6333` | Qdrant 地址 | 运行时键 `ai.rag.qdrant_url`（改后需重启装配） |
| `AI_RAG_COLLECTION_PREFIX` | `docflow_` | Qdrant collection 前缀 | 运行时键 `ai.rag.collection_prefix` |
| `AI_RAG_EMBEDDING_PROVIDER` | `mock` | embedding Provider（Provider ID 或 `mock`） | 运行时键 `ai.rag.embedding_provider` |
| `AI_RAG_EMBEDDING_MODEL` | `text-embedding-3-small` | embedding 模型 | 运行时键 `ai.rag.embedding_model` |
| `AI_RAG_TOP_K` | `8` | 向量检索召回条数（1–100） | 运行时键 `ai.rag.top_k`（热生效） |
| `AI_RAG_CHUNK_SIZE` | `1000` | 分块大小（≥100） | 运行时键 `ai.rag.chunk_size` |
| `AI_RAG_CHUNK_OVERLAP` | `100` | 分块重叠（≥0 且 < chunk_size） | 运行时键 `ai.rag.chunk_overlap` |

联网搜索（SearXNG / Tavily）没有启动级变量，全部为运行时配置，见 [AI 设置 → 联网搜索](#ai-设置平台管理--ai-设置)。

### Office 与图表（OnlyOffice / draw.io）

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `ONLYOFFICE_ENABLED` | `false` | 启用 OnlyOffice Document Server 集成 | 未启用时不注册 session/download/callback 路由（404） |
| `ONLYOFFICE_SERVER_URL` | `http://onlyoffice:80` | DocumentServer **内网**基地址；同时是回调下载 URL 防 SSRF 校验的同源基准 | 启用时须为绝对 http(s) URL |
| `ONLYOFFICE_PUBLIC_URL` | 空（回退 SERVER_URL） | **浏览器可达**的 DocumentServer 地址（`/onlyoffice/config` 返回给前端加载 api.js） | 生产经 caddy 反代时设 `https://<对外域名>/onlyoffice`；不影响 SSRF 基准 |
| `ONLYOFFICE_JWT_SECRET` | 空 | 与 DocumentServer 共享的 JWT 签名密钥（HS256） | 启用时**必填且 ≥32 字节**，与 onlyoffice 服务 `JWT_SECRET` 一致；安全敏感 |
| `ONLYOFFICE_DOWNLOAD_URL_BASE` | `http://backend:8080` | DocumentServer 回源访问后端的基地址（document.url / callbackUrl 前缀） | 仅 Docker 内网 |
| `ONLYOFFICE_RATE_LIMIT_PER_MIN` | `60` | onlyoffice 公开组（download/callback）独立按 IP 限流 | |
| `DRAWIO_ENABLED` | `false` | 启用 draw.io 图表编辑（浏览器侧 iframe，后端不与 drawio 服务通信） | 静态层已并入 caddy 镜像（`/drawio/*`） |
| `DRAWIO_SERVER_URL` | `http://drawio:8080` | drawio 内网回退地址 | 浏览器通常不可达，仅作 PUBLIC_URL 未设置时的回退 |
| `DRAWIO_PUBLIC_URL` | 空（回退 SERVER_URL） | 浏览器可达的 drawio 地址 | 生产设 `https://<对外域名>/drawio` |

### AI Provider 引导配置

> 这组 env 是管理端 **AI 设置面板未配置时的回退基线**；在「平台管理 → AI 设置」保存过 Provider 后，运行时以库中 `ai.*` 配置为准（见下文）。密钥只走环境变量或管理端只写入库。

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `AI_ENABLED` | `false` | 启用 AI 集成（文件摘要等） | 未启用时 `POST /files/:id/ai/summary` 返回 503 |
| `AI_BASE_URL` | `https://api.openai.com/v1` | OpenAI 兼容服务基地址 | 可指向任意兼容网关（OneAPI / vLLM / DeepSeek 等）；须为绝对 http(s) URL |
| `AI_API_KEY` | 空 | 上游 API Key | `AI_ENABLED=true` 时**必填**；安全敏感 |
| `AI_MODEL` | `gpt-4o-mini` | 默认摘要模型 | |

### Agent（Docker 沙箱创作舱）

Agent 的行为开关（镜像白名单、资源上限、AI 调用等）全部为**运行时键**（`agent.*`，见 [AI 智能体](#ai-智能体平台管理--ai-创作舱)）；启动级仅一个变量：

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `DOCFLOW_AGENT_IPC_DIR` | 空 | backend 与 Agent 容器共享的 **IPC socket 目录**（宿主路径，需以共享卷方式同时挂给 backend 与任务容器） | `agent.allow_ai=true` 时容器经该目录下的 Unix socket 调用平台 AI（容器保持断网 `network_mode=none`）；（新增键） |

### 安全与防爆破（限流 / 锁定 / 病毒扫描）

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `RATE_LIMIT_PER_MIN` | `120` | `/api/v1` 认证接口基础限流（每分钟；0 = 禁用） | 启动时装配；同名运行时键 `security.rate_limit_per_minute` 当前亦须重启生效 |
| `LOGIN_RATE_LIMIT_PER_MIN` | `10` | 登录接口单独限流（每分钟） | |
| `PUBLIC_RATE_LIMIT_PER_MIN` | `60` | 公开分享接口单独按 IP 限流 | |
| `LOGIN_MAX_RETRIES` | `5` | 连续登录失败锁定阈值（≥1） | 达到后锁定账号 |
| `LOGIN_LOCK_MINUTES` | `15` | 登录失败锁定时长（分钟，≥1） | |
| `ACCESS_SALT` | 由 `JWT_SECRET` 派生 | 分享访问事件 IP 哈希静态盐：`ip_hash = SHA-256(salt‖ip)`，明文 IP 不落库 | 一般无需单独设置 |
| `RAW_URL_SECRET` | 由 `JWT_SECRET` 经 HKDF 派生 | `/raw/*` 短期授权（HMAC grant，10 分钟）签名密钥源 | 显式设置时须 ≥32 字节 |
| `SCAN_ENABLED` | `false` | 启用上传病毒扫描（clamd INSTREAM） | 需 `--profile antivirus` 启动 clamav |
| `CLAMAV_ADDR` | 空 | clamd 地址 `host:port` | compose 内为 `clamav:3310`（容器内 localhost 不可达） |
| `CLAMAV_TIMEOUT` | `5m` | 单次扫描超时 | |
| `CLAMAV_REQUIRED` | `true` | `true` = clamd 不可达即拒绝上传（fail closed）；`false` = 降级放行并记警告 | 首次启动 clamav 需数分钟下载病毒库 |

### 邮件（SMTP，启动基线）

> 邮件通道支持**运行时覆盖**：管理端「平台管理 → 邮件」保存的 `smtp.*` 入库值优先于下列 env 基线，改后即时生效（见 [邮件（运行时）](#邮件平台管理--邮件)）。未启用时使用 Noop 通道：不联网，邀请/重置链接仅输出到后端日志（`[mail:noop]` 前缀）。

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `SMTP_ENABLED` | `false` | 启用 SMTP 投递 | |
| `SMTP_HOST` | 空 | SMTP 服务器主机 | 启用时**必填** |
| `SMTP_PORT` | `587` | SMTP 端口（1–65535） | 465 隐式 TLS 建议改用运行时 `smtp.tls_mode=ssl` |
| `SMTP_USER` / `SMTP_PASS` | 空 | 认证账号 / 密码（空 = 匿名投递） | SMTP_PASS 安全敏感 |
| `SMTP_FROM` | 空 | 发件人地址 | 启用时**必填** |

### TLS 与 HTTPS 运行时切换

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `CADDY_ADMIN_ADDR` | 空 = 功能关闭 | Caddy admin API 地址（如 `caddy:2019`）：管理页 TLS 热切换经其 `POST /load` 下发配置 | compose 已默认注入 `caddy:2019`（开箱可用）；显式置空关闭该功能 |
| `TLS_CERT_DIR` | `/data/tls` | 自定义证书（custom 模式）落盘目录（0600） | compose 中与 caddy 共享 `tls_certs` 卷（backend 可写 / caddy 只读） |

### 单点登录（OIDC）

| 变量 | 默认值 | 说明 | 备注 |
| --- | --- | --- | --- |
| `OIDC_ENABLED` | `false` | 启用 OIDC 单点登录（授权码 + PKCE S256） | 未启用时 login/callback 路由 404 |
| `OIDC_ISSUER` | 空 | IdP 签发方基地址（如 `https://accounts.google.com`） | 启用时**必填**；启动拉取 discovery 失败即拒绝启动 |
| `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` | 空 | IdP 侧注册的应用凭据 | 启用时**必填**；SECRET 安全敏感 |
| `OIDC_REDIRECT_URL` | `{PUBLIC_BASE_URL}/api/v1/auth/oidc/callback` | 授权码回调地址 | 显式设置时须为绝对 http(s) URL；与 PUBLIC_BASE_URL 均未设置时启动校验失败 |
| `OIDC_AUTO_PROVISION` | `true` | 自动开户：IdP 身份未关联且邮箱无匹配时自动创建 `role=user` 账号 | `false` 时无匹配一律 403 |

### 部署编排变量（compose 构建与伴生服务，backend 不读取）

| 变量 | 使用方 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `DOCFLOW_IMAGE_TAG` | compose 镜像标签 | `1.0.0` | backend / migrate / seed / caddy 共用 |
| `NPM_REGISTRY` / `GOPROXY` | 镜像构建 | 官方源 | 国内网络构建加速 |
| `POSTGRES_DB` / `POSTGRES_USER` / `POSTGRES_PASSWORD` | postgres 服务 | `docflow` / `docflow` / `change-me` | 密码须与 `DATABASE_URL` 一致 |
| `SEED_ADMIN_EMAIL` / `SEED_ADMIN_USERNAME` / `SEED_ADMIN_PASSWORD` / `SEED_ADMIN_ROLE` | seed 服务（幂等） | `admin@example.com` / `admin` / **必填** / `admin` | 初始管理员引导；密码 ≥12 字符含大小写与数字，首次登录后立即改密 |
| `APP_DOMAIN` | caddy 站点地址 | `:80`（无 TLS） | 设为对外域名后自动签发 HTTPS（ACME） |
| `CONTENT_DOMAIN` / `CONTENT_UPSTREAM` | caddy 独立内容 origin | `content.localhost` / `backend:8080` | 网页包预览 `/content` 域；生产建议独立域名 |
| `ONLYOFFICE_UPSTREAM` | caddy `/onlyoffice/*` 反代上游 | `127.0.0.1:9`（discard，minimal 下 502 属预期） | `make up-full` 自动注入 `onlyoffice:80` |
| `CADDY_ADMIN` | caddy admin 监听 | `:2019` | 仅 compose 内网 expose，不映射宿主端口 |
| `CADDY_TLS_CERT` / `CADDY_TLS_KEY` | caddy custom 模式证书路径 | `/data/tls/cert.pem` / `/data/tls/key.pem` | 热下发 `tls` 指令使用 |
| `MEILI_MASTER_KEY` | meilisearch 服务 | 空 = 无认证实例 | 设 ≥16 字节随机串并同步 `MEILI_API_KEY` |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | minio 服务 | 取 `S3_ACCESS_KEY` / `S3_SECRET_KEY` | compose 注入联动 |

---

## 运行时配置（平台管理 → 各面板）

运行时配置存储于 `system_settings` 表，分三类入口：

1. **通用键**（`GET/PUT /api/v1/admin/settings`）：「平台管理 → 系统设置」面板展示与修改；
2. **专用整块键**（专用端点，不进通用列表）：AI 设置（`/admin/settings/ai` 及子端点）、SMTP（`/admin/settings/smtp`）、Agent 面板（逐键写 `agent.*`）；
3. 生效方式（Effect）：`immediate` = 即时生效；`restart` = 须重启（启动装配读取）；`new_session` = 新会话生效。

### AI 设置（平台管理 → AI 设置）

专用端点整块读写（`ai.*`），对话/摘要链路**每次热读取**。Provider 的 `api_key`、Tavily Key 只写不读（留空 = 保持现值，回显掩码）。

| 键 | 默认值 | 生效 | 说明 |
| --- | --- | --- | --- |
| `ai.enabled` | 自动（未设置） | 即时 | AI 总开关：显式 `false` 时全站隐藏 AI 入口、网关 404；未设置 = 存在启用中的 Provider 即视为开启 |
| `ai.providers` | `[]` | 即时 | Provider 列表（JSON 数组整体读替）：`id/name/kind/base_url/api_key/models[]/enabled` + Provider 级限流 `requests_per_min`（0–10000）、`daily_quota`（0–1000000）。kind：`openai_compatible`（OpenAI/DeepSeek/Qwen/Ollama/vLLM 等兼容）\| `anthropic` \| `mock`（开发测试） |
| `ai.providers[].models[].capabilities` | — | 即时 | 模型能力勾选：`chat` / `embedding` / `vision`（视觉图片，供 OCR）/ `rerank` / `reasoning`（对话「思考」开关仅对勾选模型生效） |
| `ai.default_provider` | 空（首个启用项） | 即时 | 默认 Provider ID |
| `ai.default_models` | 空 | 即时 | 场景默认模型 `{provider_id, model_id}`，键为 `chat` / `summary` / `edit` / `embedding`；summary/edit 未配置或无效时回落 chat |
| `ai.temperature` | `0.3` | 即时 | 采样温度（0–2） |
| `ai.max_tokens` | `2048` | 即时 | 单次补全 max_tokens（1–128000） |
| `ai.per_user_per_min` | `20` | 即时 | 全局每用户每分钟 AI 请求上限（0–10000，0 = 用 Provider 级或全局兜底） |
| `ai.rag.mode` | `keyword` | 即时 | RAG 模式：`keyword` \| `hybrid`（关键词+向量） |
| `ai.rag.vector_enabled` | `false` | 即时 | 启用向量检索（hybrid 下生效；需 Qdrant） |
| `ai.rag.qdrant_url` | `http://qdrant:6333` | 重启 | Qdrant 地址 |
| `ai.rag.collection_prefix` | `docflow_` | 重启 | collection 前缀（按 provider+模型派生，切换 embedding 后需「重建向量索引」） |
| `ai.rag.embedding_provider` | `mock` | 重启 | embedding Provider（Provider ID；旧值 `openai_compatible` 按模型名直连默认 Provider） |
| `ai.rag.embedding_model` | `text-embedding-3-small` | 重启 | embedding 模型（须勾选 embedding 能力） |
| `ai.rag.rerank_provider` / `ai.rag.rerank_model` | 空 = 不重排 | 即时 | 重排序目标（Cohere / Jina 兼容 `/rerank` 协议；两者须成对配置，失败静默保持原序） |
| `ai.rag.top_k` | `8` | 即时 | 向量召回条数（面板保存按 1–10 校验；通用设置端点允许 1–100） |
| `ai.rag.chunk_size` | `1000` | 重启 | 分块大小（100–10000） |
| `ai.rag.chunk_overlap` | `100` | 重启 | 分块重叠（0–5000 且 < chunk_size） |
| `ai.search.provider` | 空 = 禁用 | 即时 | 联网搜索后端：`searxng` \| `tavily` |
| `ai.search.searxng_url` | 空 | 即时 | SearXNG 基地址（须开启 JSON API；compose `--profile ai-search` 内为 `http://searxng:8080`） |
| `ai.search.tavily_api_key` | 空 | 即时 | Tavily API Key（provider=tavily 时必填；只写不读） |
| `ai.search.max_results` | `5` | 即时 | 每次查询召回条数（1–10） |
| `ai.ocr` | 禁用 | 即时 | 图片 OCR（整体 JSON 块）：`enabled/provider_id/model_id/max_image_bytes`（默认 8MiB，硬顶 32MiB）。开启后索引管道调「视觉图片」能力模型提取图片文字入全文+向量索引 |
| `ai.personas` | `[]` | 即时 | 平台人设模板（≤50 条）：`id/name/system_prompt`（≤4000 字符），全员对话可选 |
| `ai.skills` | `[]` | 即时 | 平台技能/快捷指令模板（≤50 条）：`id/name/description(≤200)/prompt(≤4000)`，prompt 支持 `{selection}`（编辑器选区）/ `{file}`（当前文件名）占位符 |
| `ai.mcp` | `[]` | 即时 | 外部 MCP 服务器（≤8 条，Streamable HTTP）：`id/name/url/auth_header/enabled`；auth_header 只写不读；对话开启「MCP 工具」后聚合调用（单次对话 ≤5 轮） |

另见 [docs/mcp.md](mcp.md)（DocFlow 自身作为 MCP Server 对外暴露文档工具）。

### AI 智能体（平台管理 → AI 创作舱）

`agent.*` 键（通用端点可写、创作舱面板统一维护）。任务链路：目录快照 → 受限 Docker workspace → Diff 评审 → 用户确认写回。默认镜像 `alpine:3.20` 占位，自建镜像见[修改示例](#修改示例常见任务)。

| 键 | 默认值 | 生效 | 说明 |
| --- | --- | --- | --- |
| `agent.enabled` | `true` | 即时 | 创作舱总开关（AI 总开关关闭时 Agent 一并不可用） |
| `agent.runtime` | `docker` | 重启 | 运行时（当前仅 `docker`；`fake` 为开发干跑） |
| `agent.allowed_images` | 空（默认 `alpine:3.20`） | 即时 | 允许的镜像白名单（逗号分隔）；配置非空时整体覆盖默认镜像，**首个镜像**即未指定时的任务默认 |
| `agent.max_concurrent` | `1`（1–100） | 即时 | 最大并发任务数 |
| `agent.default_timeout_seconds` | `900`（1–86400） | 即时 | 任务默认超时（秒） |
| `agent.max_cpu` | `1`（1–64） | 即时 | 每容器 CPU 上限 |
| `agent.max_memory_bytes` | `536870912`（512MiB） | 即时 | 每容器内存上限（1MiB–1TiB） |
| `agent.network_mode` | `none` | 即时 | 容器网络模式：`none`（断网，默认）\| `restricted` |
| `agent.mcp_callback_base_url` | 空 | 即时 | 受限 MCP 回调基地址（不含凭据） |
| `agent.allow_ai` | （新增） | 即时 | 允许容器经 IPC socket 调用平台 AI：backend 与容器共享 `DOCFLOW_AGENT_IPC_DIR` 目录下的 Unix socket，容器保持断网（`network_mode=none`）也能使用平台 AI 能力 |
| `agent.ai_max_calls` | `40`（新增） | 即时 | 每任务 AI 调用次数上限，超出后任务内 AI 调用被拒绝 |
| `agent.sync_mode` | （新增） | 即时 | 产物同步方式：`git`（以 git 变更识别产物，按 `.gitignore` 过滤）\| `scan`（全量扫描工作区 + 内置忽略规则，如 `node_modules` 等） |

### 系统设置（平台管理 → 系统设置）

通用键全集（`GET /api/v1/admin/settings` 输出顺序）；`ai.rag.*`、`ai.search.*` 归 AI 设置面板维护、`agent.*` 归创作舱面板维护，此处一并列出以便检索。除标注「须重启」外均即时生效。

**上传与版本（upload.\*）**

| 键 | 默认值 | 范围 | 生效 | 说明 |
| --- | --- | --- | --- | --- |
| `upload.max_versions_per_file` | `5` | 1–1000 | 即时 | 每文件保留版本数上限（覆盖上传后按版本号裁剪；与时间窗组合生效） |
| `upload.version_retention_days` | `0` | 0–3650 | 即时 | 版本保留时间窗（天）：窗口内的版本不因数量裁剪删除；0 = 不启用（仅按数量） |
| `upload.blocked_extensions` | 空 | — | 即时 | 上传扩展名黑名单（逗号分隔，如 `exe,bat,sh`；不含点、大小写不敏感），建会话与完成时双侧拒绝（400） |
| `upload.max_file_size` | `1073741824`（1GiB） | 1B–1TiB | 即时 | 单文件上传大小上限（字节） |
| `upload.default_quota` | `10737418240`（10GiB） | 1B–1PiB | 即时 | 新用户开户默认存储配额；仅对新用户生效，存量用户经管理端单独调整 |
| `upload.max_concurrent_uploads_per_user` | `3` | 1–100 | 即时 | 每用户并发上传会话上限（非终态会话达到上限新建返回 429） |

**分享（share.\*）**

| 键 | 默认值 | 范围 | 生效 | 说明 |
| --- | --- | --- | --- | --- |
| `share.default_expiry_hours` | `168`（7 天） | 1–8760 | 即时 | 公开分享默认有效期（小时） |
| `share.default_watermark` | `true` | — | 即时 | 新分享默认启用水印（创建请求未显式指定时采用） |
| `share.watermark_text` | `{date} {name}` | — | 即时 | 水印默认模板，支持 `{email}/{date}/{name}` 占位符（公开访问无登录身份时 `{email}` 渲染为脱敏 IP 前缀） |
| `share.public_enabled` | `true` | — | 即时 | 是否允许创建公开分享 |

**保留与清理（retention.\* / audit.\*）**

| 键 | 默认值 | 范围 | 生效 | 说明 |
| --- | --- | --- | --- | --- |
| `retention.trash_days` | `30` | 1–3650 | 即时 | 回收站保留天数：软删除超期后由 janitor 彻底删除 |
| `retention.access_events_days` | `90` | 1–3650 | 即时 | 文件访问事件保留天数 |
| `audit.retention_days` | `90` | 0–3650 | 即时 | 审计日志保留天数（0 = 永久保留），janitor 每日清理 |

**安全（security.\*）**

| 键 | 默认值 | 范围 | 生效 | 说明 |
| --- | --- | --- | --- | --- |
| `security.rate_limit_per_minute` | `120` | 0–100000 | **须重启** | 认证 API 每分钟请求上限（限流器启动时按 env 装配，当前无热读取消费方） |
| `security.login_max_retries` | `5` | 1–100 | **须重启** | 连续登录失败锁定阈值（由 env `LOGIN_MAX_RETRIES` 启动时注入） |
| `security.login_lock_minutes` | `15` | 1–10080 | **须重启** | 登录失败锁定时长（分钟；由 env `LOGIN_LOCK_MINUTES` 启动时注入） |
| `security.scan_quarantine_policy` | `quarantine` | — | 即时 | 扫描失败处理策略：`quarantine` \| `reject`（当前版本未接线：失败一律隔离，隔离区经管理端处置） |

**批量与目录**

| 键 | 默认值 | 范围 | 生效 | 说明 |
| --- | --- | --- | --- | --- |
| `batch.max_items` | `100` | 1–1000 | 即时 | 批量操作单次最大项目数 |
| `folder.max_depth` | `32` | 1–1000 | 即时 | 目录最大深度（根为 1），创建/移动超限拒绝 |

**备份（backup.\*）**

| 键 | 默认值 | 生效 | 说明 |
| --- | --- | --- | --- |
| `backup.enabled` | `false` | **须重启** | 启用备份任务 |
| `backup.retention_days` | `30` | **须重启** | 备份保留天数 |
| `backup.encryption_required` | `true` | **须重启** | 是否要求备份加密 |
| `backup.last_verify` | 空 | **须重启** | 最近一次备份验证时间（记录项） |

**空间（space.\*，统一空间模型）**

| 键 | 默认值 | 范围 | 生效 | 说明 |
| --- | --- | --- | --- | --- |
| `space.default_quota` | `10737418240`（10GiB） | 0–1PiB | 即时 | 新空间默认存储配额（0 = 不限）；新建与注册默认空间的初始配额 |
| `space.max_quota` | `1099511627776`（1TiB） | 0–1PiB | 即时 | 空间配额上限（0 = 不限）：owner/admin 调整配额不得超过，系统 admin 不受限 |
| `space.max_per_user` | `20` | 1–1000 | 即时 | 每用户空间数上限（owner 维度计数，含默认空间），超出创建返回 413 |

**功能开关（webdav / collab）**

| 键 | 默认值 | 生效 | 说明 |
| --- | --- | --- | --- |
| `webdav.enabled` | `false` | 即时 | 启用 WebDAV 文件访问（`/webdav`，一次性令牌 Basic Auth） |
| `collab.enabled` | `true` | 即时 | 启用富文本实时协作（`/api/v1/collab/{fileId}/ws`；关闭时端点 404 且不创建房间） |

**AI 检索/搜索与 Agent 键（在通用设置列表中可见，面板归属见前两节）**：`ai.rag.mode`、`ai.rag.vector_enabled`、`ai.rag.qdrant_url`、`ai.rag.collection_prefix`、`ai.rag.embedding_provider`、`ai.rag.embedding_model`、`ai.rag.top_k`、`ai.rag.chunk_size`、`ai.rag.chunk_overlap`、`ai.search.provider`、`ai.search.searxng_url`、`ai.search.max_results`、`agent.enabled`、`agent.runtime`、`agent.allowed_images`、`agent.max_concurrent`、`agent.default_timeout_seconds`、`agent.max_cpu`、`agent.max_memory_bytes`、`agent.network_mode`、`agent.mcp_callback_base_url`（含义与默认值见前两节表格）。

### 邮件（平台管理 → 邮件）

SMTP 为「env 基线 + 入库覆盖」双层模型：每次发送时热读取生效配置（库值覆盖 env），**改后即时生效、无需重启**。密码只写不读（留空 = 保持现值，任何读路径不回显）。启用时 host/from 必填、port 1–65535。

| 键 | 默认值（= env 基线） | 说明 |
| --- | --- | --- |
| `smtp.enabled` | `false` | 启用 SMTP 投递；false 走 Noop（仅日志输出链接） |
| `smtp.host` | 空 | SMTP 服务器主机 |
| `smtp.port` | `587` | 端口（1–65535） |
| `smtp.user` / `smtp.pass` | 空 | 认证账号 / 密码（user 空 = 匿名投递；pass 只写不读） |
| `smtp.from` | 空 | 发件人地址 |
| `smtp.tls_mode` | `auto` | TLS 模式：`auto`（STARTTLS 自动协商，默认）\| `ssl`（隐式 TLS/SMTPS，465 常用）\| `none`（不协商，仅可信内网中继） |

邮件内链接前缀取 env `PUBLIC_BASE_URL`（env-only，为空时输出相对路径）。

### TLS（平台管理 → TLS）

HTTPS 模式**运行时热切换**（经 `CADDY_ADMIN_ADDR` 指向的 Caddy admin API 下发配置，无需重启）：

| 模式 | 说明 |
| --- | --- |
| `auto` | 自动证书（ACME，公网域名场景推荐） |
| `internal` | Caddy 自签证书（内网/临时） |
| `custom` | 管理页上传 PEM 证书+私钥，落盘 `TLS_CERT_DIR`（0600），caddy 经 `CADDY_TLS_CERT/KEY` 读取 |

`CADDY_ADMIN_ADDR` 为空（或 caddy 不可达）时该功能关闭，TLS 完全由部署配置（`APP_DOMAIN`）决定。

---

## Docker Compose profiles 与服务矩阵

服务组合、叠加方式与联动开关见 [README「架构与服务矩阵」](../README.md#架构与服务矩阵)：`minimal`（caddy+backend+postgres）/ `full`（+redis+onlyoffice）/ `antivirus`（+clamav）/ `search`（+meilisearch）/ `storage`（+minio）/ `ai-vector`（+qdrant）/ `ai-search`（+searxng）。启用可选服务后须按上表联动对应 env 或运行时键（如 `SCAN_ENABLED=true`、`SEARCH_DRIVER=meili`、`ai.rag.vector_enabled=true`）。

## 配置总览页（平台管理 → 配置总览）

平台管理提供**配置总览**视图，一页看清两级配置的当前状态：

- **启动级（env）只读展示**：按分组列出当前生效的环境变量（密钥类仅显示「已配置」状态，绝不回显值）；
- **运行时键索引**：`system_settings` 全部键的当前值/默认值/生效方式（即时/须重启），跳转对应面板修改；
- **AI 能力状态**：AI 总开关、Provider/模型可用性、RAG（关键词/混合+向量）、联网搜索、OCR、MCP 等能力的开闭状态（对应 `GET /api/v1/ai/status` 能力探测）。

排查「配置到底改没改、生没生效」时优先看这一页。

## 修改示例（常见任务）

### 1. 更换对外域名

```bash
# .env
APP_DOMAIN=new.example.com          # caddy 站点地址，自动签发证书
PUBLIC_BASE_URL=https://new.example.com   # 邮件/分享/OIDC 回调链接前缀
CONTENT_DOMAIN=content.new.example.com    # 可选：独立内容域
TRUSTED_PROXIES=172.16.0.0/12
```

`docker compose --profile full up -d` 重建；已启用 OIDC 的需在 IdP 侧同步新回调地址。运行时无涉（无需改 system_settings）。

### 2. 接入自有 PostgreSQL

```bash
# .env —— 指向外部实例，compose 的 postgres/migrate/seed 仍用同一连接串
DATABASE_URL=postgres://docflow:密码@db.internal:5432/docflow?sslmode=disable
```

仅需 `migrate`/`seed` 服务能访问该库（首次启动自动迁移与引导管理员）；如完全弃用内置 postgres 服务，可用 `docker compose run --rm migrate` / `seed` 单独执行。改后重启。

### 3. 开启混合检索（关键词 + 向量）

```bash
# 1) 启动 Qdrant（ai-vector profile，可与 full 叠加）
docker compose --profile full --profile ai-vector up -d

# 2) 平台管理 → AI 设置（运行时，即时生效）：
#    - 先配置至少一个 Provider 并在模型上勾选「向量（embedding）」能力
#    - RAG：模式 = hybrid、启用向量检索 = 开
#    - embedding Provider/模型 选上述勾选向量能力的模型
#    - 保存后点「重建向量索引」（collection 按 provider+模型派生）
```

Qdrant 地址默认 `http://qdrant:6333`（compose 内网），如外部实例改 `ai.rag.qdrant_url`（须重启装配）。

### 4. 配置 OnlyOffice 公网地址（full profile）

```bash
# .env
ONLYOFFICE_ENABLED=true
ONLYOFFICE_SERVER_URL=http://onlyoffice:80                 # 内网 SSRF 基准，保持不变
ONLYOFFICE_PUBLIC_URL=https://docflow.example.com/onlyoffice  # 浏览器经 caddy 反代的地址
ONLYOFFICE_JWT_SECRET=<openssl rand -hex 32>               # 与 onlyoffice 服务共享
ONLYOFFICE_DOWNLOAD_URL_BASE=http://backend:8080           # DS 回源后端
ONLYOFFICE_UPSTREAM=onlyoffice:80                          # make up-full 自动注入
```

`make up-full` 后浏览器经 `https://<域名>/onlyoffice` 加载编辑器；本地联调可用 `docker run -p 8081:80` 直连 DS 并把两个 URL 均指向 `http://localhost:8081`。

### 5. 构建并启用自建 Agent 镜像

```bash
# 1) 构建含工具链的镜像（示例：git + node + python）
cat > Dockerfile.agent <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache git nodejs npm python3 py3-pip
EOF
docker build -f Dockerfile.agent -t registry.example.com/docflow-agent:1.0.0 .
docker push registry.example.com/docflow-agent:1.0.0

# 2) 平台管理 → AI 创作舱（运行时，即时生效）：
#    agent.allowed_images = registry.example.com/docflow-agent:1.0.0,alpine:3.20
#    （逗号分隔白名单；首个镜像为任务未指定时的默认）
#    按需调整 max_cpu / max_memory_bytes / default_timeout_seconds
```

Agent 容器默认断网（`agent.network_mode=none`）；如需容器内调用平台 AI，开启 `agent.allow_ai` 并以共享卷挂载 `DOCFLOW_AGENT_IPC_DIR` 目录（IPC socket）。仓库不预置 agent 专用镜像，默认白名单仅 `alpine:3.20` 占位。

---

- 配置来源代码索引：`internal/config/config.go`（env）、`internal/settings/settings.go`（运行时键定义与校验）、`docker-compose.yml` + `deploy/Caddyfile`（编排与入口）、`.env.example` / `.env.production.example`（模板）。
- API 契约：[docs/openapi.yaml](openapi.yaml)；MCP 服务端说明：[docs/mcp.md](mcp.md)。
