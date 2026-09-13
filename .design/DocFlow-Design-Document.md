# DocFlow 产品设计文档

**版本**：v1.0 设计基线（需评审）
**协议**：Apache 2.0
**最后更新**：2026-09-09
**状态**：待评审

---

## 目录

1. [产品概述](#1-产品概述)
2. [核心场景与用户故事](#2-核心场景与用户故事)
3. [信息架构与数据模型](#3-信息架构与数据模型)
4. [系统架构](#4-系统架构)
5. [技术栈详细说明](#5-技术栈详细说明)
6. [功能规格说明](#6-功能规格说明)
7. [权限与安全设计](#7-权限与安全设计)
8. [会话管理与 API Token](#8-会话管理与-api-token)
9. [API 设计规范](#9-api-设计规范)
10. [前端设计规范](#10-前端设计规范)
11. [集成服务设计](#11-集成服务设计)
12. [部署架构](#12-部署架构)
13. [配置与运维](#13-配置与运维)
14. [国际化与本地化](#14-国际化与本地化)
15. [开发路线图](#15-开发路线图)
16. [附录](#16-附录)
    - [16.1 名词解释](#161-名词解释)
    - [16.2 安全检查清单](#162-安全检查清单)
    - [16.3 推荐部署组合](#163-推荐部署组合)
    - [16.4 参考资料](#164-参考资料)
    - [16.5 已知风险与缓解策略](#165-已知风险与缓解策略)
    - [16.6 风险监控指标](#166-风险监控指标)

---

## 1. 产品概述

### 1.1 项目简介

**DocFlow** 是一款**自托管的文件发布与协作平台**，让团队能够上传文档（Word、PDF、图片等）、设计文件、静态网页包，然后通过可分享链接让成员**点开即看、即改**，无需下载到本地。

### 1.2 一句话定位

> **DocFlow** — Your docs, flowing to your team.

### 1.3 核心价值主张

| 价值 | 说明 |
|---|---|
| **🎯 零下载查看** | 上传后访客通过浏览器直接查看，零下载、零安装 |
| **✍️ 在线协作编辑** | Word/PPT/Excel/PDF 通过 ONLYOFFICE 实时协作 |
| **🔗 双模式分享** | 公开链接 + 私有分享给指定用户/团队 |
| **🎨 设计协作** | 内置 drawIO（流程图）+ Excalidraw（手绘草图） |
| **🌐 静态网页托管** | 直接上传 zip/tar 包或目录，作为网站访问 |
| **🔒 完全私有** | 自托管，数据完全可控 |
| **🌍 开源免费** | Apache 2.0 协议，永久免费 |

### 1.4 目标用户

| 用户类型 | 典型场景 |
|---|---|
| **中小团队（5-50 人）** | 产品研发团队、咨询公司、设计工作室 |
| **企业内部部门** | 文档协作中心、设计资源库 |
| **个人开发者** | 个人项目文档管理、设计稿归档 |
| **教育/培训机构** | 教学资料共享、课程内容分发 |

### 1.5 范围决策表

| 版本 | 范围 | 明确不包含 |
|---|---|---|
| **MVP** | 邀请制认证；个人文件与固定根目录；文件夹；tus 上传/下载；基础图片/PDF/文本预览；回收站；基础 RBAC；公开分享；基础审计；可部署运行 | 团队协作、权限继承、私有分享、ONLYOFFICE、版本控制、网页包安全预览、标签、批量操作、会话/PAT、通知、drawio/Excalidraw、SSO/2FA/AI/全文搜索 |
| **v1.0** | 团队与权限继承；私有分享；ONLYOFFICE；版本控制；网页包安全预览；标签；批量操作；会话/PAT；站内通知；drawio/Excalidraw（若工作量不合适顺延 v1.1） | SSO、2FA、AI、全文搜索及其他非核心体验增强 |
| **v1.1** | 非核心体验增强：PWA 离线优化、主题/快捷键、仪表盘、邮件/Webhook、性能与部署体验 | 核心数据安全边界不降低 |
| **v2** | SSO/OIDC、2FA、AI、全文搜索、高级版本对比/合并、移动端/桌面端等 | — |

**设计决策**：MVP 先建立安全、可部署的文件闭环；协作和体验能力按依赖关系后置，不以时间预测作为发布承诺。

### 1.6 非目标

- ❌ SaaS 多租户平台（单实例 + 多用户 + 团队）
- ❌ 移动端原生 App（桌面优先，PWA 仅 v1.1 增强）
- ❌ 自研 Office 协作（依赖 ONLYOFFICE）
- ❌ SSO/2FA/AI/全文搜索（v2）

### 1.6 与竞品对比

| 特性 | DocFlow | Nextcloud + ONLYOFFICE | 语雀 | Notion |
|---|---|---|---|---|
| 部署方式 | 自托管 | 自托管 | SaaS | SaaS |
| Office 在线编辑 | ✅ ONLYOFFICE | ✅ ONLYOFFICE | ⚠️ 自研（弱）| ⚠️ 自研（弱）|
| 公开分享链接 | ✅ 短链+水印 | ✅ 链接+密码 | ⚠️ 弱 | ⚠️ 弱 |
| 静态网页托管 | ✅ 原生支持 | ⚠️ 需插件 | ❌ | ❌ |
| drawIO 集成 | ✅ 原生 | ⚠️ 需配置 | ❌ | ❌ |
| Excalidraw 集成 | ✅ 原生 | ❌ | ❌ | ❌ |
| 开源协议 | Apache 2.0 | Apache 2.0 | 闭源 | 闭源 |
| 上手成本 | 🟢 简单 | 🟡 复杂 | 🟢 简单 | 🟢 简单 |

---

## 2. 核心场景与用户故事

### 2.1 主要使用场景

#### 场景 1：产品设计稿交付
```
产品经理小张完成了一份 PRD（Word 文档）和原型设计（HTML 包）。
他将文件上传到 DocFlow，生成可分享链接发送给客户和开发团队。
客户点开链接直接查看 Word 文档（带水印），无需登录或下载。
开发团队可以同时查看 HTML 原型，并在线批注 Word 文档。
```

#### 场景 2：内部文档协作
```
某公司 10 人的产品团队：
- 产品文档统一存放在 DocFlow 的"产品文档"目录
- 每个产品有自己的子文件夹，按版本组织
- 团队成员按角色权限（编辑/只读）访问
- 所有 Word/PPT 变更通过 ONLYOFFICE 在线协作
- 关键会议纪要设置只读分享链接给跨部门同事
```

#### 场景 3：教学资料分发
```
某培训机构：
- 老师上传课件（PDF/PPT）、课程 demo（HTML 网页包）
- 为每个班级生成可分享链接（含密码）
- 学生在浏览器中直接查看课件，无需安装 Office
- 老师可以随时更新文件，学生看到最新版本
```

#### 场景 4：设计协作
```
设计师小李：
- 上传 .fig 导出图（PNG）+ 流程图（drawio）+ 草图（excalidraw）
- 通过 DocFlow 集中管理所有设计资源
- 与产品经理通过评论协作（v2.0）
- 最终定稿后生成只读分享链接给开发
```

#### 场景 5：私有协作
```
某咨询公司：
- 项目文档严格保密，只能给项目组成员访问
- 通过私有分享给指定用户/团队
- 外部协作者（外包设计）只能看到分享给他的特定文件
- 所有访问记录可在审计日志中追溯
```

### 2.2 用户故事清单

| ID | 优先级 | 角色 | 用户故事 |
|---|---|---|---|
| US-001 | P0 | 访客 | 我希望点开链接就能查看文档，无需登录或下载 |
| US-002 | P0 | 团队成员 | 我希望在线编辑 Word/PPT，与同事实时协作 |
| US-003 | P0 | 文件所有者 | 我希望生成有密码和过期时间的公开分享链接 |
| US-004 | P0 | 管理员 | 我希望创建团队，批量管理成员权限 |
| US-005 | P0 | 设计师 | 我希望上传 HTML 网页包，让客户在线预览产品 |
| US-006 | P0 | 设计师 | 我希望用 drawIO 画流程图，保存到 DocFlow |
| US-007 | P0 | 设计师 | 我希望用 Excalidraw 画草图，保存到 DocFlow |
| US-008 | P0 | 团队成员 | 我希望上传大文件时支持断点续传 |
| US-009 | P0 | 团队成员 | 我希望搜索文件（按名称、类型、时间）|
| US-010 | P0 | 文件所有者 | 我希望查看文件被谁访问过（分享统计）|
| US-011 | P0 | 文件所有者 | 我希望私有分享给指定用户或团队 |
| US-012 | P1 | 团队成员 | 我希望批量上传/下载/删除文件 |
| US-013 | P1 | 团队成员 | 我希望给文件打标签、按标签筛选 |
| US-014 | P1 | 团队成员 | 我希望恢复误删的文件（回收站 + 撤销）|
| US-015 | P1 | 管理员 | 我希望审计日志记录所有关键操作 |
| US-016 | P1 | 团队成员 | 我希望文件有版本历史，可回滚 |
| US-017 | P1 | 管理员 | 我希望切换主题色（5 种）|
| US-018 | P1 | 团队成员 | 我希望收到文件变更通知 |
| US-019 | P1 | 用户 | 我希望查看我的登录设备、远程登出 |
| US-020 | P1 | 开发者 | 我希望创建个人 API Token 用于外部脚本 |
| US-021 | P1 | 用户 | 我希望导出我的所有数据 |
| US-022 | P1 | 用户 | 我希望切换中英文界面 |
| US-023 | P1 | 团队成员 | 我希望使用键盘快捷键高效操作 |
| US-024 | P1 | 用户 | 我希望撤销误操作（删除后 5 秒内可撤销）|
| US-025 | P2 | 用户 | 我希望用 AI 搜索文件（v2.0）|
| US-026 | P2 | 管理员 | 我希望 Webhook 集成外部系统 |
| US-027 | P2 | 团队成员 | 我希望离线访问最近文件（PWA）|

---

## 3. 信息架构与数据模型

### 3.1 实体关系图

```
┌────────────┐         ┌────────────┐         ┌────────────┐
│   User     │         │   Team     │         │   Role     │
│            │ N     N │            │ N     N │            │
│  ID        │─────────│  ID        │         │  ID        │
│  Username  │         │  Name      │         │  Name      │
│  Email     │         │  Desc      │         │  Desc      │
│  Password  │         │  CreatedBy │         │  Permissions│
│  Avatar    │         └────────────┘         └────────────┘
│  Status    │              │                       │
└─────┬──────┘              │                       │
      │ N                  │ N                     │
      │                    │                       │
      │ 1                  │ 1                     ▼
      ▼                    ▼                ┌──────────────┐
┌────────────┐         ┌────────────┐         │  Permission  │
│   File     │         │TeamMember  │         │   (Casbin)  │
│            │         │            │         │              │
│  ID        │ 1     N │  TeamID    │         │  Subject     │
│  Name      │─────────│  UserID    │         │  Object      │
│  Path      │         │  Role      │         │  Action      │
│  ParentID  │         │  JoinedAt  │         │  Effect      │
│  OwnerID   │         └────────────┘         └──────────────┘
│  Type      │
│  Size      │         ┌────────────┐
│  MimeType  │ 1     N │  Share     │
│  MD5       │─────────│            │
│  StorageKey│         │  Token     │
│  CreatedAt │         │  FileID    │
│  UpdatedAt │         │  Type      │ ← public/private
│  DeletedAt │         │  Password  │
└─────┬──────┘         │  ExpiresAt │
      │                │  AllowedUsers/Teams
      │                └────────────┘
      │ 1
      │
      │ N
      ▼
┌────────────┐         ┌────────────┐         ┌────────────┐
│FileVersion │         │  AuditLog  │         │  Session   │
│            │         │            │         │            │
│  ID        │         │  ID        │         │  ID        │
│  FileID    │         │  UserID    │         │  UserID    │
│  Version   │         │  Action    │         │  Token     │
│  StorageKey│         │  IP        │         │  IP/UA     │
│  Size      │         │  Resource  │         │  Device    │
│  Comment   │         │  CreatedAt │         │  CreatedAt │
│  UserID    │         └────────────┘         │  LastActive│
│  CreatedAt │                                │  ExpiresAt │
└────────────┘                                └────────────┘

      ┌────────────┐         ┌────────────┐
      │  APIToken  │         │    Tag     │
      │            │         │            │
      │  ID        │         │  ID        │
      │  UserID    │         │  Name      │
      │  Name      │         │  Color     │
      │  Token     │         │  CreatedBy │
      │  Scopes    │         └─────┬──────┘
      │  ExpiresAt │               │
      │  LastUsedAt│               │ N
      └────────────┘               │
                                   ▼
                            ┌────────────┐
                            │  FileTag   │
                            │            │
                            │  FileID    │
                            │  TagID     │
                            └────────────┘
```

### 3.2 核心实体详细说明

#### 3.2.1 User（用户）
```yaml
字段:
  id:              UUID 主键
  username:        用户名（唯一，3-32字符，字母数字下划线）
  email:           邮箱（唯一，登录用）
  password_hash:   bcrypt 哈希（cost=12）
  nickname:        昵称（可空，默认等于 username）
  avatar:          头像 URL（可空，自动生成首字母头像）
  department:      部门（可空）
  position:        职位（可空）
  phone:           电话（可空）
  bio:             简介（可空）
  status:          状态（active/disabled/locked）
  language:        偏好语言（zh-CN/en-US）
  timezone:        时区（默认 UTC）
  storage_quota:   存储配额（字节，默认 10GB）
  storage_used:    已用存储（字节，触发器更新）
  last_login_at:   最后登录时间
  last_login_ip:   最后登录 IP
  failed_login_count: 连续失败次数
  locked_until:    锁定截止时间
  is_system:       是否系统账号（不可删除）
  created_at:      创建时间
  updated_at:      更新时间
  deleted_at:      软删除时间
约束:
  - username 唯一
  - email 唯一
  - 软删除
索引:
  - (username), (email), (status, deleted_at)
```

#### 3.2.2 Team（团队，原 Group）
```yaml
字段:
  id:          UUID 主键
  name:        团队名（唯一）
  description: 描述
  avatar:      团队头像（可空）
  created_by:  创建者（User.ID）
  created_at:  创建时间
  updated_at:  更新时间
  deleted_at:  软删除
关系:
  - members:   多对多（TeamMember 表，含 role）
  - permissions: 通过 Casbin 关联
```

#### 3.2.3 TeamMember（团队成员）
```yaml
字段:
  id:         主键
  team_id:    团队 ID
  user_id:    用户 ID
  role:       团队内角色（owner/admin/member）
  joined_at:  加入时间
约束:
  - (team_id, user_id) 唯一
```

#### 3.2.4 File（文件/文件夹）
```yaml
字段:
  id:           UUID 主键
  name:         Unicode NFC 名称
  parent_id:    父目录 ID（所有节点非空；根目录使用固定 root_folder_id）
  tree_path:    ltree 层级路径（仅用于查询，不包含名称）
  owner_id:     所有者 ID
  team_id:      所属团队（可空，个人文件）
  type:         类型（file/folder/webpage）
  current_version_id: 当前版本 ID（文件可空，文件夹必须为空）
  description:  描述
  is_root:      是否系统创建的不可删除根目录
  scope_type:   作用域（personal/team）
  is_starred:   是否收藏（用户维度状态另行建模时替换）
  is_public:    是否存在有效公开分享（辅助标记）
  view_count:   浏览次数
  download_count: 下载次数
  created_at:   创建时间
  updated_at:   更新时间
  deleted_at:   软删除时间
约束:
  - 每个用户/团队仅有一个 is_root=true 的固定根目录
  - 同一 parent_id 下 lower(name) 唯一；根目录统一使用固定 root_folder_id
  - 名称使用 Unicode NFC，去除首尾空白，最长 255 个 Unicode code points
  - 拒绝空名、/、\\、NUL、控制字符、结尾点/空格及 Windows 保留名称
  - 重命名按大小写不敏感规则检查冲突，大小写变化本身允许
  - parent_id 为目录关系权威字段；禁止移动到自身或后代
索引:
  - parent_id、owner_id、team_id、deleted_at、tree_path
  - 同一 parent_id 的 lower(name) 唯一索引
```

#### 3.2.5 FileVersion（不可变逻辑版本）
```yaml
字段:
  id:           UUID 主键
  file_id:      文件 ID
  version:      版本号（自增，从 1 开始）
  object_blob_id: 物理对象 ID
  content_sha256: 内容 SHA-256（服务端权威校验值）
  size:         该版本大小
  comment:      版本说明（可选）
  user_id:      上传者或系统账号
  created_at:   创建时间
约束:
  - (file_id, version) 唯一；版本创建后不可修改
  - File.current_version_id 指向当前版本
  - 每个文件默认保留最新 5 个版本（MAX_VERSIONS_PER_FILE 可配）
策略:
  - 清理版本只解除引用；不得删除仍被引用的 ObjectBlob
```

#### 3.2.6 ObjectBlob（内容寻址物理对象）
```yaml
字段:
  id:           UUID 主键
  sha256:       内容 SHA-256，唯一
  storage_key:  对象存储 Key
  size:         对象大小
  mime_type:    服务端检测后的 MIME 类型
  ref_count:    FileVersion 引用计数
  status:       created/scanning/available/quarantined/failed/deleting
  created_at:   创建时间
约束:
  - 只有 available 对象可以被预览、下载、分享或用于新版本
  - sha256 相同的 available 对象可复用，但必须重新校验授权与配额
  - ref_count 归零后进入延迟垃圾回收；回收前再次确认无引用
```

#### 3.2.7 Share（分享）
```yaml
字段:
  id:              UUID 主键
  token:           8位哈希（URL 短链）
  file_id:         分享的文件/文件夹 ID
  type:            分享类型（public=公开链接/private=私有分享）
  password:        密码哈希（可空）
  expires_at:      过期时间（NULL = 永久）
  max_downloads:   最大下载次数（NULL = 无限）
  download_count:  已下载次数
  view_count:      已查看次数
  last_access_at:  最后访问时间
  allow_download:  是否允许下载（默认 true）
  watermark_enabled: 是否启用水印（默认 true）
  watermark_custom_text: 自定义水印文本（默认空，使用系统默认）
  # 私有分享授权存于 share_users / share_teams 关联表
  require_login:   是否要求登录（公开分享可设为 false）
  created_by:      创建者
  created_at:      创建时间
约束:
  - token 唯一（带索引）
  - 公开分享 type='public'，私有分享 type='private'
```

#### 3.2.8 Tag（标签）
```yaml
字段:
  id:         UUID 主键
  name:       标签名（全局唯一）
  color:      颜色（HEX，如 #4F46E5）
  created_by: 创建者
  created_at: 创建时间
```

#### 3.2.9 FileTag（文件标签关联）
```yaml
字段:
  file_id: 文件 ID
  tag_id:  标签 ID
主键: (file_id, tag_id)
```

#### 3.2.10 AuditLog（审计日志）
```yaml
字段:
  id:            BIGSERIAL 主键
  user_id:       操作者（NULL 表示系统/匿名）
  action:        动作（upload/download/delete/share/login/permission_change 等）
  resource_type: 资源类型（file/folder/share/team/user/role）
  resource_id:   资源 ID
  ip:            客户端 IP
  user_agent:    User-Agent
  status:        状态（success/failure）
  metadata:      JSONB 扩展字段
  created_at:    创建时间
策略:
  - 默认保留 90 天（LOG_RETENTION_DAYS 可配）
  - 定期归档到冷存储（可选）
索引:
  - (user_id, created_at), (action, created_at), (resource_type, resource_id)
```

#### 3.2.11 Session（会话，用于会话管理）
```yaml
字段:
  id:             UUID 主键
  user_id:        用户 ID
  refresh_token_hash: Refresh Token 哈希
  ip:             登录 IP
  user_agent:     User-Agent
  device:         设备信息（解析自 UA）
  location:       地理位置（可选，需 IP 库）
  is_current:     是否当前会话（用于前端标识）
  last_active_at: 最后活跃时间
  created_at:     创建时间
  expires_at:     过期时间
  revoked_at:     撤销时间（NULL = 未撤销）
约束:
  - 用户可以查看自己的所有活跃会话
  - 可远程撤销（删除）
索引:
  - (user_id, revoked_at), (refresh_token_hash)
```

#### 3.2.12 APIToken（个人访问令牌 PAT）
```yaml
字段:
  id:           UUID 主键
  user_id:      用户 ID
  name:         Token 名称（如 "我的脚本"）
  token_hash:   Token SHA256 哈希（仅存哈希，不存明文）
  prefix:       Token 前缀（如 docflow_pat_xxx，用于 UI 展示）
  scopes:       TEXT[] 权限范围（files:read, files:write, shares:create 等）
  expires_at:   过期时间（NULL = 不过期，但不推荐）
  last_used_at: 最后使用时间
  created_at:   创建时间
  revoked_at:   撤销时间
约束:
  - 创建时明文 Token 仅返回一次
  - 撤销后立即失效
```

#### 3.2.13 Permission（权限策略，Casbin 管理）
```yaml
模型: RBAC + ABAC（Casbin）
subject:   user:UUID / team:UUID / role:NAME
object:    file:UUID / folder:UUID / /path/* / *
action:    read / write / delete / share / admin
effect:    allow / deny
规则示例:
  - super_admin, *, *, allow
  - admin, /admin/*, *, allow
  - alice, file:UUID001, read, allow
  - team:design, /design/*, write, allow
  - team:design, /design/*, share, allow
  - *, /public/*, read, allow
存储: Casbin 自带 adapter（GORM adapter），存于 casbin_rule 表
```

#### 3.2.14 补充实体
```yaml
Invitation: invitations(id, email, invited_by, role, token_hash, expires_at, accepted_at, created_at)
PasswordResetToken: password_reset_tokens(id, user_id, token_hash, expires_at, used_at, created_at)
ShareUser: share_users(share_id, user_id, created_at)，(share_id, user_id) 唯一
ShareTeam: share_teams(share_id, team_id, created_at)，(share_id, team_id) 唯一
ShareAccessSession: share_access_sessions(id, share_id, user_id/session_hash, expires_at, created_at)
FileAccessEvent: file_access_events(id, file_id, share_id, user_id, action, ip_hash, user_agent, created_at)
UploadSession: upload_sessions(id, user_id, parent_id, tus_id, size, sha256, status, expires_at, retry_count, created_at, completed_at)
UserNotificationPreference: user_notification_preferences(user_id, event_type, enabled, updated_at)
Webhook: webhooks(id, user_id, url, events, secret_hash, enabled, created_at, updated_at)
Notification: notifications(id, user_id, type, title, body, resource_id, is_read, created_at, read_at)
SystemSetting: system_settings(key, value_json, value_type, description, updated_by, updated_at)
ObjectBlob：详见 3.2.6；表为 `object_blobs(id, sha256, storage_key, size, mime_type, ref_count, status, created_at)`
```
过期清理由定时任务处理 invitations/password_reset_tokens/share_access_sessions/upload_sessions；按 created_at 的审计、访问事件、通知、版本和回收站保留策略清理，所有清理可审计。

### 3.3 数据库表清单

| # | 表名 | 用途 | 关键索引 |
|---|---|---|---|
| 1 | `users` | 用户 | username, email |
| 2 | `teams` | 团队 | name |
| 3 | `team_members` | 团队成员 | (team_id, user_id) |
| 4 | `files` | 文件/文件夹 | parent_id, owner_id, (parent_id, name) |
| 5 | `file_versions` | 文件版本 | (file_id, version) |
| 6 | `shares` | 分享 | token |
| 7 | `tags` | 标签 | name |
| 8 | `file_tags` | 文件标签 | (file_id, tag_id) |
| 9 | `audit_logs` | 审计日志 | (user_id, created_at) |
| 10 | `sessions` | 会话 | (user_id, revoked_at) |
| 11 | `api_tokens` | API Token | (user_id, token_hash) |
| 12 | `casbin_rule` | 权限规则 | (ptype, v0, v1) |
| 13 | `webhooks` | Webhook | (user_id, event) |
| 14 | `notifications` | 站内通知 | (user_id, is_read) |
| 15 | `system_settings` | 非密钥运行参数，JSON 值、类型、描述、更新者/时间 | key |
| 16 | `invitations` | 邀请制认证 | (email, expires_at) |
| 17 | `password_reset_tokens` | 密码重置一次性令牌 | (user_id, expires_at), token_hash |
| 18 | `share_users` / `share_teams` | 私有分享授权关联 | 各自唯一约束 |
| 19 | `share_access_sessions` | 分享访问会话 | (share_id, expires_at) |
| 20 | `file_access_events` | 文件访问统计（IP 哈希/脱敏） | (file_id, created_at), (share_id, created_at) |
| 21 | `upload_sessions` | tus 上传会话与状态 | (tus_id), (expires_at, status) |
| 22 | `user_notification_preferences` | 用户通知偏好 | (user_id, event_type) |
| 23 | `object_blobs` | SHA-256 内容寻址物理对象 | sha256, (status, ref_count) |

---

## 4. 系统架构

### 4.1 整体架构图

```
┌─────────────────────────────────────────────────────────────┐
│                     客户端 (Browser / PWA)                   │
│   React SPA + shadcn/ui + Tailwind + i18next + PWA          │
└──────────────────────────┬──────────────────────────────────┘
                           │ HTTPS / HTTP（可配置）
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                反向代理层 (Caddy / Nginx)                     │
│   - TLS 终止（可选）                                         │
│   - 静态资源服务                                              │
│   - WebSocket 升级                                            │
│   - 限流（IP/UA）                                             │
└──────────────────────────┬──────────────────────────────────┘
                           │
        ┌──────────────────┼──────────────────┬─────────────┐
        ▼                  ▼                  ▼             ▼
┌──────────────┐  ┌──────────────┐  ┌──────────────┐  ┌──────────┐
│  /api/*      │  │  /onlyoffice │  │  /drawio     │  │ /assets  │
│  Go API      │  │  ONLYOFFICE  │  │  jgraph/     │  │ 静态资源  │
│  (Gin + Casbin)│ │  Document Srv│  │  drawio      │  │          │
└──────┬───────┘  └──────┬───────┘  └──────┬───────┘  └──────────┘
       │                 │                 │
       └─────────────────┼─────────────────┘
                         │ (读写文件)
                         ▼
┌─────────────────────────────────────────────────────────────┐
│                       数据 & 服务层                          │
│  ┌────────────┐ ┌────────────┐ ┌────────────┐              │
│  │ PostgreSQL │ │   Redis    │ │ SeaweedFS  │              │
│  │ (元数据)    │ │(缓存/会话/ │ │ (S3 对象)   │              │
│  │            │ │  队列)     │ │            │              │
│  └────────────┘ └────────────┘ └────────────┘              │
│                                                             │
│  v2.0+  ┌────────────┐                                       │
│         │ Meilisearch│                                       │
│         │ (全文搜索) │                                       │
│         └────────────┘                                       │
│                                                             │
│  可选   ┌────────────┐                                       │
│         │   ClamAV   │                                       │
│         │(病毒扫描)   │                                       │
│         └────────────┘                                       │
└─────────────────────────────────────────────────────────────┘
```

### 4.2 后端模块划分

```
backend/
├── cmd/server/               # 主入口
├── internal/
│   ├── handler/              # HTTP handlers (Gin)
│   │   ├── auth.go           # 认证（登录/注册/刷新/登出）
│   │   ├── user.go           # 用户管理
│   │   ├── team.go           # 团队管理
│   │   ├── file.go           # 文件 CRUD
│   │   ├── folder.go         # 文件夹操作
│   │   ├── share.go          # 分享管理
│   │   ├── tag.go            # 标签管理
│   │   ├── version.go        # 版本控制
│   │   ├── search.go         # 搜索
│   │   ├── onlyoffice.go     # ONLYOFFICE 集成
│   │   ├── drawio.go         # drawio 集成
│   │   ├── session.go        # 会话管理
│   │   ├── token.go          # API Token
│   │   ├── webhook.go        # Webhook
│   │   ├── notification.go   # 通知
│   │   ├── admin.go          # 管理员功能
│   │   └── system.go         # 系统设置
│   ├── service/              # 业务逻辑层
│   │   ├── auth_service.go
│   │   ├── file_service.go
│   │   ├── permission_service.go
│   │   ├── share_service.go
│   │   ├── onlyoffice_service.go
│   │   ├── search_service.go
│   │   ├── audit_service.go
│   │   ├── notification_service.go
│   │   └── webhook_service.go
│   ├── repository/           # 数据访问层
│   ├── middleware/           # 中间件
│   │   ├── auth.go           # JWT 验证
│   │   ├── casbin.go         # 权限检查
│   │   ├── ratelimit.go      # 限流
│   │   ├── cors.go
│   │   ├── logger.go         # 请求日志
│   │   ├── recovery.go       # panic 恢复
│   │   └── tus.go            # tus 协议
│   ├── model/                # 数据模型
│   ├── pkg/                  # 通用包
│   │   ├── storage/          # S3 客户端
│   │   ├── onlyoffice/       # ONLYOFFICE API
│   │   ├── drawio/           # drawio 集成
│   │   ├── casbin/           # 权限引擎
│   │   ├── jwt/              # JWT 工具
│   │   ├── hash/             # 哈希工具
│   │   ├── tus/              # tus 协议
│   │   ├── watermark/        # 水印生成
│   │   └── i18n/             # 国际化
│   ├── task/                 # 异步任务（asynq）
│   │   ├── thumbnail.go      # 缩略图生成
│   │   ├── watermark.go      # 水印添加
│   │   ├── preview.go        # 预览生成
│   │   ├── cleanup.go        # 清理（回收站、版本、日志）
│   │   └── notification.go   # 异步通知
│   ├── config/               # 配置加载
│   └── server/               # 服务启动
├── migrations/               # 数据库迁移（golang-migrate）
└── docs/                     # Swagger 文档
```

### 4.3 数据流图

#### 4.3.1 文件上传流程

```
[用户拖入文件 / 整个文件夹 / zip 包]
    ↓
[前端] 计算文件 MD5 → 查询后端
    ├─ URL 秒传 → 复用现有 storage_key → 完成
    └─ 不存在 → 启动 tus 分片上传
              ↓
         [后端 tus endpoint] 接收分片 → 暂存 Redis → 推送进度
              ↓
         [所有分片完成] → 异步合并 → 校验 MD5
              ↓
         [异步任务]
              ├─ 上传 SeaweedFS
              ├─ 写 DB（File 记录）
              ├─ 生成缩略图
              └─ 触发 Webhook
              ↓
         [WebSocket] 推送上传完成通知
              ↓
         [前端] Toast 提示成功
```

#### 4.3.2 ONLYOFFICE 编辑流程

```
[用户点击 Word → "编辑"]
    ↓
[前端] → [后端: POST /api/v1/onlyoffice/session]
    ↓
[后端] 验证权限 + 生成 JWT token（含 user_id, file_id）
    ↓
[后端] 调用 ONLYOFFICE API 创建 document session
    ↓
[后端] 返回 ONLYOFFICE editor URL（带 JWT）
    ↓
[前端] 在 iframe 中加载 ONLYOFFICE 编辑器
    ↓
[ONLYOFFICE] 从后端下载文件（GET /api/v1/onlyoffice/download/:fileId）
    ↓
[用户编辑]
    ↓
[ONLYOFFICE 自动保存或用户保存] → 回调: POST /api/v1/onlyoffice/callback
    ↓
[后端] 验证回调 JWT；仅 status=2/6 下载并保存新版本，status=4 仅清理
    ↓
[后端] 上传 SeaweedFS → 创建 FileVersion → 记录 AuditLog
    ↓
[WebSocket] 推送文件更新通知 → 其他协作者刷新
```

#### 4.3.3 公开分享访问流程

```
[访客点开分享链接：https://domain.com/s/abc123]
    ↓
[前端 / 公开页面] → [后端: GET /api/v1/public/shares/abc123]
    ↓
[后端] 校验流程：
    ├─ Token 不存在 → 404
    ├─ 已过期（expires_at < now）→ 410
    ├─ 下载次数超限 → 410
    ├─ 私有分享需登录 → 401
    ├─ 需密码 → 返回密码输入页
    └─ 有效 → 返回文件信息（脱敏）
              ↓
         [前端公开页] 根据类型渲染：
            ├─ Office → ONLYOFFICE Live Viewer
            ├─ PDF → PDF.js
            ├─ 图片 → 图片查看器（含 EXIF）
            ├─ HTML → 沙箱 iframe
            ├─ 网页包 → 沙箱 iframe（支持子资源）
            ├─ 视频/音频 → video.js
            └─ 代码/Markdown → 代码高亮
              ↓
         [可选] 添加水印层（用户邮箱 + 时间 + 文档名）
              ↓
         [异步] 更新 view_count + 记录 AuditLog
```

#### 4.3.4 私有分享访问流程

```
[分享创建] 文件所有者 → 创建私有分享
   ↓
[指定] share_users / share_teams 关联授权
   ↓
[系统] 按当前直接授权用户或团队成员发送站内通知 + 可选邮件
   ↓
[成员登录] 后看到通知 → 点开
   ↓
[前端] → [后端: GET /api/v1/public/shares/abc123]
    ├─ 当前用户不是直授用户且不是当前团队成员 → 403
    └─ 满足关联授权 → 返回文件信息
              ↓
         [渲染预览页]
```

### 4.4 部署模式

#### 4.4.1 Docker Compose 模式（默认）

```yaml
# docker-compose.yml
services:
  caddy:           # 反代 + 自动 HTTPS
  postgres:        # 主数据库
  redis:           # 缓存 + 队列
  seaweedfs:       # 对象存储
  backend:         # Go API
  frontend:        # React 静态资源
  onlyoffice:      # Office 处理
  drawio:          # drawio 编辑器（可选 profile）
```

#### 4.4.2 Profiles

```bash
# 完整版（推荐，含 drawio）
docker compose --profile full up -d

# 极简版（不含 ONLYOFFICE/drawio；默认安全策略仍启用 ClamAV）
docker compose --profile minimal --profile antivirus up -d

# 含 AI（v2.0）
docker compose --profile ai up -d

# 含病毒扫描（v1.0 可选）
docker compose --profile antivirus up -d
```

---

## 5. 技术栈详细说明

### 5.1 后端（Go）

| 类别 | 技术 | 版本 | 说明 |
|---|---|---|---|
| 语言 | Go | 1.22+ | 编译型、强类型、高性能 |
| Web 框架 | Gin | v1.10+ | 生态最丰富，性能优异 |
| ORM | GORM | v2.x | 含 Gen（类型安全代码生成）|
| 数据库 | PostgreSQL | 16+ | 关系型主库 |
| 缓存/队列 | Redis | 7+ | 缓存 + asynq 任务队列 |
| 鉴权 | Casbin | v2.x | RBAC + ABAC 灵活组合 |
| JWT | golang-jwt/jwt | v5 | 业界标准 |
| 对象存储 SDK | aws-sdk-go-v2 | 1.x（由 go.mod 锁定） | 兼容 SeaweedFS/S3/MinIO |
| 文件上传 | tusd | v1.x | tus 协议服务端 |
| 异步任务 | hibiken/asynq | v0.24+ | Go 生态任务队列 |
| WebSocket | gorilla/websocket | v1.5+ | 实时推送 |
| 配置 | spf13/viper | v1.18+ | 多源配置 |
| 日志 | uber-go/zap | v1.26+ | 高性能结构化日志 |
| 日志轮转 | natefinch/lumberjack | v3.x | 日志切割 |
| 验证 | go-playground/validator | v10.x | 请求校验 |
| API 文档 | swaggo/swag | v1.16+ | 自动生成 OpenAPI |
| 国际化 | nicksnyder/go-i18n | v2.x | 多语言 |
| 邮件 | wneessen/go-mail | v0.4+ | SMTP 客户端 |
| 数据库迁移 | golang-migrate | v4.x | 版本化迁移 |
| 病毒扫描 | go-clamav | - | ClamAV 绑定（可选）|
| 测试 | testify + gomock + dockertest | - | 单元 + 集成测试 |
| Markdown | yuin/goldmark | - | Markdown 解析 |
| 缩略图 | disintegration/imaging | - | 图片处理 |

### 5.2 前端（React）

| 类别 | 技术 | 版本 | 说明 |
|---|---|---|---|
| 语言 | TypeScript | 5.4+ | 类型安全 |
| 框架 | React | 18+ | 生态最丰富 |
| 构建 | Vite | 5+ | 快速 HMR + 构建 |
| UI 组件 | shadcn/ui | latest | 可复制、可深度定制 |
| CSS | Tailwind CSS | 3.4+ | 原子化 + 设计令牌 |
| 状态管理 | Zustand | 4+ | 轻量、TS 友好 |
| 路由 | React Router | 6+ | |
| HTTP 客户端 | TanStack Query | 5+ | 缓存 + 乐观更新 |
| 表单 | react-hook-form | 7+ | 高性能表单 |
| 校验 | zod | 3+ | TS-first 校验 |
| 上传 | uppy + @uppy/tus | 4+ | tus 协议 + 进度 + 拖拽 |
| 拖拽 | @dnd-kit | 6+ | 文件夹拖拽移动 |
| 国际化 | react-i18next | 23+ | |
| 图表 | Recharts | 2+ | 仪表盘 |
| 图标 | lucide-react | 由 package-lock.json 锁定 | 现代图标 |
| 通知 | sonner | 1+ | Toast 通知 |
| 弹窗 | Radix UI | 按 lockfile 锁定 | shadcn/ui 底层 |
| 虚拟滚动 | @tanstack/react-virtual | 3+ | 大列表性能 |
| 代码高亮 | shiki | 1+ | VSCode 同款 |
| Markdown | react-markdown + remark-gfm | - | |
| 键盘快捷键 | react-hotkeys-hook | 4+ | |
| PWA | vite-plugin-pwa | 0.20+ | 基础 PWA |
| 测试 | Vitest + React Testing Library | - | 单元测试 |
| E2E | Playwright | - | 关键流程测试 |
| 包管理 | pnpm | 9+ | 节省磁盘 |

### 5.3 文件预览库

| 文件类型 | 预览库 | 集成方式 |
|---|---|---|
| Office（Word/Excel/PPT）| ONLYOFFICE API | iframe |
| PDF | pdf.js（react-pdf）| 直接渲染 |
| 图片 | react-photo-view | 缩放/旋转/全屏 |
| 视频 | video.js | HLS/DASH/MP4 |
| 音频 | wavesurfer.js + react-h5-audio-player | 波形可视化 |
| 代码/Markdown | shiki + react-markdown | 语法高亮 |
| HTML（单文件）| iframe sandbox | 沙箱化 |
| 网页包（zip/tar/目录）| iframe + 反代 | 沙箱化 + 子资源代理 |
| drawio | drawio embed（@drawio/drawio-integration）| iframe |
| Excalidraw | @excalidraw/excalidraw | React 组件 |
| 3D 模型 | three.js + @react-three/fiber | OBJ/GLTF |
| 压缩包 | jszip + fflate | 树形浏览 + 内预览 |
| 字体 | fontkit | 字体预览 |
| 邮件 (.eml) | mailparser + 自实现 | 渲染预览 |

### 5.4 中间件

| 服务 | 版本 | 最低配置 | 必要性 | 说明 |
|---|---|---|---|---|
| PostgreSQL | 16 | 256MB | 必需 | 主数据库 |
| Redis | 7 | 128MB | 必需 | 缓存 + 队列 |
| **SeaweedFS** | 3.80 | 256MB | 必需 | S3 对象存储 |
| ONLYOFFICE Document Server | **8.2.3（锁定）** | 2GB | 必需 | Office 处理，详见 16.5 风险 4 |
| drawio (jgraph/drawio) | 27.0.9 | 512MB | full profile | 图表编辑器 |
| Meilisearch | v1.6+ | 256MB | v2.0 | 全文搜索 |
| ClamAV | 1.4.1 | 512MB | antivirus profile | 病毒扫描；默认扫描通过才可用 |

### 5.5 推荐硬件配置

> ⚠️ **配置调整**：由于 ONLYOFFICE Document Server 至少需要 2GB RAM（详见 16.5 风险 1），已将推荐配置整体上调。"极小"配置仅供个人开发测试，不推荐生产使用。

| 规模 | CPU | RAM | 磁盘 | 用户数 | 备注 |
|---|---|---|---|---|---|
| **开发/测试** | 2 核 | 4 GB | 50 GB SSD | 仅个人 | 仅适合本地开发，不含 ONLYOFFICE |
| **极小**（个人）| 4 核 | 8 GB | 100 GB SSD | < 20 用户 | 包含 ONLYOFFICE，**生产起步** |
| **小**（推荐）| 8 核 | 16 GB | 500 GB SSD | 20-100 用户 | **推荐生产配置**，含 Meilisearch |
| **中** | 16 核 | 32 GB | 2 TB SSD | 100-500 用户 | 高性能配置，集群化部署 |
| **大** | 32 核 | 64 GB | 10 TB SSD | 500+ 用户 | K8s 多实例 + 读写分离 |

### 5.6 关键设计原则

#### 5.6.1 外部状态与横向扩展
- 应用实例无本地持久状态；PostgreSQL 保存元数据、会话和权限，Redis 提供缓存，队列保存异步任务，S3/SeaweedFS 保存对象，WebSocket 连接/广播由外部消息机制协调
- 临时上传分片使用专用临时磁盘或 S3 multipart，不得存 Redis
- 任意实例可处理请求，扩展必须共享上述外部状态服务，不依赖本地文件、内存会话或粘性会话
- 外部依赖不可用时由 readiness 拒绝新流量；任务和回调必须幂等

#### 5.6.2 固定根目录与层级
- 系统为每个用户和团队创建不可删除根文件夹：File.type=folder、is_root=true、scope_type=personal/team，并关联 owner/team
- 所有 File 的 parent_id 非空，根节点也以自身归属表示；启用 PostgreSQL ltree，tree_path 仅作查询，parent_id 为权威关系，名称不进入 tree_path

#### 5.6.3 基础设施解耦
- 代码层面**不针对规模做特殊优化**
- 规模化靠基础设施：SeaweedFS 分布式、PostgreSQL 主从（v2.0）、多后端实例 + 负载均衡

#### 5.6.4 配置驱动
- 所有可调参数通过 `.env` 配置
- 提供 `config.example.yaml` 示例
- 关键参数有合理默认值

#### 5.6.5 前端按需加载（详见 16.5 风险 3）
- **路由级 code splitting**：每个页面独立 chunk
- **大库懒加载**（关键！）：
  - PDF.js（react-pdf）—— 仅在 PDF 预览页加载
  - ONLYOFFICE SDK —— 仅在 Office 编辑页加载
  - Excalidraw —— 仅在 Excalidraw 编辑页加载
  - drawio viewer —— 仅在 drawio 预览/编辑页加载
  - Monaco Editor —— 仅在代码编辑器页加载
  - video.js / wavesurfer.js —— 仅在音视频页加载
- **首屏优化**：landing/login 页面 bundle < 300KB gzip
- **运行时优化**：使用 dynamic import + Suspense 包裹
- **监控**：通过 Vite 的 `build --report` 验证 chunk 分布

---

## 6. 功能规格说明

### 6.1 用户与认证

#### 6.1.1 用户注册
**v1.0：仅管理员邀请制**
- 管理员创建用户 → 系统生成临时密码或激活链接
- 用户首次登录 → 强制修改密码
- 配置项：`ALLOW_SELF_REGISTER=false`（默认）/ `true`（v2.0 自注册）

**v2.0：自注册（可选启用）**
- 邮箱 + 密码注册
- 邮箱验证（可选 SMTP）
- 配置项：`ALLOW_SELF_REGISTER=true` + `REQUIRE_EMAIL_VERIFICATION=true`

#### 6.1.2 用户档案字段
所有字段均为可选，无值时：
- 头像：基于用户名首字母自动生成（4 种背景色随机）
- 昵称：默认等于 username
- 部门/职位/电话/简介：留空

```yaml
必填:
  - username
  - email
  - password
可选（无值用默认）:
  - nickname      # 默认 = username
  - avatar        # 自动生成
  - department
  - position
  - phone
  - bio
系统字段:
  - storage_quota  # 默认 10GB
  - language       # 默认 zh-CN
  - timezone       # 默认 UTC
```

#### 6.1.3 登录
- **输入**：username/email + password
- **流程**：
  1. 查询用户（按 username 或 email）
  2. 校验密码（bcrypt cost=12）
  3. 失败 → `failed_login_count++`
     - `failed_login_count >= 5` → 锁定 15 分钟
  4. 成功 → 重置失败计数
  5. 创建 Session（写 DB）
  6. 颁发 JWT（Access Token 15 分钟 + Refresh Token 7 天）
  7. 更新 `last_login_at`、`last_login_ip`
  8. 记录 AuditLog

#### 6.1.4 JWT 设计
```json
AccessToken Payload:
{
  "user_id": "uuid",
  "username": "alice",
  "role": "editor",        // 主角色
  "teams": ["design", "pm"], // 所属团队
  "type": "access",
  "iat": 1234567890,
  "exp": 1234568790        // 15 分钟后
}

RefreshToken Payload:
{
  "user_id": "uuid",
  "session_id": "uuid",    // 关联 Session 表
  "type": "refresh",
  "iat": 1234567890,
  "exp": 1235172690        // 7d 后
}
```

- Access Token 仅存前端内存，TTL 固定 15 分钟；Refresh Token 为随机不透明 token，仅通过 HttpOnly、Secure、SameSite=Lax Cookie 发送
- Cookie Path 限定为 `/auth/refresh`；数据库仅保存 Refresh Token 哈希
- 每次刷新执行 rotation；检测到重放后撤销整个 Session/token family
- Refresh Token 对应的 Session 标记 `revoked_at = NOW()` 后不可继续刷新

#### 6.1.5 登出与 CSRF

- 登出撤销 Session/token family；Access Token 依靠 15 分钟短 TTL 自然失效，不使用 Redis 黑名单。
- Refresh Cookie 写接口及其他 cookie 认证写接口同时校验 Origin/Referer 与 CSRF token；CSRF token 通过安全方式提供并随请求显式提交。
- Redis 仅用于缓存、队列和广播；其故障由 readiness/失败关闭策略处理，不承担 Access Token 撤销。

### 6.2 用户与团队管理

#### 6.2.1 用户管理（管理员）
- 增删改查
- 禁用 / 启用
- 重置密码
- 搜索（按 username/email/nickname）
- 分页、排序
- 调整存储配额

#### 6.2.2 团队管理（原 Group）
- **命名**：统一使用"团队（Team）"
- 创建 / 删除 / 重命名
- 添加 / 移除成员
- 设置成员在团队内的角色（owner / admin / member）
- 团队级权限（统一授权）

### 6.3 文件管理

#### 6.3.1 上传
- tus 仅负责协议层断点续传；分片进入专用临时磁盘或 S3 multipart，upload_sessions 记录 tus_id、状态、TTL、重试次数，不存 Redis
- 完成后服务端校验授权、配额、SHA-256、大小、魔数和文件名；状态为 uploading → verifying → scanning → available 或 quarantined/failed
- 只有 available ObjectBlob 可秒传，且每次重新校验授权与配额；对象成功而 DB 失败由 outbox/补偿任务收敛，完成接口幂等
- ClamAV 默认扫描通过才可用；verifying/scanning 期间禁止预览、下载、分享、ONLYOFFICE。管理员可在隔离区查看、重扫或删除；开发环境可配置禁用扫描
- 上传完成后异步生成缩略图/预览并发出通知；tus 显示真实上传进度，处理阶段显示状态，不伪造精确扫描百分比
- upload_sessions、临时分片和失败任务按 TTL 清理并支持有限次重试

#### 6.3.2 下载
- **方式**：流式下载（不占用磁盘）
- **断点续传**：HTTP Range 支持
- **限速**：可配 `DOWNLOAD_RATE_LIMIT`（默认不限速）
- **日志**：记录到 AuditLog（含 user_id、ip、file_id、size）

#### 6.3.3 删除
- **软删除** → 进入回收站
- **撤销**：UI 层支持（删除后 5 秒内可撤销 Toast）
- **回收站**：
  - 保留 30 天（`TRASH_RETENTION_DAYS` 可配）
  - 列出 / 恢复 / 彻底删除
  - 过期自动清理（定时任务）
- **文件夹递归删除**

#### 6.3.4 目录树
- 无限层级（PostgreSQL 递归 CTE）
- 路径表达式：`/产品文档/v2.0/PRD.docx`
- 拖拽移动 / 重命名
- 显示子文件数量、总大小
- 面包屑导航

#### 6.3.5 文件预览
按文件类型自动选择预览器（见 5.3 表格）

#### 6.3.6 静态网页包
- **支持形式**：
  - 单个 HTML 文件（带 CSS/JS/图片）
  - 上传 zip / tar / tar.gz 压缩包 → 自动解压为目录
  - 上传整个文件夹（保留结构）
  - 已存在的文件夹（手动管理）
- **入口识别**：自动识别 `index.html`
- **沙箱渲染**：
  - iframe sandbox 属性
  - CSP 头限制
  - 子资源代理（防 CORS）
- **路径映射**：内容域 `https://content.<domain>/share/{token}/path/to/file.html`，主站页面仍为 `/s/{token}`
- **安全边界**：独立 origin，不发送主站 Cookie/认证头；默认 iframe sandbox 不含 `allow-same-origin`，资源仅从内容域服务
- **响应头**：严格 CSP（默认禁止外联网络，仅允许内容域必要子资源）、`X-Content-Type-Options: nosniff`、按类型设置 `Content-Disposition`；主站与内容域分别配置 CORS/Cookie，内容域不设置主站可用 Cookie
- **压缩包防护**：拒绝 Zip Slip、绝对路径和符号链接；限制条目数、展开总大小、单文件大小和目录深度，限制或禁止外部网络请求
- 单包最大 500 MB（`WEBPAGE_MAX_SIZE` 可配）

#### 6.3.7 视图切换
- **列表视图**：表格形式（图标 + 名称 + 大小 + 修改时间 + 操作）
- **网格视图**：卡片形式（缩略图 + 名称）
- 每个用户偏好保存到 LocalStorage
- 全局快捷键切换：`Cmd/Ctrl + 1`（列表）/`Cmd/Ctrl + 2`（网格）

#### 6.3.8 排序
- 按名称（默认）
- 按大小
- 按修改时间
- 按创建时间
- 按类型
- 升降序切换
- 排序偏好保存

#### 6.3.9 拖拽
- **拖拽上传**：从桌面拖入浏览器（支持文件夹）
- **拖拽移动**：在文件树内拖拽文件/文件夹到目标位置
- **拖拽到侧边栏**：移动到常用文件夹
- 多选拖拽支持

#### 6.3.10 右键菜单
- 单文件 / 多文件 / 文件夹均支持
- 菜单项：
  - 打开 / 预览
  - 在 ONLYOFFICE 中编辑
  - 下载
  - 分享
  - 重命名
  - 移动
  - 复制
  - 删除
  - 查看版本
  - 查看详情
  - 复制链接（私有分享时）

#### 6.3.11 快捷访问面板
侧边栏 4 个模块：
- **📁 我的文件**：所有有权限的文件
- **⭐ 收藏**：`is_starred=true` 的文件
- **🕐 最近访问**：按 `last_access_at` 排序（保留 50 个）
- **🔗 我分享的**：当前用户创建的分享

#### 6.3.12 撤销操作
- 删除后弹出 Toast："已删除 [文件名]  [撤销]"
- 5 秒内可点击撤销（恢复文件）
- 超过 5 秒后只能从回收站恢复

### 6.4 批量操作

支持的批量动作：
- ✅ 批量上传（多文件、整个文件夹）
- ✅ 批量下载（打包为 zip 流式下载）
- ✅ 批量删除
- ✅ 批量移动（拖拽到目标文件夹）
- ✅ 批量分享
- ✅ 批量打标签
- ✅ 批量添加/移除收藏

实现：
- 多选：`Shift + 点击`、`Cmd/Ctrl + 点击`
- 全选：`Cmd/Ctrl + A`
- 顶部操作栏根据选择数量动态显示

### 6.5 权限管理

#### 6.5.1 预置角色

| 角色 | 权限范围 |
|---|---|
| **super_admin** | 全部权限，包括系统设置、用户管理 |
| **admin** | 用户/团队/文件管理，无系统级设置 |
| **editor** | 文件 CRUD、分享、版本控制 |
| **viewer** | 只读（预览、下载） |

#### 6.5.2 自定义角色（v1.0 支持）
- 管理员可创建自定义角色
- 角色包含细粒度权限勾选（read/write/delete/share/admin 等）

#### 6.5.3 权限继承
- 文件夹权限 → 子文件/子文件夹继承
- 子项可独立覆盖（更高优先级）

#### 6.5.4 路径权限
- 支持通配符：`/财务部/*`、`/projects/*/design/*`
- 支持反选：`! /private/*`

#### 6.5.5 谁能分享
- 文件/文件夹的所有者
- 被授予 `share` 权限的用户/团队
- 管理员（admin / super_admin）

### 6.6 分享管理

#### 6.6.1 两种分享模式

**模式 1：公开分享（`type='public'`）**
- 生成短链：`https://domain.com/s/{8位token}`
- 可配置：密码、过期、下载限制、水印
- 任何人拿到链接可访问（不需登录）

**模式 2：私有分享（`type='private'`）**
- 通过 `share_users`、`share_teams` 关联表授权指定用户或团队
- 受邀用户需登录后才能访问；团队成员变化立即生效
- 可附加密码、过期等设置；撤销分享或文件删除/隔离/权限撤销立即阻断访问

#### 6.6.2 分享配置项

| 项 | 公开分享 | 私有分享 |
|---|---|---|
| 密码 | 可选 | 可选（额外保护）|
| 过期时间 | 可选 | 可选 |
| 下载次数限制 | 可选 | 可选 |
| 允许下载 | ✅ | ✅ |
| 水印 | 默认开启 | 默认开启 |
| 自定义水印文本 | 可选 | 可选 |

#### 6.6.3 分享统计
- 总访问次数 / 唯一访客数（按 IP 去重）
- 最近访问记录（IP、时间、UA）
- 下载次数
- 分享页底部："Powered by DocFlow"（可关闭）

#### 6.6.4 反爬虫
- API 限流：100 req/min/IP（`RATE_LIMIT` 可配）
- 分享 token 熵：8 位 base62（约 47 亿种组合）
- 可选：CAPTCHA 验证（v2.0）

### 6.7 ONLYOFFICE 集成

#### 6.7.1 支持的操作

| 操作 | 支持 |
|---|---|
| 查看 Word/Excel/PPT | ✅ |
| 编辑 Word/Excel/PPT | ✅ |
| 新建 Word/Excel/PPT（空白）| ✅ |
| PDF 查看 | ✅ |
| PDF 编辑 | ✅（v2.0 增强）|
| 实时协作 | ✅（ONLYOFFICE 自带）|
| 版本历史 | ✅（保存到 DocFlow）|

#### 6.7.2 配置

```yaml
ONLYOFFICE_URL: http://onlyoffice:80
ONLYOFFICE_JWT_SECRET: <random-32-bytes>
ONLYOFFICE_HTTPS: false  # 内网可关
```

#### 6.7.3 启动会话

后端返回由 ONLYOFFICE_JWT_SECRET 签名的 DocEditor 配置：

```json
{
  "document": {
    "fileType": "docx",
    "key": "{file_id}_v{version}",
    "title": "PRD.docx",
    "url": "https://docflow/api/v1/onlyoffice/download/{file_id}"
  },
  "editorConfig": {
    "callbackUrl": "https://docflow/api/v1/onlyoffice/callback",
    "user": { "id": "user_id", "name": "username" }
  },
  "token": "<signed-jwt>"
}
```

前端将该配置传给 ONLYOFFICE DocEditor；编辑会话由 DocEditor 配置 API 建立。

#### 6.7.4 回调处理

```http
POST /api/v1/onlyoffice/callback

{
  "status": 2,           // 1=编辑中 2=保存 3=错误 4=关闭 6=强制保存 7=强制保存错误
  "url": "https://onlyoffice/...",  // 新版本下载URL
  "key": "{file_id}_v{version}",
  "users": ["alice", "bob"]
}
```

处理逻辑：
1. 验证 JWT
2. 从 ONLYOFFICE 下载新版本文件
3. 校验关联编辑会话、下载 URL 的 scheme/host allowlist，并计算 SHA-256
4. 上传内容寻址的 ObjectBlob
5. 幂等写 FileVersion 表（唯一键：file_id、document_key、callback_version）
6. 更新 File.current_version_id 与元数据
7. 记录 AuditLog
8. 推送通知

### 6.8 drawIO 集成

#### 6.8.1 部署模式

**模式 1：Docker 依赖（推荐）**
- 启动 `jgraph/drawio` Docker 镜像
- 前端通过 iframe URL 嵌入
- 完整功能（OAuth、字体、导出）

**模式 2：轻量嵌入（不依赖 Docker）**
- 前端引入 `https://embed.diagrams.net/js/embed.min.js`
- 通过 URL 参数配置
- 功能受限但无需后端服务

#### 6.8.2 与 DocFlow 集成

```
[前端 React]
   └─ 打开 drawio 编辑器
        ├─ URL: https://draw.docflow.example.com/?offline=1&url=...
        ├─ 加载文件：通过 DocFlow API 下载 .drawio XML
        ├─ 保存：postMessage('save', xml) → DocFlow API 上传
        └─ 导出：用户可在 drawio 内导出 PNG/SVG/PDF，再上传到 DocFlow
```

#### 6.8.3 文件存储
- 保存为 `.drawio` XML 格式（可二次编辑）
- 导出格式：PNG、SVG、PDF（只读快照）

### 6.9 Excalidraw 集成

#### 6.9.1 集成方式
- npm 包：`@excalidraw/excalidraw`
- 直接作为 React 组件嵌入前端
- **无需额外服务**（所有数据存前端或后端）

#### 6.9.2 保存机制
```typescript
<Excalidraw
  initialData={loadFromAPI(fileId)}
  onChange={(elements) => debouncedSave(fileId, elements, 2000)}
/>
```
- 防抖保存（2 秒）
- 保存为 `.excalidraw` JSON 格式
- 也可导出 PNG、SVG

### 6.10 搜索

#### 6.10.1 v1.0 文件名搜索
- 数据库 LIKE 查询
- 支持中英模糊匹配
- 性能：< 1s（百万级数据）

#### 6.10.2 v2.0 全文搜索
- Meilisearch 全文索引
- 文件内容抽取（PDF、Office、文本、Markdown）

#### 6.10.3 搜索过滤
- 类型（文件/文件夹/Office/PDF/图片/视频/音频/压缩包/网页包/图表）
- 时间范围（今天/7天/30天/自定义）
- 作者（owner_id）
- 标签
- 路径（递归）

### 6.11 标签

- 创建 / 删除标签（仅创建者可删，全局可用）
- 给文件打标签（多对多）
- 按标签筛选
- 标签云可视化（v2.0）

### 6.12 通知

#### 6.12.1 站内信（必做）
- 通知列表（已读/未读）
- 通知设置（可关闭某些类型）
- 实时推送（WebSocket）

#### 6.12.2 邮件（可选）
- SMTP 配置（`SMTP_HOST`、`SMTP_PORT`、`SMTP_USER`、`SMTP_PASSWORD`）
- 通知模板（HTML）
- 异步发送（asynq 队列）

#### 6.12.3 Webhook（可选）
- 事件订阅：文件创建/更新/删除/分享/权限变更
- HMAC-SHA256 签名验证
- 重试机制：指数退避（1min、5min、30min、2h、12h）

#### 6.12.4 触发事件

| 事件 | 默认通知 |
|---|---|
| 文件被分享给我 | ✅ 站内 + 邮件 |
| 文件被修改 | ✅ 站内 |
| 文件被删除 | ✅ 站内 |
| 团队邀请 | ✅ 站内 + 邮件 |
| 权限变更 | ✅ 站内 |
| 存储配额警告（>80%）| ✅ 站内 |

### 6.13 版本控制

- 每次覆盖上传 → 创建新版本
- ONLYOFFICE 自动保存 → 创建新版本
- 默认保留最新 5 个版本（`MAX_VERSIONS_PER_FILE` 可配）
- 可手动删除任意版本
- 可回滚到任意版本（创建为当前版本）
- 自动清理策略：
  - 保留最新 N 个
  - 或保留最近 30 天的
  - 或两者结合

### 6.14 回收站

- 软删除文件保留 30 天（`TRASH_RETENTION_DAYS` 可配）
- 列出所有已删除项
- 单个恢复 / 批量恢复
- 单个永久删除 / 批量永久删除
- 30 天后自动清理（定时任务，每日凌晨执行）

### 6.15 审计日志

- 记录所有关键操作：
  - 登录 / 登出
  - 上传 / 下载 / 删除 / 重命名 / 移动
  - 分享创建 / 访问
  - 权限变更
  - 用户/团队管理
- 字段：user_id、action、resource_type、resource_id、ip、user_agent、status、metadata
- 保留 90 天（`LOG_RETENTION_DAYS` 可配）
- 支持查询、导出（CSV）、归档

### 6.16 快捷键

#### 6.16.1 全局

| 快捷键 | 动作 |
|---|---|
| `Cmd/Ctrl + K` | 快速搜索 |
| `Cmd/Ctrl + U` | 上传文件 |
| `Cmd/Ctrl + N` | 新建文件/文件夹 |
| `Cmd/Ctrl + 1` | 切换列表视图 |
| `Cmd/Ctrl + 2` | 切换网格视图 |
| `Esc` | 关闭弹窗/取消选择 |
| `F11` | 切换全屏 |

#### 6.16.2 文件浏览

| 快捷键 | 动作 |
|---|---|
| `↑ / ↓ / ← / →` | 切换选择 |
| `Space` | 预览文件 |
| `Enter` | 打开文件 |
| `Delete` | 删除选中（带撤销提示）|
| `Cmd/Ctrl + A` | 全选 |
| `Cmd/Ctrl + Click` | 多选 |
| `Shift + Click` | 范围选择 |
| `F2` | 重命名 |

#### 6.16.3 预览页

| 快捷键 | 动作 |
|---|---|
| `← / →` | 上一个/下一个（图片预览）|
| `+ / -` | 缩放 |
| `0` | 适应窗口 |
| `F` | 切换全屏 |

### 6.17 系统设置（管理员）

- 设置页路由为 `/settings`，仅 `super_admin` 可读写；所有保存操作记录 `system_settings` 更新人与审计事件，并通过 `/api/v1/admin/settings` 返回配置值、类型、校验规则与生效方式。
- 页面按“站点、用户、上传与存储、安全与分享、集成、备份恢复”分组。保存前执行类型、范围和依赖校验；界面明确标识“即时生效”“新会话生效”或“需重启”。
- 非密钥运行参数保存在 `system_settings`，由启动环境变量提供默认值；首次管理员保存后以数据库值覆盖默认值。密钥、连接串、对象存储凭据、JWT/ONLYOFFICE 密钥和 SMTP 密码只能由部署 Secret 或受保护的 `.env` 注入，页面仅显示“已配置”状态且不得读取、回显或修改明文。

```yaml
站点设置（即时生效）:
  - 站点名称、Logo、Favicon、主题色、默认语言、时区

用户策略（新会话生效）:
  - ALLOW_SELF_REGISTER、默认存储配额、密码策略

上传与存储（即时生效；进行中的上传沿用创建时快照）:
  - MAX_FILE_SIZE、TUS_CHUNK_SIZE、MAX_VERSIONS_PER_FILE
  - 文件类型黑/白名单、TRASH_RETENTION_DAYS、LOG_RETENTION_DAYS
  - 并行上传、文件夹深度与批处理上限

安全与分享（即时生效）:
  - 登录失败阈值、锁定时间、API/分享限流
  - 默认水印与模板、公开分享开关、默认过期策略
  - 扫描启用状态与隔离区保留期（生产环境不得在页面关闭扫描）

集成（Secret 仅显示状态；变更连接地址需重启）:
  - ONLYOFFICE、drawio、SMTP、Webhook 启用状态与非密钥地址/策略

备份与恢复（即时生效；恢复须二次确认）:
  - 自动备份开关、保留天数、立即备份、备份历史与恢复演练记录
```

### 6.18 PWA 基础支持

- **Service Worker**：缓存关键资源（HTML、CSS、JS、字体）
- **Web App Manifest**：可安装到桌面
- **离线降级**：断网时显示友好提示
- **不投入优化**：不做离线编辑、后台同步

### 6.19 容量与并发限制

| 配置项 | 默认值 | 约束与说明 |
|---|---:|---|
| 单文件大小 | 2 GB | `MAX_FILE_SIZE`，仅管理员可调整，受对象存储能力限制。 |
| tus 分片大小 | 10 MB | `TUS_CHUNK_SIZE`；分片落专用临时磁盘或 S3 multipart。 |
| 单用户并行上传 | 3 | `MAX_CONCURRENT_UPLOADS_PER_USER`，超出返回 429。 |
| 系统并行上传 | 50 | `MAX_CONCURRENT_UPLOADS_SYSTEM`，按部署资源调节。 |
| 单次批量操作 | 100 项 | `BATCH_OPERATION_MAX_ITEMS`，必须使用幂等键并异步执行。 |
| 文件夹最大深度 | 50 层 | `MAX_FOLDER_DEPTH`，创建或移动时校验。 |
| 目录单页大小 | 100 | `API_MAX_LIMIT`；默认 cursor 分页，管理页 offset 必须受限。 |

### 6.20 访问统计、可访问性与浏览器支持

- 访问统计仅信任配置的反向代理传入的客户端地址；落库使用带轮换盐的 IP 哈希或脱敏前缀，不保存原始 IP。`file_access_events` 默认保留 90 天，汇总统计可按更长保留策略保存。
- 界面遵循 WCAG 2.1 AA：键盘可达、可见焦点、语义化标签、错误提示可被辅助技术读取，文字与关键控件满足对比度要求。
- 支持当前及前一主要版本的 Chrome、Edge、Firefox、Safari；不支持 Internet Explorer。上传、预览与编辑能力需在目标浏览器 E2E 验证。

---

## 7. 权限与安全设计

### 7.1 认证架构

```
┌──────────┐                      ┌──────────┐
│ Browser  │ ─── login ────────> │ Backend  │
│          │ <── access+refresh ─ │          │
│          │                      │          │
│          │ ─── API + Bearer ───>│          │
│          │ <── response ────────│          │
└──────────┘                      └──────────┘
```

### 7.2 授权模型（RBAC + ABAC）

#### 7.2.1 资源类型
- `file:{uuid}` - 单个文件
- `folder:{uuid}` - 单个文件夹
- `/path/to/folder/*` - 路径通配
- `*` - 全局

#### 7.2.2 动作
- `read` - 读（预览、下载、列出）
- `write` - 写（上传、重命名、移动、编辑）
- `delete` - 删除
- `share` - 生成分享
- `admin` - 管理（权限授予）

#### 7.2.3 Casbin 策略示例

```csv
# 系统策略
p, super_admin, *, *, allow
p, role:admin, *, admin, allow

# 用户策略
p, alice, file:UUID001, read, allow
p, alice, folder:UUID002, write, allow

# 团队策略
p, team:design, folder:UUID002, write, allow
p, team:design, folder:UUID002, share, allow

# 角色绑定
g, alice, design
g, bob, design
g, design, role:editor
```

#### 7.2.4 权限判定优先级

| 顺序 | 规则 |
|---|---|
| 1 | `super_admin` 在系统未禁用资源的前提下可管理全部资源；已删除或隔离对象仍不得被预览、下载、分享或编辑。 |
| 2 | 显式 `deny` 优先于任何 `allow`。 |
| 3 | 资源自身规则优先于从父目录继承的规则。 |
| 4 | 用户直授与其当前团队的 `allow` 取并集；团队成员变化立即重新判定。 |
| 5 | 公开分享是独立的只读访问授权边界，但不能绕过文件删除、隔离、系统禁用或分享撤销。 |

**设计决策**：以 `parent_id` 与资源 ID 判定继承，不以名称路径作为授权对象；私有分享通过 `share_users`、`share_teams` 关联表实时求值。

### 7.3 安全防护清单

| 防护 | 实现 |
|---|---|
| SQL 注入 | GORM 参数化查询 |
| XSS | CSP 头 + React 自动转义 + 输出编码 |
| CSRF | Refresh Cookie 使用 SameSite=Lax + Origin/Referer 校验 + CSRF token |
| 暴力破解 | 5 次失败锁 15 分钟 |
| 路径越权 | parent_id/tree_path 校验 + Casbin 权限检查 |
| 文件上传校验 | MIME + 扩展名 + 文件头魔数 |
| 病毒扫描 | 默认扫描通过才可用，开发环境可禁用 |
| API 限流 | Redis 滑动窗口（按 IP + 按用户）|
| HTML 沙箱 | iframe sandbox + CSP |
| 数据导出安全 | 用户导出需二次验证（密码确认）|

### 7.4 数据安全

| 项 | 措施 |
|---|---|
| 传输加密 | HTTPS（推荐反代配置，应用层可走 HTTP）|
| 密码存储 | bcrypt cost=12 |
| JWT 签名 | HS256 + 强密钥（仅由环境变量/secret 注入）|
| Access Token | 仅存前端内存，TTL 15 分钟；依靠短 TTL 自然失效，不使用 Redis 黑名单 |
| Refresh Token | 随机不透明 token，仅存哈希（Session 表），HttpOnly/Secure/SameSite=Lax Cookie，Path=/auth/refresh，TTL 7d，刷新 rotation |
| Redis 可用性 | 用于缓存、队列和广播；故障时由 readiness 拒绝依赖服务，不承担 Access Token 撤销 |
| 存储加密 | SeaweedFS SSE（可选）|
| 数据库加密 | 透明数据加密（TDE，可选，v2.0）|
| 备份加密 | GPG（文档说明）|
| 密钥管理 | HashiCorp Vault 集成（v2.0，可选）|
| 密钥轮换 | 支持运行时 JWT 密钥轮换（双密钥过渡期）|

### 7.5 审计日志

- 所有写操作（上传/删除/修改/权限变更）
- 所有认证操作（登录/登出/失败）
- 所有管理操作（用户/团队/角色）
- 保留 90 天（可配）
- 可导出 CSV
- 异步写入（避免影响主流程）

---

## 8. 会话管理与 API Token

### 8.1 会话管理（多端登录）

#### 8.1.1 功能列表

| 功能 | 说明 |
|---|---|
| 查看活跃会话 | 用户可在"账户设置 → 会话"看到所有登录设备 |
| 设备信息 | IP、地理位置（可选）、设备类型、User-Agent、最后活跃时间 |
| 标记当前 | 当前会话高亮显示 |
| 远程登出 | 可撤销任意会话（除当前）|
| 一键全部登出 | 撤销除当前外的所有会话 |

#### 8.1.2 实现

```yaml
会话存储: sessions 表（DB）
会话识别: 通过 RefreshToken 关联
会话刷新: 每次使用 RefreshToken 刷新时，更新 last_active_at
会话撤销: 设置 revoked_at = now()
会话清理: 定时任务清理已撤销超过 7 天的会话
```

#### 8.1.3 API

```http
GET /api/v1/sessions            # 获取当前用户所有活跃会话
DELETE /api/v1/sessions/:id        # 撤销指定会话
DELETE /api/v1/sessions            # 撤销除当前外的所有会话
```

### 8.2 API Token（个人访问令牌 PAT）

#### 8.2.1 用途
- 外部脚本/CI/CD 集成
- Webhook 接收端验证
- 第三方应用对接（v2.0）

#### 8.2.2 设计

```yaml
格式: docflow_pat_<base62_random_32_chars>
示例: docflow_pat_a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6
存储: SHA256(token) → api_tokens.token_hash
展示: 前缀 docflow_pat_a1b2c3d4...（前 16 位 + 省略号）
```

#### 8.2.3 Scopes

```yaml
files:read       # 读取文件
files:write      # 创建/更新/删除文件
shares:create    # 创建分享
shares:manage    # 管理分享
tags:manage      # 管理标签
webhooks:manage  # 管理 Webhook
admin:users      # 用户管理（仅 admin）
*                # 全部权限（慎重）
```

#### 8.2.4 API

```http
GET    /api/v1/tokens                    # 列出当前用户的 Token
POST   /api/v1/tokens                    # 创建 Token（返回明文，仅一次）
DELETE /api/v1/tokens/:id                # 撤销 Token
PATCH  /api/v1/tokens/:id                # 修改名称/Scopes
```

#### 8.2.5 使用方式

```http
GET /api/v1/files
Authorization: Bearer docflow_pat_a1b2c3d4e5f6g7h8...
```

---

## 9. API 设计规范

### 9.1 RESTful 约定

**设计决策**：OpenAPI 是唯一 API 契约；所有业务接口使用 `/api/v1`，公开分享接口为 `/api/v1/public/shares/:token`，前端页面为 `/s/:token`。默认 cursor 分页，管理页仅可使用受限 offset；`limit`、稳定排序及排序字段白名单由 OpenAPI 定义。写操作、上传完成、创建分享、批处理和 ONLYOFFICE 回调使用幂等键；文件元数据更新使用 ETag/If-Match，冲突返回 409，Range 下载成功返回 206。

#### 9.1.1 URL 设计

```
/api/v1/auth/login                  POST   登录
/api/v1/auth/register               POST   注册（v2）
/api/v1/auth/logout                 POST   登出
/api/v1/auth/refresh                POST   刷新令牌
/api/v1/auth/forgot-password        POST   忘记密码

/api/v1/users                       GET/POST/GET/:id/PATCH/:id/DELETE/:id
/api/v1/teams                       GET/POST/GET/:id/PATCH/:id/DELETE/:id
/api/v1/teams/:id/members           GET/POST/DELETE
/api/v1/files                       GET/POST
/api/v1/files/:id                   GET/PATCH/DELETE
/api/v1/files/:id/download          GET
/api/v1/files/:id/preview           GET
/api/v1/files/:id/star              POST
/api/v1/files/:id/tags              GET/POST/DELETE
/api/v1/folders                     POST
/api/v1/folders/:id                 PATCH/DELETE
/api/v1/files/:id/versions          GET/POST
/api/v1/files/:id/versions/:v       GET/DELETE
/api/v1/files/:id/restore/:v        POST
/api/v1/shares                      GET/POST
/api/v1/shares/:id                  GET/PATCH/DELETE
/api/v1/public/shares/:token        GET    公开访问分享（无需认证）
/api/v1/tags                        GET/POST/DELETE
/api/v1/search?q=                   GET
/api/v1/notifications               GET
/api/v1/notifications/:id/read      POST
/api/v1/sessions                    GET/DELETE
/api/v1/tokens                      GET/POST/DELETE
/api/v1/webhooks                    GET/POST/DELETE
/api/v1/onlyoffice/session          POST   返回签名 DocEditor 配置
/api/v1/onlyoffice/callback         POST   ONLYOFFICE 回调
/api/v1/onlyoffice/download/:id     GET    ONLYOFFICE 下载文件
/api/v1/admin/settings              GET/PATCH
/api/v1/admin/stats                 GET
/health                              GET    健康检查
/ready                               GET    就绪检查
/metrics                             GET    Prometheus 指标
```

#### 9.1.2 HTTP 状态码

| 状态码 | 用途 |
|---|---|
| 200 | 成功 |
| 201 | 创建成功 |
| 204 | 删除成功（无返回体）|
| 400 | 请求参数错误 |
| 401 | 未认证 |
| 403 | 无权限 |
| 404 | 资源不存在 |
| 409 | 资源冲突 |
| 410 | 资源已过期 |
| 413 | 文件过大 |
| 429 | 限流 |
| 500 | 服务器错误 |

### 9.2 响应格式

#### 9.2.1 成功响应

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "id": "uuid",
    "name": "PRD.docx"
  }
}
```

#### 9.2.2 分页响应

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "items": [],
    "total": 100,
    "page": 1,
    "page_size": 20
  }
}
```

#### 9.2.3 错误响应

```json
{
  "code": 1003,
  "message": "Permission denied",
  "details": {
    "resource": "file:UUID001",
    "action": "delete"
  }
}
```

### 9.3 错误码

| 范围 | 类别 |
|---|---|
| 0 | 成功 |
| 1xxx | 认证授权（1001 未认证、1003 无权限、1004 不存在）|
| 2xxx | 请求参数（2001 参数错误、2002 缺少必填）|
| 3xxx | 资源冲突（3001 已存在、3002 已被占用）|
| 4xxx | 业务限制（4001 配额超限、4002 文件过大、4003 类型不允许）|
| 5xxx | 第三方服务（5001 ONLYOFFICE 错误、5002 存储错误）|
| 9xxx | 服务器错误（9001 内部错误、9002 数据库错误）|

### 9.4 OpenAPI 文档

- 通过 `swaggo` 自动生成
- 访问：`/api/v1/docs`（Swagger UI）
- 包含所有接口、模型、错误码、认证方式
- 支持在线测试（Try it out）

---

## 10. 前端设计规范

### 10.1 视觉规范

#### 10.1.1 主题色（5 种可切换）

```css
/* Indigo (默认) */
--primary-50:  #EEF2FF
--primary-100: #E0E7FF
--primary-500: #6366F1
--primary-600: #4F46E5  /* 主色 */
--primary-700: #4338CA

/* Violet */
--primary-50:  #F5F3FF
--primary-500: #8B5CF6
--primary-600: #7C3AED

/* Emerald */
--primary-50:  #ECFDF5
--primary-500: #10B981
--primary-600: #059669

/* Rose */
--primary-50:  #FFF1F2
--primary-500: #FB7185
--primary-600: #F43F5E

/* Amber */
--primary-50:  #FFFBEB
--primary-500: #FBBF24
--primary-600: #F59E0B
```

切换方式：CSS 变量 + `data-theme="violet"` 切换

#### 10.1.2 字体

```css
--font-sans: "Inter", -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif;
--font-mono: "JetBrains Mono", "Fira Code", "SF Mono", monospace;
```

#### 10.1.3 圆角

```css
--radius-sm: 4px;
--radius:    8px;     /* 默认 */
--radius-md: 12px;
--radius-lg: 16px;
--radius-full: 9999px; /* 头像 */
```

#### 10.1.4 阴影

```css
--shadow-sm: 0 1px 2px rgba(0,0,0,0.05);
--shadow:    0 1px 3px rgba(0,0,0,0.1), 0 1px 2px rgba(0,0,0,0.06);
--shadow-md: 0 4px 6px rgba(0,0,0,0.07), 0 2px 4px rgba(0,0,0,0.06);
--shadow-lg: 0 10px 15px rgba(0,0,0,0.1), 0 4px 6px rgba(0,0,0,0.05);
```

#### 10.1.5 间距

```css
--space-1: 4px;
--space-2: 8px;
--space-3: 12px;
--space-4: 16px;
--space-6: 24px;
--space-8: 32px;
--space-12: 48px;
```

#### 10.1.6 暗色模式

- 跟随系统（默认）
- 手动切换（明 / 暗 / 跟随）
- 偏好保存到 LocalStorage
- 通过 CSS 变量切换

### 10.2 路由结构

```
/login                                    登录
/register                                 注册（v1.0 仅邀请激活）
/forgot-password                          忘记密码
/reset-password?token=xxx                 重置密码

/                                         → 重定向到 /files
/files                                    文件浏览（默认页）
/files?parent=:folderId                   指定目录
/files?view=list|grid                     视图切换
/files?sort=name&order=asc                排序
/files?type=image                         类型筛选
/files?tag=:tagId                         标签筛选

/files/:fileId                            文件详情
/files/:fileId/preview                    预览
/files/:fileId/edit                       编辑（Office）
/files/:fileId/versions                   版本历史
/files/:fileId/share                      分享管理
/files/:fileId/permissions                权限管理
/files/:fileId/info                       文件信息（侧边栏）

/search?q=&type=&tag=                     搜索结果
/starred                                  收藏
/recent                                   最近访问
/trash                                    回收站
/tags                                     标签管理

/users                                    用户管理（管理员）
/teams                                    团队管理
/roles                                    角色管理

/settings                                 系统设置（管理员）
/settings/profile                         站点设置
/settings/storage                         存储设置
/settings/security                        安全设置
/settings/onlyoffice                      ONLYOFFICE 配置
/settings/drawio                          drawio 配置
/settings/smtp                            SMTP 配置
/settings/webhooks                        Webhook 配置

/profile                                  个人资料
/profile/security                         安全设置（密码、2FA、API Token）
/profile/sessions                         会话管理
/profile/notifications                    通知设置

/s/:token                                 公开分享页（无主布局）
```

### 10.3 核心组件

#### 10.3.1 文件相关
- `FileBrowser` - 文件列表/网格容器
- `FileListItem` - 列表项
- `FileGridItem` - 网格卡片
- `FileUploader` - 上传组件（uppy + tus）
- `FilePreview` - 多类型预览器（路由分发）
- `FileVersionList` - 版本列表
- `FileInfo` - 文件信息侧边栏
- `FileBreadcrumb` - 面包屑

#### 10.3.2 编辑器
- `OfficeEditor` - ONLYOFFICE 编辑器
- `DrawioEditor` - drawio 编辑器（iframe）
- `ExcalidrawEditor` - Excalidraw 编辑器
- `CodeEditor` - 代码编辑器（Monaco）
- `MarkdownEditor` - Markdown 编辑器（Tiptap）

#### 10.3.3 弹窗
- `ShareDialog` - 创建分享弹窗
- `PermissionEditor` - 权限编辑器
- `UserPicker` - 用户选择器
- `TeamPicker` - 团队选择器
- `TagSelector` - 标签选择器
- `ConfirmDialog` - 确认弹窗

#### 10.3.4 通用
- `ThemeSwitcher` - 主题切换
- `LanguageSwitcher` - 语言切换
- `CommandPalette` - 命令面板（Cmd+K）
- `NotificationCenter` - 通知中心
- `SessionManager` - 会话管理
- `TokenManager` - API Token 管理

### 10.4 状态管理

```typescript
// 全局状态（Zustand）
useAuthStore      // 用户信息、token
useUIStore        // 主题、语言、侧边栏折叠
useUploadStore    // 上传任务队列
useNotificationStore  // 通知列表

// 服务端状态（TanStack Query）
useFiles          // 文件列表
useFile           // 单个文件
useShares         // 分享列表
useTeams          // 团队列表
useNotifications  // 通知列表
```

### 10.5 错误处理

```typescript
// 全局错误边界
<ErrorBoundary fallback={<ErrorPage />}>
  <App />
</ErrorBoundary>

// API 错误处理
axios.interceptors.response.use(
  response => response,
  error => {
    if (error.response?.status === 401) {
      // 跳转登录
    }
    if (error.response?.status === 403) {
      // Toast 提示无权限
    }
    // ... 其他处理
  }
)
```

### 10.6 性能优化

- **路由级 code splitting**（React.lazy）
- **图片懒加载**（Intersection Observer）
- **虚拟滚动**（大列表）
- **请求缓存**（TanStack Query）
- **防抖/节流**（搜索、上传）
- **预加载**（鼠标悬停时预加载预览）

---

## 11. 集成服务设计

### 11.1 ONLYOFFICE 集成

**安全时序与约束**：后端先校验当前用户对当前 FileVersion 的权限与可用状态，再生成绑定 `file_id + document_key + version` 的短期下载授权，并以 JWT 签名 DocEditor config。DocumentServer 只能使用该授权下载单文件/单版本；回调入口仅接受配置的 DocumentServer 来源（网络隔离或 allowlist）并验证回调 JWT。服务端只下载与编辑会话关联且 scheme/host 在 allowlist 内的 URL，禁止 SSRF；文件已删除、隔离或权限撤销时终止处理。status 2/6 保存，status 4 仅清理；异步落盘不阻塞回调，始终按 ONLYOFFICE 协议返回 `{"error":0}`。

#### 11.1.1 部署

```yaml
# ONLYOFFICE 版本锁定 8.2.3（详见 16.5 风险 4）
onlyoffice:
  image: onlyoffice/documentserver:8.2.3
  environment:
    - JWT_SECRET=${ONLYOFFICE_JWT_SECRET}
    - JWT_ENABLED=true
    - JWT_HEADER=Authorization
  ports: ["8081:80"]
  restart: unless-stopped
```

> **版本升级策略**：ONLYOFFICE 8.2.x 范围内小版本可平滑升级；8.x → 9.x 需评估 API 变更，建议仅在 LTS 版本内升级。

#### 11.1.2 创建会话

**前端**：
```typescript
const { editorUrl } = await api.post('/onlyoffice/session', {
  file_id: fileId,
  action: 'edit',
});

// 在 iframe 中加载
<iframe src={editorUrl} className="w-full h-full" />
```

**后端**：
```go
// 1. 验证权限
// 2. 生成下载 URL（带 JWT）
downloadURL := fmt.Sprintf("%s/api/v1/onlyoffice/download/%s", appDomain, fileID)

// 3. 构造 ONLYOFFICE 配置
config := onlyoffice.Config{
  Document: onlyoffice.Document{
    FileType: "docx",
    Key:      fmt.Sprintf("%s_v%d", fileID, version),
    Title:    file.Name,
    URL:      downloadURL,
  },
  EditorConfig: onlyoffice.EditorConfig{
    User: onlyoffice.User{
      ID:   user.ID,
      Name: user.Nickname,
    },
    Mode: "edit",
  },
}

// 4. 后端签名 DocEditor config JWT
 token := jwt.Sign(config, secret)
  editorURL := onlyofficeURL + "/web-apps/apps/api/documents/api.js?token=" + token
```

#### 11.1.3 下载文件（ONLYOFFICE 调）

```http
GET /api/v1/onlyoffice/download/:fileId
Authorization: Bearer <JWT>

返回: 文件二进制流
```

#### 11.1.4 回调处理

```go
func OnlyOfficeCallback(c *gin.Context) {
  var payload onlyoffice.Callback
  if err := c.ShouldBindJSON(&payload); err != nil {
    return
  }

  // 1. 验证 JWT
  if !jwt.Verify(payload.Token, secret) {
    return
  }

  // 2. 处理不同状态
  switch payload.Status {
  case 2, 6: // 已保存
    // 仅接受关联会话且 scheme/host 在 allowlist 的 payload.URL，防止 SSRF
    // 异步下载并校验后上传内容寻址 ObjectBlob
    // 按 (file_id, document_key, callback_version) 唯一约束幂等写 FileVersion
    // 更新 File.current_version_id；文件删除、隔离或权限撤销时安全终止
    // 记录 AuditLog
    // 推送通知
  case 4: // 已关闭
    // 清理临时资源
  }

  c.JSON(200, gin.H{"error": 0})
}
```

### 11.2 drawIO 集成

#### 11.2.1 部署模式选择

| 模式 | 适用 | 部署 |
|---|---|---|
| **Docker 模式** | 功能完整、生产推荐 | docker-compose profile=full |
| **轻量嵌入** | 节省内存、功能受限 | 仅引入前端 SDK |

#### 11.2.2 Docker 模式集成

```yaml
drawio:
  image: jgraph/drawio:27.0.9
  environment:
    - DRAWIO_SERVER_URL=https://draw.docflow.example.com/
    - DRAWIO_VIEWER_URL=https://draw.docflow.example.com/js/viewer.min.js
```

#### 11.2.3 前端集成

```typescript
const openDrawio = (fileId: string) => {
  const fileURL = `${appDomain}/api/v1/files/${fileId}/download`;
  const iframeURL = `https://draw.docflow.example.com/?offline=1&url=${encodeURIComponent(fileURL)}&spin=1&libraries=1`;
  
  const iframe = document.createElement('iframe');
  iframe.src = iframeURL;
  
  window.addEventListener('message', (e) => {
    if (e.data.type === 'save') {
      api.post(`/files/${fileId}/versions`, {
        content: e.data.xml,
      });
    }
  });
};
```

### 11.3 Excalidraw 集成

```typescript
import { Excalidraw } from "@excalidraw/excalidraw";

<Excalidraw
  initialData={await loadExcalidrawData(fileId)}
  onChange={debounce((elements, state) => {
    saveExcalidraw(fileId, elements, state);
  }, 2000)}
  UIOptions={{
    canvasActions: {
      loadScene: false,
      exportToImage: true,
    },
  }}
/>
```

存储格式：`.excalidraw` JSON（包含 elements、appState）

### 11.4 SeaweedFS 集成

#### 11.4.1 配置

```yaml
seaweedfs:
  image: chrislusf/seaweedfs:3.80
  command: >
    server -dir=/data -s3
    -volume.max=0
  ports:
    - "8333:8333"
    - "9333:9333"
```

#### 11.4.2 S3 客户端

```go
import "github.com/aws/aws-sdk-go-v2/service/s3"

cfg, _ := config.LoadDefaultConfig(context.TODO(),
  config.WithEndpointResolverWithOptions(
    aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
      return aws.Endpoint{
        URL: "http://seaweedfs:8333",
      }, nil
    }),
  ),
  config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
    accessKey, secretKey, "",
  )),
  config.WithRegion("us-east-1"),
)

client := s3.NewFromConfig(cfg, func(o *s3.Options) {
  o.UsePathStyle = true
})
```

#### 11.4.3 操作

```go
// 上传
client.PutObject(ctx, &s3.PutObjectInput{
  Bucket: aws.String("docflow"),
  Key:    aws.String(storageKey),
  Body:   file,
})

// 下载
client.GetObject(ctx, &s3.GetObjectInput{
  Bucket: aws.String("docflow"),
  Key:    aws.String(storageKey),
})

// 预签名 URL（用于 ONLYOFFICE）
presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
  Bucket: aws.String("docflow"),
  Key:    aws.String(storageKey),
}, presign.WithExpires(15*time.Minute))
```

---

## 12. 部署架构

### 12.1 Docker Compose 服务清单

生产 Compose 不使用顶层 `version`；除 Caddy 的 80/443 外，不发布任何宿主机端口。前端镜像只提供已构建静态产物，由 Caddy 服务，绝不运行开发服务器。`minimal`、`full`、`antivirus` 三个 profile 均真实定义；默认安全部署须启用 `antivirus`，扫描未通过的对象不可用。

```yaml
# docker-compose.yml
services:
  backend:
    image: docflow/backend:1.0.0
    restart: unless-stopped
    depends_on:
      postgres: { condition: service_healthy }
      redis: { condition: service_healthy }
      seaweedfs: { condition: service_healthy }
    environment:
      DB_HOST: postgres
      REDIS_HOST: redis
      REDIS_PASSWORD: ${REDIS_PASSWORD_SECRET}
      S3_ENDPOINT: seaweedfs:8333
      JWT_SECRET: ${JWT_SECRET}
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8080/health"]
      interval: 30s
      timeout: 5s
      retries: 3
    networks: [docflow-net]

  caddy:
    image: caddy:2.8.4-alpine
    restart: unless-stopped
    ports: ["80:80", "443:443"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - frontend_dist:/srv/frontend:ro
      - caddy_data:/data
      - caddy_config:/config
    networks: [docflow-net]

  postgres:
    image: postgres:16.4-alpine
    restart: unless-stopped
    environment:
      POSTGRES_DB: docflow
      POSTGRES_USER: docflow
      POSTGRES_PASSWORD: ${DB_PASSWORD_SECRET}
    volumes: [postgres_data:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U docflow -d docflow"]
      interval: 10s
      timeout: 5s
      retries: 5
    networks: [docflow-net]

  redis:
    image: redis:7.4.1-alpine
    restart: unless-stopped
    command: redis-server --appendonly yes --requirepass ${REDIS_PASSWORD_SECRET}
    volumes: [redis_data:/data]
    healthcheck:
      test: ["CMD-SHELL", "redis-cli -a $$REDIS_PASSWORD_SECRET ping"]
      interval: 10s
      timeout: 5s
      retries: 5
    networks: [docflow-net]

  seaweedfs:
    image: chrislusf/seaweedfs:3.80
    profiles: ["minimal", "full", "antivirus"]
    restart: unless-stopped
    command: server -dir=/data -s3 -volume.max=0
    volumes: [seaweedfs_data:/data]
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:9333/cluster/status"]
      interval: 30s
      timeout: 5s
      retries: 3
    networks: [docflow-net]

  onlyoffice:
    image: onlyoffice/documentserver:8.2.3
    profiles: ["full"]
    restart: unless-stopped
    environment:
      JWT_SECRET: ${ONLYOFFICE_JWT_SECRET}
      JWT_ENABLED: "true"
    volumes: [onlyoffice_data:/var/www/onlyoffice/Data, onlyoffice_logs:/var/log/onlyoffice]
    networks: [docflow-net]

  drawio:
    image: jgraph/drawio:27.0.9
    profiles: ["full"]
    restart: unless-stopped
    networks: [docflow-net]

  clamav:
    image: clamav/clamav:1.4.1
    profiles: ["antivirus"]
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "clamdscan", "--version"]
      interval: 30s
      timeout: 10s
      retries: 3
    networks: [docflow-net]

volumes:
  postgres_data:
  redis_data:
  seaweedfs_data:
  frontend_dist:
  caddy_data:
  caddy_config:
  onlyoffice_data:
  onlyoffice_logs:

networks:
  docflow-net:
    driver: bridge
```

应用 `/ready` 必须检查实际启用的 PostgreSQL、Redis、对象存储及可选集成；未就绪实例不得接收业务流量。密码和密钥均由部署 secret 或受保护的 `.env` 注入，Redis 开启 AOF 持久化并使用密码认证。

### 12.2 Caddyfile 示例

```caddyfile
{$APP_DOMAIN} {
  encode gzip
  
  route /assets/* {
    root * /srv/frontend
    file_server
  }
  root * /srv/frontend
  try_files {path} /index.html
  file_server
  
  reverse_proxy /api/* backend:8080
  
  reverse_proxy /onlyoffice/* onlyoffice:80
  
  reverse_proxy /drawio/* drawio:8080 {
    header_up Host {host}
    header_up X-Real-IP {remote}
  }
  
  respond /health 200
  
  log
}
```

### 12.3 Profiles 启动方式

```bash
# 完整版（推荐，含 drawio）
docker compose --profile full up -d

# 极简版（不含 ONLYOFFICE/drawio；默认安全策略仍启用 ClamAV）
docker compose --profile minimal --profile antivirus up -d
```

---

## 13. 配置与运维

### 13.1 环境变量与配置边界

基础设施地址、凭据、签名密钥和进程级参数通过环境变量或部署 Secret 注入；可由管理员调整的非密钥运行参数由 `system_settings` 和 `/settings` 页面管理。以下为部署时环境变量示例，页面不读取或回显 Secret。

```bash
# ========== 应用基础 ==========
APP_NAME=DocFlow
APP_DOMAIN=docflow.example.com
APP_SCHEME=https
APP_PORT=8080
APP_LANG=zh-CN
APP_THEME=indigo
APP_DEBUG=false

# ========== 数据库 ==========
DB_HOST=postgres
DB_PORT=5432
DB_NAME=docflow
DB_USER=docflow
DB_PASSWORD=${DB_PASSWORD_SECRET}
DB_SSLMODE=require
DB_MAX_OPEN_CONNS=100
DB_MAX_IDLE_CONNS=20

# ========== Redis ==========
REDIS_HOST=redis
REDIS_PORT=6379
REDIS_PASSWORD=${REDIS_PASSWORD_SECRET}
REDIS_DB=0

# ========== SeaweedFS (S3) ==========
S3_ENDPOINT=seaweedfs:8333
S3_REGION=us-east-1
S3_BUCKET=docflow
S3_ACCESS_KEY=${S3_ACCESS_KEY_SECRET}
S3_SECRET_KEY=${S3_SECRET_KEY_SECRET}
S3_PATH_STYLE=true
S3_SSE=false

# ========== ONLYOFFICE ==========
ONLYOFFICE_URL=http://onlyoffice:80
ONLYOFFICE_JWT_SECRET=${ONLYOFFICE_JWT_SECRET}
ONLYOFFICE_HTTPS=false

# ========== drawio ==========
DRAWIO_URL=http://drawio:8080

# ========== 文件上传 ==========
MAX_FILE_SIZE=2147483648  # 2GB
TUS_CHUNK_SIZE=10485760   # 10MB
MAX_VERSIONS_PER_FILE=5
TRASH_RETENTION_DAYS=30

# ========== 用户与认证 ==========
ALLOW_SELF_REGISTER=false
DEFAULT_STORAGE_QUOTA=10737418240  # 10GB
PASSWORD_MIN_LENGTH=8
JWT_ACCESS_TTL=900       # 15m
JWT_REFRESH_TTL=604800   # 7d；随机不透明 Cookie，rotation
JWT_SECRET=${JWT_SECRET}
LOGIN_MAX_RETRIES=5
LOGIN_LOCK_MINUTES=15

# ========== 安全 ==========
RATE_LIMIT=100
API_RATE_LIMIT=600
CORS_ALLOWED_ORIGINS=https://docflow.example.com

# ========== 水印 ==========
WATERMARK_ENABLED_BY_DEFAULT=true
WATERMARK_TEXT_TEMPLATE={email} | {time}

# ========== 日志 ==========
LOG_LEVEL=info
LOG_FORMAT=json
LOG_RETENTION_DAYS=90

# ========== 邮件（可选）==========
SMTP_ENABLED=false
SMTP_HOST=smtp.example.com
SMTP_PORT=587
SMTP_USER=noreply@example.com
SMTP_PASSWORD=${SMTP_PASSWORD_SECRET}
SMTP_FROM=DocFlow <noreply@example.com>

# ========== AI（v2.0，可选）==========
AI_ENABLED=false
AI_PROVIDER=openai
AI_API_KEY=
AI_MODEL=gpt-4
AI_BASE_URL=
```

### 13.2 健康检查

```http
GET /health
{
  "status": "ok",
  "version": "1.0.0",
  "uptime": 3600
}

GET /ready
{
  "status": "ready",
  "checks": {
    "database": "ok",
    "redis": "ok",
    "storage": "ok",
    "onlyoffice": "ok"
  }
}

GET /metrics  # Prometheus 格式
```

### 13.3 Prometheus 指标

HTTP 指标使用规范化路由模板或 route name，禁止将文件 ID、分享 token 等动态 path 作为标签。

```yaml
docflow_files_total
docflow_storage_bytes
docflow_users_active{period="24h"}
docflow_shares_total
docflow_uploads_total{status}
docflow_upload_processing_duration_seconds{stage,status}
docflow_preview_jobs_total{status}
docflow_scan_results_total{status}
docflow_onlyoffice_callbacks_total{status,result}
docflow_http_requests_total{method,route,status}
docflow_http_request_duration_seconds{method,route}
docflow_db_connections_active
docflow_queue_tasks_pending
docflow_queue_tasks_failed_total
docflow_backup_verification_total{result}
```

### 13.4 日志规范

```json
{
  "ts": "2026-09-09T10:00:00.123Z",
  "level": "INFO",
  "msg": "file uploaded",
  "logger": "file_service",
  "request_id": "req_abc123",
  "user_id": "uuid",
  "file_id": "uuid",
  "file_name": "PRD.docx",
  "size": 1234567,
  "duration_ms": 1234,
  "ip": "192.168.1.1"
}
```

### 13.5 备份与恢复策略

| 类型 | 频率 | 保留期 | 措施 |
|---|---|---|---|
| PostgreSQL | 每日全量 + 持续 WAL 归档 | 30 天 | 加密备份；与对象存储时间点一致 |
| SeaweedFS | 每日 04:00 | 30 天 | 版本化/加密备份并异地复制 |
| Redis | 每日 05:00 | 7 天 | 用于恢复任务状态，不作为文件唯一来源 |
| 配置 | 每次变更 | 永久 | 非密钥设置审计化导出；密钥由 Secret 管理 |
| 审计日志 | 每周归档 | 1 年 | 压缩、加密、异地存放 |

- 目标：生产默认 RPO ≤ 24 小时、RTO ≤ 8 小时；采用 WAL 归档时可进一步缩小 RPO。实际目标须在部署设置页和运维告警中可见。
- 每月至少执行一次恢复演练：恢复数据库与对象存储、验证对象 SHA-256、FileVersion/ObjectBlob 引用一致性、抽样预览下载和权限隔离；结果写入审计与 `docflow_backup_verification_total`。
- 恢复操作必须由 `super_admin` 二次确认并生成不可篡改审计事件；恢复前停止写入并在完成后重新执行扫描/队列补偿检查。

### 13.6 升级流程

```bash
# 1. 备份数据库和文件
./scripts/backup.sh

# 2. 拉取新镜像
docker compose pull

# 3. 运行数据库迁移（自动）
docker compose up -d backend

# 4. 重启服务
docker compose restart backend frontend

# 5. 验证
curl https://docflow.example.com/health
```

---

## 14. 国际化与本地化

### 14.1 支持语言（v1.0）

- 🇨🇳 简体中文（zh-CN，默认）
- 🇺🇸 English（en-US）

### 14.2 实现

#### 14.2.1 前端（react-i18next）
```
src/i18n/
├── zh-CN/
│   └── translation.json
└── en-US/
    └── translation.json
```

#### 14.2.2 后端（go-i18n）
```
internal/i18n/locales/
├── zh-CN.yaml
└── en-US.yaml
```

### 14.3 翻译规范

```json
{
  "file": {
    "upload": {
      "success": "文件上传成功",
      "failed": "上传失败",
      "uploading": "上传中..."
    },
    "actions": {
      "download": "下载",
      "delete": "删除",
      "share": "分享"
    }
  }
}
```

### 14.4 时区与日期

- 数据库存储 UTC 时间戳
- 前端按用户偏好时区显示（`Intl.DateTimeFormat`）
- 日期格式跟随 locale
- 数字格式跟随 locale（千分位、小数点）

---

## 15. 开发路线图

路线图只表达范围与依赖，不提供周/天工期预测。

### 阶段 1：MVP
- 邀请制认证、密码重置、基础 RBAC 与管理员用户管理
- 个人文件和系统创建的固定根目录、文件夹、命名校验与回收站
- tus 上传/下载、Range、校验/扫描状态、基础图片/PDF/文本预览
- 公开分享、基础访问统计与审计
- Docker Compose/Caddy 部署、健康检查、备份与关键 E2E/安全验收

### 阶段 2：v1.0
- 团队、团队根目录、权限继承、私有分享与成员变更即时生效
- ObjectBlob/FileVersion 版本控制、ONLYOFFICE 安全编辑回调
- 独立内容域网页包安全预览、标签、批量操作
- 会话/Refresh rotation、PAT、站内通知与通知偏好
- drawio/Excalidraw；若验证后工作量不适合，整体顺延至 v1.1，并同步 US-013

### 阶段 3：v1.1 非核心体验增强
- PWA 离线降级、主题、快捷键、视图与拖拽体验、仪表盘
- 邮件/Webhook、性能与部署体验、更多预览格式

### 阶段 4：v2
- OIDC/SSO、2FA、AI、全文搜索、高级版本对比/合并、移动端/桌面端

### 里程碑

| 里程碑 | 交付物 |
|---|---|
| MVP 基线 | 上述 MVP 范围通过关键 E2E 与安全验收 |
| v1.0 基线 | 团队协作、版本/编辑、隔离预览和个人化能力完成 |
| v1.1 增强 | 非核心体验与集成增强完成 |
| v2 规划 | SSO/2FA/AI/全文搜索等高级能力进入独立发布线 |

---

## 16. 附录

### 16.1 名词解释

| 术语 | 说明 |
|---|---|
| **DocFlow** | 项目名 |
| **Team** | 团队（原 Group），用户组 |
| **Share** | 分享（公开链接或私有分享）|
| **Token** | 短链哈希（8 位）|
| **RBAC** | 基于角色的访问控制 |
| **ABAC** | 基于属性的访问控制 |
| **Casbin** | 权限管理库 |
| **tus** | 基于 HTTP 的可断点续传文件上传协议 |
| **JWT** | JSON Web Token |
| **CSP** | Content Security Policy |
| **ONLYOFFICE** | 开源 Office 套件 |
| **drawIO** | 开源图表工具（diagrams.net）|
| **Excalidraw** | 开源手绘风格白板工具 |
| **SeaweedFS** | 分布式对象存储 |
| **TOTP** | 基于时间的一次性密码（2FA）|
| **PAT** | Personal Access Token，个人访问令牌 |
| **SSE** | Server-Side Encryption，服务端加密 |

### 16.2 安全检查清单

部署前必检：

- [ ] 所有默认密码已修改（DB、JWT、ONLYOFFICE、Caddy）
- [ ] 仅暴露必要端口（80、443）
- [ ] 数据库不对外暴露
- [ ] Redis 设置密码
- [ ] SeaweedFS 设置访问密钥
- [ ] Caddy 自动 HTTPS 配置正确
- [ ] 备份策略已配置
- [ ] 日志已结构化
- [ ] 健康检查端点可访问
- [ ] Prometheus 指标正常上报

### 16.3 推荐部署组合

#### 极小团队（< 20 用户）
```yaml
单服务器: 4C8G 100GB SSD（生产最低配置；2C4G 仅用于不含 ONLYOFFICE 的开发测试）
服务: Postgres + Redis + SeaweedFS + Backend + Frontend + ONLYOFFICE + drawio
内存: 后端 200MB + ONLYOFFICE 2GB + drawio 500MB = ~3GB
冗余: 无（建议每日备份）
```

#### 小团队（20-100 用户）
```yaml
单服务器: 4C8G 500GB SSD
服务: 同上 + Meilisearch
内存: ~5GB
冗余: 每日本地备份 + 每周异地备份
```

#### 中等团队（100-500 用户）
```yaml
应用服务器: 4C8G（后端多实例 + 负载均衡）
数据库服务器: 4C8G（Postgres + Redis）
存储服务器: 8C16G（SeaweedFS 集群）
集成服务器: 8C16G（ONLYOFFICE + drawio + Meilisearch）
冗余: 数据库主从 + 存储多副本
```

### 16.4 参考资料

#### 官方文档
- [Casbin](https://casbin.org/)
- [PostgreSQL ltree](https://www.postgresql.org/docs/current/ltree.html)
- [SeaweedFS](https://github.com/chrislusf/seaweedfs)
- [ONLYOFFICE](https://api.onlyoffice.com/)
- [drawIO](https://www.drawio.com/doc/faq/)
- [Excalidraw](https://docs.excalidraw.com/)
- [tus Protocol](https://tus.io/)
- [Shadcn/ui](https://ui.shadcn.com/)
- [TanStack Query](https://tanstack.com/query)

#### 项目仓库
- 后端：`github.com/docflow/docflow/backend`
- 前端：`github.com/docflow/docflow/frontend`
- 文档：`github.com/docflow/docflow/docs`

### 16.5 已知风险与缓解策略

本节汇总设计阶段识别的关键风险及其缓解措施，供后续开发与运维参考。

#### 风险 1：ONLYOFFICE 内存占用大 ⚠️⚠️⚠️（高优先级）

**风险描述**：
- ONLYOFFICE Document Server 启动后至少占用 **2GB RAM**（Java + Node.js + 服务进程）
- 编辑大型文档时可能峰值达 **3-4GB**
- 小型部署（2C4G）跑完 ONLYOFFICE + 后端 + 前端 + 数据库 + Redis 后会**严重内存不足**，导致 OOM 或频繁 swap

**影响范围**：
- 部署规模受限
- 个人开发者难以本地体验
- 极小规模团队成本上升

**缓解措施**：

| 措施 | 说明 |
|---|---|
| ✅ 提升推荐起步配置 | 见 5.5 节，**生产起步 4C8G**（不再推荐 2C4G）|
| ✅ 配置上限设置 | 通过 Docker `--memory=2g --memory-swap=3g` 限制 ONLYOFFICE 上限 |
| ✅ 健康检查 | 通过 `/health` 端点监控 ONLYOFFICE 状态，自动重启异常实例 |
| ⏸ 仅查看模式优化（v2.0）| ONLYOFFICE Community Server 提供 viewing-only 模式，内存占用更低 |
| ⏸ 共享 ONLYOFFICE 实例（v2.0）| 多 DocFlow 实例可共享一个 ONLYOFFICE 集群 |

#### 风险 2：SeaweedFS 数据迁移成本 ✅ 已规避

**状态**：✅ **不适用**

本项目为全新项目，无历史数据需要从 MinIO 迁移。如果未来需要切换 SeaweedFS 的不同版本，可通过：
- `weed shell` 工具迁移 volume
- 或使用 `mc` (MinIO Client) 跨 S3 兼容存储复制

#### 风险 3：前端包大小 ⚠️（中优先级）

**风险描述**：
- 前端集成了多个大型依赖：PDF.js（~500KB）、Excalidraw（~300KB）、Monaco Editor（~2MB）、drawio viewer（~200KB）、ONLYOFFICE SDK（~150KB）、video.js（~200KB）
- 如果全部打包进主 bundle，首屏 JS 可能超过 **2MB**（未压缩），严重影响首屏加载
- 移动端/弱网用户体验差

**缓解措施**（已写入 5.6.4）：

| 库 | 大小 | 加载策略 |
|---|---|---|
| shadcn/ui 基础 | ~150KB | 主 bundle（必需）|
| recharts | ~200KB | 主 bundle（仪表盘使用）|
| PDF.js (react-pdf) | ~500KB | **懒加载**（仅 PDF 预览页）|
| Excalidraw | ~300KB | **懒加载**（仅 Excalidraw 编辑页）|
| Monaco Editor | ~2MB | **懒加载**（仅代码编辑页）|
| drawio viewer | ~200KB | **懒加载**（仅 drawio 页）|
| ONLYOFFICE SDK | ~150KB | **懒加载**（仅 Office 编辑页）|
| video.js | ~200KB | **懒加载**（仅视频播放页）|
| wavesurfer.js | ~100KB | **懒加载**（仅音频页）|

**性能指标**：
- 首屏 bundle < 300KB（gzip）
- 主路由 bundle < 500KB
- 编辑器页面按需加载，最坏情况 < 1MB

**实现要点**：
```typescript
// 使用 dynamic import + Suspense
const OfficeEditor = lazy(() => import('./editors/OfficeEditor'));
const ExcalidrawEditor = lazy(() => import('./editors/ExcalidrawEditor'));

<Suspense fallback={<Loading />}>
  <OfficeEditor />
</Suspense>
```

#### 风险 4：ONLYOFFICE 版本兼容性 ⚠️（中优先级）

**风险描述**：
- ONLYOFFICE 8.x 与 7.x 之间 API 有破坏性变更
- 与 9.x 兼容性未确认
- 容器升级可能导致客户端报错或回调失败

**缓解措施**（已写入 5.4 / 11.1.1 / 12.1）：

| 措施 | 说明 |
|---|---|
| ✅ **版本锁定** | `onlyoffice/documentserver:8.2.3`（精确版本，避免 latest 漂移）|
| ✅ **升级策略** | 仅在 8.2.x 小版本内自动升级；8.x → 9.x 需先评估 API 变更 |
| ✅ **数据迁移** | ONLYOFFICE 升级会保留 `Data` 目录内的文件，文件系统级备份即可 |
| ✅ **回调验证** | 升级前在测试环境验证 JWT 签名、回调 URL、文档转换流程 |
| ✅ **回滚预案** | 升级前备份 `onlyoffice_data` 卷，可在 5 分钟内回滚 |

#### 风险 5：外部依赖不可用 ⚠️（中优先级）

**风险描述**：PostgreSQL、Redis、对象存储、队列或 ClamAV 不可用时，部分请求和异步任务可能失败。

**缓解措施**：
- `/ready` 检查实际启用的依赖；未就绪实例不接收新业务流量。
- 写操作、上传完成、扫描和回调均幂等，失败任务有限重试并进入补偿/告警队列。
- Redis 仅承担缓存、队列协调与广播，不存 tus 分片，也不承担 Access Token 黑名单；Access Token 依靠 15 分钟 TTL 自然失效。
- 关键数据以 PostgreSQL、对象存储及 outbox 为准，恢复后通过一致性校验收敛。

#### 其他潜在风险

| 风险 | 等级 | 缓解策略 |
|---|---|---|
| **大文件上传网络中断** | 中 | tus 协议断点续传（已实现）|
| **SeaweedFS 磁盘满** | 中 | 监控卷使用率 ≥ 80% 告警，自动扩容 |
| **PostgreSQL 连接数耗尽** | 低 | GORM 连接池配置 + PgBouncer 中间件（v2.0）|
| **CDN 缓存策略不当** | 低 | 静态资源 cache-control 头 + 内容哈希 |
| **DDoS 攻击** | 中 | Caddy + Cloudflare 反代层防护 |
| **日志文件膨胀** | 低 | lumberjack 日志轮转 + Loki 聚合（v2.0）|
| **第三方依赖 CVE** | 中 | Dependabot 自动 PR + 每月安全审计 |

### 16.6 风险监控指标

部署后建议监控以下指标。HTTP 指标使用规范化路由模板或 route name，禁止将文件 ID、分享 token 等动态 path 作为 Prometheus 标签，避免高基数。

```yaml
必需监控:
  - docflow_uploads_total{status}
  - docflow_upload_processing_duration_seconds{stage,status}
  - docflow_preview_jobs_total{status}
  - docflow_onlyoffice_callbacks_total{status,result}
  - docflow_scan_results_total{status}
  - docflow_backup_last_success_timestamp
  - docflow_backup_verification_total{result}
  - docflow_http_request_duration_seconds{route,method,status}

建议 SLO:
  - API 可用性 >= 99.9%
  - 上传完成接口成功率 >= 99.5%
  - 可用对象预览成功率 >= 99%
  - ONLYOFFICE 回调 error=0 响应率 >= 99.9%
  - 恶意文件不得进入 available
  - 每月恢复演练按期完成且一致性校验通过

资源与队列:
  - onlyoffice_container_memory_usage > 80%
  - upload_concurrent_count
  - scan_queue_depth
  - preview_queue_depth
  - object_storage_capacity_ratio
  - db_connections_active
```

### 16.7 测试与验收

- **认证**：验证 Access Token 仅存内存、15 分钟过期、Refresh Token rotation、重放检测撤销 token family、登出撤销 Session 与 CSRF 防护。
- **权限**：验证显式 deny、权限继承、团队成员变更即时撤权、私有分享撤销，以及删除/隔离状态不可通过分享绕过。
- **上传与扫描**：验证 tus 断点续传、幂等完成、SHA-256/大小/魔数校验、ClamAV 通过后才可用、失败 fail closed、补偿与延迟 GC。
- **网页包安全**：验证独立 origin、Cookie/认证头隔离、sandbox/CSP/nosniff、Zip Slip/符号链接/解压炸弹防护及外部请求限制。
- **ONLYOFFICE**：验证 JWT、来源 allowlist、下载 URL SSRF 防护、status 2/6 保存、status 4 仅清理，以及 `(file_id, document_key, callback_version)` 幂等。
- **恢复演练**：验证数据库与对象存储备份解密恢复、SHA-256 引用一致性、RPO/RTO 记录和月度演练审计。

```yaml
必需监控:
  - docflow_uploads_total{status}
  - docflow_upload_processing_duration_seconds{stage,status}
  - docflow_preview_jobs_total{status}
  - docflow_onlyoffice_callbacks_total{status,result}
  - docflow_scan_results_total{status}
  - docflow_backup_last_success_timestamp
  - docflow_backup_verification_total{result}
  - docflow_http_request_duration_seconds{route,method,status}

建议 SLO:
  - API 可用性 >= 99.9%
  - 上传完成接口成功率 >= 99.5%
  - 可用对象预览成功率 >= 99%
  - ONLYOFFICE 回调 error=0 响应率 >= 99.9%
  - 恶意文件不得进入 available
  - 每月恢复演练按期完成且一致性校验通过

资源与队列:
  - onlyoffice_container_memory_usage > 80%
  - upload_concurrent_count
  - scan_queue_depth
  - preview_queue_depth
  - object_storage_capacity_ratio
  - db_connections_active
```

---

---

## 📋 文档完结

本文档为 **DocFlow v1.0 设计基线（需评审）**，包含：

- ✅ **产品范围决策与目标用户**
- ✅ **15 条按 MVP/v1.0/v1.1/v2 标记的用户故事**
- ✅ **23 张数据表/关联表及核心字段定义**
- ✅ **系统架构图、数据流图与外部状态约束**
- ✅ **技术栈、文件版本/对象存储与安全设计**
- ✅ **功能规格、权限判定优先级与配置页面**
- ✅ **OpenAPI 唯一契约、版本化路由与错误规范**
- ✅ **ONLYOFFICE、网页包隔离、上传扫描与部署设计**
- ✅ **无时间预测的四阶段路线图**
- ✅ **监控、SLO、备份恢复与关键 E2E/安全验收要求**

**不包含**（按用户要求）：
- ❌ 具体代码实现
- ❌ 测试用例代码
- ❌ UI 设计稿

---

## 🚀 下一步

本文档可作为后续 agent 开发的依据。建议：

1. **审阅本文档** —— 检查是否有遗漏或需要调整
2. **分模块开发** —— 按第 15 章的阶段依赖拆分给不同 agent
3. **持续迭代** —— 每月回顾，根据实际反馈调整

**祝开发顺利！**

— DocFlow Team
