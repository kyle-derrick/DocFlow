# DocFlow 架构总览

面向希望理解 DocFlow 内部结构的开发者与运维者。本文描述**当前代码实现**（以仓库为准），包含模块划分、数据模型、关键链路、部署拓扑，以及若干**有意为之的设计取舍与已知局限**。

配套阅读：

- 部署与配置：[README「配置」](../README.md#配置)、[configuration.md](configuration.md)
- 场景化 `.env` 模板：[deploy/env/](../deploy/env/)
- API 契约：[openapi.yaml](openapi.yaml)
- MCP 能力：[mcp.md](mcp.md)
- 版本变更：[CHANGELOG.md](../CHANGELOG.md)

---

## 1. 总体形态

自托管的企业文档与文件管理平台。四个反直觉但关键的形态特征：

1. **前端没有独立容器**。React SPA 在构建期把产物打进 caddy 镜像，与 draw.io 静态层一起由 Caddy 直接托管。
2. **Caddy 是唯一映射宿主端口的服务**（80/443）。backend / postgres / redis / onlyoffice / clamav 等一律只走 compose 内网。
3. **内容与站点分离**。文件原始内容经 `/content/*`、`/raw/*` 在"唯一化 origin 沙箱"中提供，与主站会话彻底解耦。
4. **配置分两级**：启动级环境变量（改后须重启）+ 运行时 `system_settings`（多数热生效）。

技术栈：

| 层 | 选型 |
| --- | --- |
| 后端 | Go 1.22 + Gin + GORM，PostgreSQL 16 |
| 迁移 | 手写增量 SQL（`migrations/*.sql`），无 ORM 自动迁移 |
| 前端 | React 18 + TypeScript + Vite（PWA） |
| 入口 | Caddy 2.8.4（TLS 终止 + 静态托管 + 全部反代） |
| 队列 | inprocess（默认）或 Redis + asynq |
| 存储 | local（默认）或 S3 兼容（MinIO / AWS S3） |

---

## 2. 系统架构

### 2.1 部署拓扑

```
                         ┌─────────────────────────────────────────┐
   公网 :80/:443 ───────▶│  caddy（唯一宿主端口映射）               │
                         │  · TLS 终止（自动 ACME / 自签 / 自定义）  │
                         │  · /srv/frontend  SPA 静态托管           │
                         │  · /srv/drawio    draw.io 静态层         │
                         │  · /api/*  /mcp  /webdav* → backend      │
                         │  · /content/*  /raw/*     → backend      │
                         │  · /onlyoffice/*          → onlyoffice   │
                         └───────────────┬─────────────────────────┘
                                         │ compose 内网（无宿主映射）
          ┌──────────────┬───────────────┼───────────────┬──────────────┐
          ▼              ▼               ▼               ▼              ▼
     backend:8080   postgres:5432    redis:6379    onlyoffice:80   meilisearch
     （Go API）      （元数据）        （队列/广播）  （文档编辑）     / clamav
          │
          ├── backend_storage 卷（local 驱动时的文件对象）
          ├── tls_certs 卷（与 caddy 共享，只读挂给 caddy）
          └── docflow-agent-ipc 卷（Agent 容器 AI IPC socket）
```

### 2.2 三个二进制的镜像复用

`Dockerfile` 一次构建产出三个二进制，装进同一个 `alpine` 运行镜像：

| 二进制 | 源码 | 镜像内路径 | 职责 |
| --- | --- | --- | --- |
| `/docflow` | `cmd/server` | `ENTRYPOINT` | API 服务主进程 |
| `/migrate` | `cmd/migrate` | — | 按文件名序执行 `migrations/*.sql` |
| `/seed` | `cmd/seed` | — | 首次建库引导管理员（幂等） |

`docker-compose.yml` 中 `migrate`/`seed` 是**一次性服务**（`restart: "no"`），用 `entrypoint:` 覆盖镜像 ENTRYPOINT（注意：`command` 只覆盖 CMD，故必须用 entrypoint），并形成启动依赖链：

```
postgres(healthy) → migrate(completed) → seed(completed) → backend(healthy) → caddy
```

迁移实现细节（`cmd/migrate/main.go`）：版本号取文件名下划线前段整数，用 `schema_migrations` 表记录；**执行期持有 PostgreSQL 咨询锁** `pg_advisory_lock(0x444F4346)`（"DOCF" 的 ASCII），保证多实例并发启动时只有一个执行；支持 `-dry-run`。

seed 的幂等策略：`INSERT ... ON CONFLICT (email) DO UPDATE ... WHERE users.status='active'` —— 且**不会复活被禁用/锁定的账号**。密码强制强度校验（≥12 位、含大小写与数字）。

### 2.3 启动装配（`cmd/server/main.go`）

**没有 DI 框架**（无 wire / fx），全部靠显式构造 + `SetXxx` 后置注入。启动顺序：

1. `config.Load()` —— 读全部环境变量并做启动级校验，失败即 `log.Fatal`
2. `signal.NotifyContext` 建立可取消 ctx（**先于**后台任务），janitor 等共享
3. `gorm.Open` → 构造各 store/service
4. **注入式接线密集区**：大量 `SetXxx` 跨包接线
5. 通知/队列/搜索/向量按配置装配（可选组件开关）
6. `httpapi.NewHandler(...)` + 约 40 次 `handler.SetXxx(...)`
7. `handler.Register(router, ...)` —— **全部路由在这一次调用中注册**
8. `if cfg.JanitorEnabled { go j.RunForever(ctx) }`
9. `server.ListenAndServe()` 阻塞
10. 收到 SIGINT/SIGTERM 后按序收尾：`inProcess.Close(10s)` → `shutdownQueue()`（asynq，10s 超时）→ 入队客户端 `Close()` → 广播桥 `Close()`

三种接线模式：

| 模式 | 用途 | 示例 |
| --- | --- | --- |
| 构造注入 | 主要依赖 | `NewService(store, storage, ttl, maxSize, ...)` |
| `SetXxx` 后置注入 | **跨包解耦**（回调/能力函数） | `fileStore.SetACLResolver(aclService.ResolveForFile)` |
| 热读取 Provider (`func() T`) | 两级配置热生效 | `fileStore.SetMaxVersionsProvider(...)` |

`internal/http` 的 `Handler` 有 40+ 可选依赖字段。**未注入时对应端点返回 503 而非 panic** —— 这是刻意的，便于契约测试只装配所需依赖。

---

## 3. 后端模块划分

`internal/` 共 35 个子包。按职责分三层。

### 3.1 核心业务模块

| 模块 | 职责 |
| --- | --- |
| `auth` | 账号、会话（refresh 轮换）、access/refresh token、PAT、TOTP、WebDAV 令牌、邮箱换绑、中间件鉴权 |
| `files` | 文件/目录树、版本管理、blob 去重、回收站、配额、目录快照、复制/移动、列表检索 |
| `space` | 统一空间模型：生命周期、五级角色矩阵、成员/用户组授权、邀请、所有权转让 |
| `share` | 公开/私有分享、打包分享、密码与水印、访问统计、目录分享树授权 |
| `acl` | 路径级 ACL：`folder_acl` 链求值（近覆盖远、user 优先 space、同主体 deny 优先） |
| `upload` | tus 断点续传状态机、分片 PATCH、秒传去重、病毒扫描、S3/local 存储抽象、Office 校验 |
| `tagging` | 用户维度标签与 `file_tags` 关联（标签属创建者，不参与内容授权） |
| `group` | 管理端用户组，作为空间授权维度 |
| `invite` | 管理员邀请制注册 |
| `notify` | 站内通知：事件分发、落库、每用户每事件开关 |
| `webhook` | 出站 Webhook：HMAC 签名投递、退避重试、连续失败自动禁用 |
| `realtime` | WebSocket Hub + Redis Pub/Sub 跨实例广播桥 |
| `collab` | 富文本实时协作的房间管理（ProseMirror 权威排序端点） |
| `search` | 全文检索抽象：pg（ILIKE+tsvector）/ meili 双实现 + 索引构建器 |
| `audit` | 审计日志写入与查询 |
| `settings` | 运行时设置键定义、类型/范围校验、热读取、审计 |
| `ai` | 多 Provider 对话、内容抽取、OCR、摘要、RAG（关键词/hybrid+Qdrant）、rerank、联网搜索、用量记账、平台文件工具 |
| `agent` | Docker 沙箱 Agent 创作舱：镜像白名单、容器生命周期、产物同步与 diff |
| `oidc` | OIDC 客户端（授权码 + PKCE S256，手写标准库流程，不引 oauth2） |

### 3.2 基础设施模块

| 模块 | 职责 |
| --- | --- |
| `config` | 启动级配置（全部环境变量）读取与校验 |
| `http` | Gin 路由注册与全部 handler（体量最大，110+ 文件） |
| `tasks` | 后台任务队列：inprocess / redis(asynq) 双驱动，共用同一组处理函数 |
| `metrics` | Prometheus 指标与 `/metrics` 端点 |
| `mail` | 邮件抽象：Noop / SMTP / SettingsMailer（每次发送热读配置） |
| `readiness` | `/ready` 就绪探针 |
| `contenturl` | `/raw/*` 短期 HMAC 授权 token（HKDF 派生密钥） |
| `caddytls` | HTTPS 运行时切换：渲染 Caddyfile 并经 admin API 热下发 |
| `janitor` | 后台定期清理（过期会话、孤儿 blob、回收站、超期记录） |
| `backup` | 备份产物的**只读**校验（解析 manifest、重算 sha256） |
| `officetemplate` | 内置 Office 空白模板生成 |

### 3.3 集成模块

| 模块 | 职责 |
| --- | --- |
| `onlyoffice` | Document Server 集成：编辑配置 JWT、下载 token、保存回调幂等 |
| `webpkg` | 网页包（zip）安全解包与内容提供（Zip Slip / 符号链接 / 解压炸弹防护） |
| `mcp` | MCP 服务端（JSON-RPC 2.0 over HTTP）+ 平台/AI 工具集 |
| `mcpclient` | 外部 MCP 服务器轻客户端（Streamable HTTP，自研不引 SDK） |
| `xmind` | 解析 `.xmind` 思维导图（zip 容器） |

---

## 4. 数据模型

50 个增量迁移文件（`migrations/001`–`050`）呈现出的领域全貌。

### 4.1 内容寻址模型（核心设计）

这是整个数据模型的基石（migration 002 引入）：

```
files（文件可见实体）           file_versions（版本记录）        object_blobs（物理内容）
├─ id                        ├─ (file_id, version) UNIQUE     ├─ sha256 CHAR(64) UNIQUE
├─ parent_id（自引用树）  ───▶├─ object_blob_id ──────────────▶ ├─ storage_key
├─ space_id（NOT NULL）      ├─ content_sha256                ├─ ref_count
├─ owner_id                  ├─ size                          └─ status
├─ type(file|folder|webpage) └─ user_id
├─ current_version_id ──┐
├─ is_root / is_public  │
└─ deleted_at           └──▶ 指向某个 file_version
```

**关键点：`object_blobs.sha256` 唯一** —— 相同内容在物理上只存一份。多个文件（或多个版本）可共享同一 blob，靠 `ref_count` 引用计数回收。

这一个设计同时支撑了两件事：

- **秒传去重**：上传时先算 SHA-256，命中已有 `available` blob 则 `ref_count+1`，并删除本次上传的冗余物理对象
- **版本管理**：覆盖上传不复制内容，只新增 `file_versions` 行

其他约束：`(parent_id, lower(name))` 未删时唯一（同目录不可重名）；每空间至多一个根目录。

### 4.2 空间与权限域

```
users ──┬── spaces（owner_id）
        │      ├── space_members（五级角色，PK space_id+user_id）
        │      ├── space_group_members（PK space_id+group_id）
        │      └── space_invites
        └── group_members ──▶ groups
                                └──（经 space_group_members 授权到空间）
```

**空间是唯一的组织容器**（migration 040）。每个用户注册即自动获得一个默认空间「{name}的空间」（`is_default`，可改名不可删，`idx_spaces_default_per_user` 部分唯一索引保证每用户至多一个）。

五级角色矩阵（`internal/space/service.go` `rolePermissions`）：

| 角色 | read | write | delete | share | admin |
| --- | :-: | :-: | :-: | :-: | :-: |
| owner | ✓ | ✓ | ✓ | ✓ | ✓（+解散/转让） |
| admin | ✓ | ✓ | ✓ | ✓ | ✓ |
| member_share | ✓ | ✓ | ✓ | ✓ | — |
| member | ✓ | ✓ | ✓ | — | — |
| guest | ✓ | — | — | — | — |
| 非成员 | 全无（fail closed） | | | | |

**有效角色 = 直接成员角色 ∪ 用户组角色，取等级高者**（等级序 `owner(4) > admin(3) > member_share(2) > member(1) > guest(0)`）。

路径级 ACL（migration 031）叠加在其上：`folder_acl(folder_id, subject_type user|space, subject_id, effect allow|deny, permissions[])`。

### 4.3 权限模型的演进史（重要）

这条演进线能解释为什么当前代码里有一些"遗留"痕迹：

| 迁移 | 变化 | 结局 |
| --- | --- | --- |
| 008 | 引入 `teams`/`team_members` + `scope_type(personal\|team)` 双轨模型 | **已被 040 移除** |
| 026 | 建 `roles` 表（`permissions JSONB` 自定义角色） | **已被 037 移除** |
| 029 | `team_members.role_id` 指向自定义角色；权限继承、无路径覆盖 | **已被 037 收敛** |
| 031 | 引入 `folder_acl` 路径级 ACL | 保留 |
| 037 | **反向收敛**：自定义角色迁回五级内置，删除 `roles` 表与 `role_id` | 当前形态 |
| 040 | **统一空间**：DROP `teams`/`team_members`/`roles`/`share_teams`/`team_invites`，`files.team_id` RENAME 为 `space_id` | 当前形态 |

两个结论：

1. **权限模型最终收敛为固定五级内置角色**，不保留自定义角色能力（曾短暂引入过）。
2. **migration 040 是清库重建**，不做存量数据迁移（迁移头注释明确要求 `docker compose down` + 清 postgres 卷）。这使"个人/团队双轨"合并为唯一容器"空间"。

### 4.4 其他域

| 域 | 主要表 |
| --- | --- |
| 账号凭据 | `users`、`sessions`、`api_tokens`、`webdav_tokens`、`user_totp`、`oidc_links`、`email_change_codes`、`password_reset_tokens`、`invitations`、`user_ai_prefs`、`ai_memory`、`ai_usage` |
| 文件内容 | `files`、`file_versions`、`object_blobs`、`upload_sessions`、`directory_snapshots`、`web_packages`、`file_search_docs` |
| 分享 | `shares`、`share_users`、`share_spaces`、`share_files`、`share_access_sessions`、`file_access_events` |
| 平台 | `audit_logs`、`system_settings`、`notifications`、`webhooks`、`tags`/`file_tags`、`onlyoffice_callbacks`、`document_comments`、`agent_tasks`、`tls_state` |

---

## 5. 关键链路

### 5.1 认证与会话

**与常见做法不同的三点**：access token 不入 cookie；refresh token 每次使用都轮换；用户不存在时也执行等耗 bcrypt 以消除时序差异。

**登录**（`internal/http/handler.go`）：

1. 按 `identifier` 查用户
2. 用户不存在/非 active → 对 `dummyPasswordHash` 做等耗 bcrypt 比较后统一 401（**防账号枚举时序攻击**）
3. 锁定检查 → 423；密码错误 → `recordLoginFailure`（阈值/时长热读 `settings`）
4. **TOTP 启用时不发任何 token**，返回 401 `code=TOTP_REQUIRED`，走 `POST /api/v1/auth/login/totp` 二段提交
5. 签发 access token（**HS256 JWT，claims 仅 `RegisteredClaims`，subject = 用户 UUID**），**只在响应体返回**
6. refresh token 明文写入 cookie：`Name=refresh_token`、`Path=/api/v1/auth/refresh`、`HttpOnly`、`SameSite=Lax`、`Secure=cookieSecure`；**DB 只存 `refresh_token_hash CHAR(43)`**

**refresh**：轮换后**复查账号状态** —— 若账号已被禁用/锁定，撤销新 token 并返回 401/423。这是防止"禁用用户仍能靠旧 refresh 续期"的关键。

**中间件**（`internal/auth/middleware.go`）：`VerifyBearer` 先看是否 `dfpat_` 前缀（PAT 路径），否则 JWT 解析并**强制校验 `token.Method == HS256`**（防算法混淆）。成功注入 `user_id`、`pat_scopes`、`auth_kind`。

**CSRF**：`CSRF_STRICT=true`（默认）时，refresh/logout 缺失 Origin/Referer 一律 403。

**WS 鉴权**：从 `Sec-WebSocket-Protocol` 取 `bearer, <JWT>` 子协议对；**非 production 环境回退 `?access_token=`**（本地调试便利）。

### 5.2 权限判定链（层层回退）

统一模式 —— **文件行 owner 短路 → ACL 链命中则以其结果为准 → 未命中回退空间角色**：

```
authorizeFileAccess(file, user):
  1. file.OwnerID == user                    → 放行（上传者恒可读）
  2. ACL Resolve(file, user, "read")
       matched && !allowed                   → ErrNotFound（不泄露资源存在性）
       matched && allowed                    → 放行
  3. 未匹配 → spaceService.CanRead(user, spaceID)
       未注入判定器                          → 一律拒绝（安全默认）
       非成员                                → ErrNotFound
```

**ACL 与空间角色是"或"关系而非叠加**：ACL 链一旦 `matched`，空间角色判定**完全不参与** —— 可被 ACL 显式 deny 或 allow 覆盖。这是路径级 ACL 的设计意图（局部覆盖全局）。

ACL 求值（`internal/acl/acl.go`）：链由目标向上到空间根构造（文件自父目录起，目录含自身），沿途遇软删即截断；逐节点**由近及远**判定，优先级 `userDeny > userAllow > spaceDeny > spaceAllow`；链空则 `matched=false`。

ACL 管理端点另行门控：须空间 owner/admin 或系统 admin。

### 5.3 上传链路

自实现 tus 1.0.0（**不引 tusd**），全链路：

```
POST /api/v1/tus/files          创建会话
  ├─ 扩展名黑名单（upload.blocked_extensions 热读）
  ├─ 每用户并发会话上限
  ├─ 名称规范化（拒绝路径分隔符/控制字符/Windows 保留名）
  ├─ 父目录写权限（ACL write / CanWrite）
  ├─ 大小上限（upload.max_file_size 热读）
  ├─ 空间配额（超限 413）
  └─ storage_key = tmp/<sessionID>

PATCH .../:id                   分片上传
  ├─ Upload-Offset 不匹配 → 409
  ├─ Content-Length > PATCH_MAX_BYTES(64MiB) → 413
  ├─ 并发防护双保险：进程内 per-session Mutex + 存储层 CAS(AdvanceOffset)
  └─ 写满即自动触发 Complete（经队列，入队失败回退进程内 goroutine）

Complete 流水线（对终态幂等）
  uploading → verifying   全量重算 SHA-256 比对（防传输损坏）→ 不符置 failed
            → scanning    clamd INSTREAM 扫描 → 感染置 quarantined + 通知属主
            → available   移入 objects/<userID>/<sessionID>，删 tmp/*
                           └─ 秒传去重：sha256 命中已有 available blob
                              则 ref_count+1 复用，删除本次冗余物理对象

落库后钩子：入队 zip 解包 + 搜索索引；配额 ≥80% 发警告；写审计
```

扫描器（`internal/upload/clamav.go`）：拨号超时**独立 cap 5s**（避免 clamd 不可达时挂满 5 分钟）；`CLAMAV_REQUIRED=true`（默认）时拨号失败 **fail closed** 拒绝上传。

### 5.4 实时通知（含跨实例）

`GET /api/v1/ws/notifications` 挂在**根路由**（非 `/api/v1` 组）。

- `Hub`：`map[userID]map[*client]struct{}`，每 client 一个缓冲 16 的 send channel；**缓冲满视作失效连接异步摘除**
- `Broadcast`：先 `deliverLocal` 本地直发，再 `Publish` 到共享通道
- Redis 桥（`QUEUE_DRIVER=redis` 时）：频道 `docflow:notify`，复用队列同一 Redis 连接
- **去重回环**：`seen` map 记录本实例已发布的 msg_id（TTL 10s），订阅回调命中即跳过 —— 避免自己发布的消息经 Pub/Sub 回环再分发
- `QUEUE_DRIVER=inprocess` 时为 NoopBroadcaster：Hub 保持纯本地分发。**单实例自洽，多实例必须 redis**

### 5.5 富文本协作

`GET /api/v1/collab/:fileId/ws`，升级前校验文件写权限。

服务器**不理解 ProseMirror steps 语义**，只做四件事：JSON 透传、房间锁下统一排序、广播给全部参与者（含发送者，客户端按 clientID 忽略自己的）、版本计数（**仅累计 steps 总数**，非文档内容版本）。

后加入者一致性流程：进 `pending` → 广播 `sync-begin` 暂停 steps → 向 leader 发 `sync-request` → leader 回 `snapshot` → **同一次房间锁内**校验版本后原子转正、下发 `init-doc` → `sync-end`。

**已知局限（代码注释明示为有意为之）**：

1. **房间为每实例内存态** —— 多实例下同一文件协作者连到不同实例会落入不同房间、版本序列互相独立（**不引入 Redis 跨实例桥**）。单实例或按文件粘性会话部署下语义完整。
2. **不保存历史 steps** —— 断线重连即整篇重拉，无追赶/补发。
3. 写泵消费过慢即摘除连接（steps 一旦丢序即错版，宁可断开让对方重拉）。

> 因此**多实例部署必须按文件做粘性会话**，否则协作无法收敛。

### 5.6 后台任务与 janitor

**任务队列**（`internal/tasks/`）：`task:complete-upload`、`task:extract-webpkg`、`task:webhook-delivery`、`task:search-index`。

| | inprocess（默认） | redis(asynq) |
| --- | --- | --- |
| 执行 | 入队即起 goroutine | 每实例既生产又消费 |
| 重试 | **不重试**，仅记日志 | asynq 默认策略重试 |
| 跨实例 | 不支持 | 支持 |
| 启动校验 | 无依赖 | `PingRedis` 失败即 `Fatalf` |

两种驱动**共用同一组处理函数**，差异仅在重试语义与跨实例能力。

**janitor**（`JANITOR_INTERVAL` 默认 10m，启动即先跑一轮）：过期上传会话置 failed 并删 tmp 对象、终态会话超期删行、超期会话/PAT/通知清理、`ref_count=0` 的 blob 复核后物理删除、回收站超 `retention.trash_days` 彻底删除。

### 5.7 内容安全边界

文件原始内容经 `/content/*`（网页包）与 `/raw/*`（受控原始内容）提供，与主站会话**彻底解耦**：

```
Caddy:  header { -Cookie; Content-Security-Policy "sandbox allow-scripts"; nosniff }
        reverse_proxy { header_up -Cookie }     # 请求侧也剥
/raw/*  额外 Referrer-Policy: no-referrer        # URL 含短期 grant，不得外泄
```

`sandbox allow-scripts` **刻意不加 `allow-same-origin`** —— 浏览器因此给内容分配唯一化 opaque origin，脚本碰不到主站 localStorage / Cookie / DOM。

剥离 Cookie 是沙箱的**配套必要条件**：这些端点本就不依赖 Cookie（`/raw/*` 走 HMAC grant），主动双向剥离可确保内容 origin 与主站会话零关联。

后端侧（`internal/http/resolve.go`）：扩展名白名单，未知扩展 **415**（不提供 octet-stream，避免浏览器嗅探）；blob 须 `available`；响应带 `Cache-Control: private, no-store`。

### 5.8 HTTPS 运行时热切换

管理页可在**不重启容器**的前提下切换 TLS 模式（`internal/caddytls/`）：

| 模式 | 说明 |
| --- | --- |
| `http` | 明文（本地验证默认） |
| `auto` | 自动 ACME 签发受信证书（要求公网域名 DNS 指向本机） |
| `internal` | Caddy 内部 CA 自签（内网/IP，流量加密但浏览器不受信） |
| `custom` | 管理员上传的企业证书 |

机制：backend 渲染完整 Caddyfile 文本 → `POST http://caddy:2019/load`（`Content-Type: text/caddyfile`）→ Caddy 运行时原子替换配置。

三个要点：
- **非法配置被 Caddy 原子拒绝**，下发失败时不落库、保持旧配置
- **`{$VAR}` 占位符原样下发**，由 caddy 容器自身 env 展开 —— backend 无需复制站点配置
- **启动期对账**（`ReapplyStartup`）：持久化模式非 http 时最多重试 12 次 × 5s 重下发，覆盖"caddy 先于 backend 单独重启回到初始态"的窗口

安全边界：admin API 仅 `expose` 给 compose 内网（无宿主映射）；`tls_certs` 卷 backend 可写、caddy 只读。

---

## 6. 前端架构

### 6.1 路由与页面

单入口 `BrowserRouter`，路由扁平声明在 `frontend/src/App.tsx`。分三类：

| 类别 | 路径 | 外壳 |
| --- | --- | --- |
| 公开页 | `/login`、`/sso`、`/register/:token`、`/forgot`、`/reset/:token`、`/s/:token` | 无 |
| 常规页 | `/`、`/files`、`/dashboard`、`/spaces`、`/shared`、`/studio`、`/admin/:section`、`/settings/:section` | TopBar + 全局 AI Drawer |
| 独立编辑器 | `/view/:fileId`、`/edit/:fileId`、`/drawio/:fileId`、`/excalidraw/:fileId`、`/text/:fileId`、`/dfdoc/:fileId` 等 | `bare`（不挂顶栏） |

管理后台 13 个分区（`AdminPage.tsx`）：overview / people / spaces / audit / threat / backup / ai / agent / mail / tls / system / security / config。重面板经 `React.lazy` 懒加载。

`RequireAuth` 关键行为：`access_token` **仅存内存**（`useState`），未持有 token 时先用 `refreshSession()` 静默续期，失败才跳登录。

### 6.2 Token 与会话（前端侧）

`frontend/src/api.ts` 的机制：

- `access_token` 仅模块级内存变量，**不写 localStorage**；刷新页面后靠 refresh cookie 恢复
- **CSRF**：从 `document.cookie` 读 `docflow_csrf`，非 GET 请求加 `X-CSRF-Token` 头
- **401 自动刷新重放**：首次 401 → `refreshSession()` → 成功重放，失败清 token 并广播 `SESSION_EXPIRED_EVENT`
- **跨标签页串行**：额外用 `navigator.locks.request('docflow-refresh', {mode:'exclusive'})` —— 因为 refresh token 每次使用都轮换，**并发重放会触发整个 session 撤销**

### 6.3 构建与分包

`frontend/vite.config.ts`：

- `manualChunks` 只显式拆 3 个 vendor：`vendor-react`、`vendor-antd`、`vendor-icons`（首屏 gzip 目标 <400KB）；monaco / mermaid / excalidraw / tiptap 等**刻意不匹配规则**，保持 rollup 默认按使用方分包（即懒加载语义）
- `define` 固定 `process.env.IS_PREACT='false'`（excalidraw 入口按此分发 preact/React 构建）
- **PWA**：SW 只预缓存应用 Shell；mermaid 生态（5MB+）、富文本 Tiptap（~640KB）、Monaco（3MB+）及 worker **均不进预缓存**，在线按需拉取
- dev server 仅代理 `/api`，且 **`changeOrigin: false`** —— 必须为 false，后端 CSRF 校验要求 Origin/Referer 的 host 与请求 Host 一致

---

## 7. 部署与服务矩阵

### 7.1 Profiles

| profile | 服务 | 入口 |
| --- | --- | --- |
| `minimal` | caddy + backend + postgres（+ migrate/seed 一次性） | `make up-minimal` |
| `full` | + redis + onlyoffice | `make up-full` |
| `antivirus` | + clamav（可叠加） | `--profile antivirus` |
| `search` | + meilisearch（可叠加） | `--profile search` |
| `storage` | + minio（可叠加） | `--profile storage` |
| `ai-vector` | + qdrant | `--profile ai-vector` |
| `ai-search` | + searxng | `--profile ai-search` |

**关键约束**：核心三件套（caddy/backend/postgres）只列在 `minimal/full/antivirus` —— 因此 **search/storage/ai-vector/ai-search 必须与它们叠加启动**，不能单独起。且 profile 必须与 `.env` 的驱动开关联动（如 `SEARCH_DRIVER=meili` 不叠加 `--profile search` 会导致 backend 启动失败）。

`backend` 不声明 `depends_on: redis` —— 避免把 redis 拖进 minimal。

### 7.2 端口边界

**只有 caddy 映射宿主端口**（80/443）。其余服务的端口意图：

| 服务 | 端口 | 暴露方式 |
| --- | --- | --- |
| caddy admin | 2019 | 仅 `expose`（HTTPS 热切换下发通道，外部不可达） |
| backend | 8080 | 仅 `expose` |
| postgres | 5432 | 无 ports 无 expose |
| redis | 6379 | 同上（仅 compose 内网 DNS） |
| onlyoffice / clamav / meili / qdrant / minio / searxng | 各自 | 仅 `expose` |

### 7.3 数据卷与备份

| 卷 | 内容 | 备份 |
| --- | --- | --- |
| `postgres_data` | 全部元数据 | **必须**（`pg_dump`） |
| `backend_storage` | local 驱动的文件对象 | **必须**（仅 local；s3 时跳过） |
| `minio_data` | S3 对象（storage profile） | **必须**（若启用 s3） |
| `caddy_data` | ACME 证书与状态 | 建议（避免重复申请触发限流） |
| `tls_certs` | 自定义证书（含私钥） | 建议 |
| `onlyoffice_data` | DS 文档数据 | 视情形 |
| `meili_data` / `qdrant_data` | 索引/向量 | 可不备（可重建） |
| `clamav_data` / `caddy_config` / `onlyoffice_logs` | 病毒库/配置缓存/日志 | 不需要 |
| `docflow-agent-ipc` | Agent AI IPC socket | 不需要（`name:` 钉死实际卷名） |

`make backup` / `backup-verify` / `backup-windows`；`GET /admin/backups/status` 读校验标记。

### 7.4 镜像清单

| 镜像 | 构建 | 用途 |
| --- | --- | --- |
| `docflow/caddy:$TAG` | `frontend/Dockerfile`（上下文 `./frontend`） | 入口：TLS + SPA 静态 + draw.io 静态 + 反代 |
| `docflow/backend:$TAG` | `Dockerfile`（上下文 `.`） | Go API；migrate/seed 复用同镜像 |
| `docflow/onlyoffice:8.2.3-cjk` | `deploy/onlyoffice/Dockerfile` | 官方 DS + `fonts-noto-cjk`（无 CJK 字体时中文渲染为方块） |
| `docflow/agent:1.0.0` | `Dockerfile.agent` | Agent 沙箱执行镜像 |

**所有 up/deploy 目标都依赖 `build-agent`** —— Agent 镜像随部署自动构建。

### 7.5 Agent 沙箱安全模型

`internal/agent/docker.go` 直接经 Docker Engine HTTP API 创建容器，加固项：

`User 65534:65534`、`NetworkMode: none`、`Privileged: false`、`CapDrop: ["ALL"]`、`no-new-privileges`、`ReadonlyRootfs: true`、`PidsLimit: 64`、tmpfs `/tmp`（`noexec,nosuid,nodev`）、CPU/内存受控。

三重边界：
1. **workspace 必须是 `os.TempDir()` 下 `docflow-agent-` 前缀目录**，且非符号链接 —— 绝不暴露 daemon socket 或宿主内部目录
2. **绝不持久化 stdout/stderr** —— 容器输出可能回显 prompt 或密钥，只取固定状态字符串
3. **断网容器调平台 AI 走 unix socket**（`/run/docflow-ipc/ai.sock`）：`NetworkMode=none` 下经共享卷挂载的 socket 是唯一通道，鉴权用任务级 Bearer token，每任务调用上限 `agent.ai_max_calls`

---

## 8. 配置两级模型

| 级别 | 载体 | 修改 | 生效 |
| --- | --- | --- | --- |
| 启动级 | 环境变量（`internal/config`） | 编辑 `.env` / compose | **改后必须重启 backend** |
| 运行时 | DB 表 `system_settings`（`internal/settings`） | 管理端在线修改 | 多数即时生效；个别键标注须重启 |

**非密钥原则**：密钥类配置（`JWT_SECRET`、S3 凭据、OIDC secret、`ONLYOFFICE_JWT_SECRET`、`AI_API_KEY` 等）**只走环境变量，不入库**。

已声明的例外（允许入库、**只写不读**，读路径恒掩码 `******`）：SMTP 密码、AI Provider `api_key`、Tavily Key、MCP `auth_header`。

与 env 重叠的运行时键**优先取库值，读取失败回退 env 基线** —— 实现即 `main.go` 里的 `SetXxxProvider` 闭包。

每个设置键带 `Effect` 元数据：`EffectImmediate`（消费方每次请求热读）/ `EffectRestart`。注意**部分 `EffectRestart` 键当前根本没有热读取消费方**（如 `security.rate_limit_per_minute`、`backup.*`），即"改了不会有任何效果"。

逐键说明见 [configuration.md](configuration.md)。

---

## 9. 设计取舍与已知局限

完整罗列**有意为之**的取舍（非缺陷，但使用者需知）：

### 9.1 明确不支持的能力

| 项 | 说明 |
| --- | --- |
| **中文全文检索** | `tsv` 用 `to_tsvector('simple', ...)`，中文整句成单 token；实际依赖名称 `ILIKE` 兜底 |
| **富文本协作多实例** | 房间为每实例内存态，多实例需按文件粘性会话 |
| **自定义角色** | 曾引入（026/029），已在 037 收敛回五级内置 |
| **协作历史 steps** | 不保存，断线即整篇重拉 |
| **inprocess 队列跨实例** | 多实例必须 `QUEUE_DRIVER=redis` |

### 9.2 数据模型遗留

以下列/表**存在但当前无消费方**，属历史痕迹：

- `casbin_rule` 表（001 遗留）
- `thumbnail_jobs` 表 + `task:generate-thumbnail` 常量（已声明，无处理函数与入队方）
- `files.tree_path`（ltree）列（目录深度实际按 `parent_id` 链遍历）

### 9.3 与常见做法不同

| 项 | 常见做法 | DocFlow |
| --- | --- | --- |
| DI | wire/fx 容器 | 无框架，40+ `SetXxx` 后置注入 |
| access token 存放 | 响应体 + cookie | **仅响应体** |
| refresh token | 长期有效 | **每次使用轮换** |
| Caddy 上游 | 动态 upstream | 默认"丢弃端口" `127.0.0.1:9`，规避 minimal 下 DNS 解析失败退出 |
| CSP 下发 | 入口统一 | **交由应用层**，Caddy 只发 nosniff + Referrer-Policy |
| 密码/Token 哈希 | 统一算法 | **密码用 bcrypt，分享密码与各类 token 用 SHA-256** |

---

## 10. 目录结构速查

```
cmd/            server / migrate / seed / backup-verify / wscheck（后两个不进镜像）
internal/       35 个业务与基础设施模块（见 §3）
migrations/     增量 SQL，按文件名序执行，幂等
frontend/       React SPA（src/）+ E2E（e2e/）+ 多阶段 Dockerfile
deploy/         Caddyfile（TLS/反代/子路径）+ onlyoffice Dockerfile
deploy/env/     四套场景化 .env 模板
scripts/        备份/恢复/验证脚本（sh + ps1）
docs/           本目录（架构/配置/MCP/API 契约）
```

关键入口文件：

- 后端装配：[cmd/server/main.go](../cmd/server/main.go)
- 路由注册：[internal/http/handler.go](../internal/http/handler.go)
- 启动级配置：[internal/config/config.go](../internal/config/config.go)
- 运行时设置：[internal/settings/settings.go](../internal/settings/settings.go)
- 前端路由：[frontend/src/App.tsx](../frontend/src/App.tsx)
- API 客户端：[frontend/src/api.ts](../frontend/src/api.ts)
- 编排：[docker-compose.yml](../docker-compose.yml)
- 入口配置：[deploy/Caddyfile](../deploy/Caddyfile)
