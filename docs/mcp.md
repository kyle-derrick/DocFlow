# DocFlow MCP 服务端（/mcp）

DocFlow 以 MCP（Model Context Protocol）对外暴露文件与协作能力：在 Claude / Cursor / Cline 等 AI agent 中接入后，AI 可以完全操作 DocFlow（浏览、读写、版本管理、分享、检索）。

- **协议**：JSON-RPC 2.0 over HTTP（Streamable HTTP 简化版）——单次请求-响应，无 session / SSE 长连
- **端点**：`POST /mcp`（`GET /mcp` 返回 405，不支持服务端推送流）
- **协议版本**：`initialize` 返回 `protocolVersion: 2025-03-26`
- **支持方法**：`initialize` / `notifications/*`（静默确认）/ `ping` / `tools/list` / `tools/call`
- **限流**：按 IP 每分钟 60 次（429 超限）

## 接入方法

### 1. 创建个人访问令牌（PAT）

MCP 不走浏览器 Cookie，鉴权使用 PAT（推荐，可限定 scope）或用户 access token。

1. 登录 DocFlow Web → 个人设置 → 个人访问令牌（`POST /api/v1/tokens`）
2. 创建令牌，**明文 `dfpat_...` 仅展示一次**，立即复制
3. scope 二选一或多选：
   - `files:read`：只读工具
   - `files:write`：写入工具（含分享创建/撤销）
   - 不限定 scope 的 PAT 拥有全部能力（等同账号权限）

### 2. 配置客户端

Cursor / Cline（`~/.cursor/mcp.json` 或 `cline_mcp_settings.json`）：

```json
{
  "mcpServers": {
    "docflow": {
      "url": "https://your-docflow.example.com/mcp",
      "headers": {
        "Authorization": "Bearer dfpat_xxxxxxxxxxxxxxxxxxxxxxxx"
      }
    }
  }
}
```

Claude Desktop（HTTP 传输，需 2025-03-26 及以上协议版本支持；亦可经 mcp-remote 桥接）：

```json
{
  "mcpServers": {
    "docflow": {
      "type": "http",
      "url": "https://your-docflow.example.com/mcp",
      "headers": { "Authorization": "Bearer dfpat_xxxxxxxxxxxxxxxxxxxxxxxx" }
    }
  }
}
```

> 本实现为单次请求-响应（每次 POST 均可独立鉴权），不要求客户端维持 session；`initialized` 通知以 HTTP 202 确认。

### 3. curl 冒烟

```bash
# initialize
curl -s https://your-docflow.example.com/mcp \
  -H "Authorization: Bearer dfpat_..." \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}'

# 列工具
curl -s .../mcp -H "Authorization: Bearer dfpat_..." \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'

# 建目录
curl -s .../mcp -H "Authorization: Bearer dfpat_..." \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"df_create_folder","arguments":{"name":"AI 工作区"}}}'
```

## 工具清单

| 工具 | 一句话说明 | scope |
| --- | --- | --- |
| `df_list_files` | 列出目录内容（个人/团队空间，limit/offset 分页） | files:read |
| `df_get_file` | 文件/目录元数据 + 当前版本（大小/sha256/MIME/状态） | files:read |
| `df_create_folder` | 创建子目录（团队目录继承作用域） | files:write |
| `df_write_file` | 新建文件写入内容（text/base64 二选一，走上传管线） | files:write |
| `df_write_version` | 覆盖既有文件为新版本（编辑文本/图表源码） | files:write |
| `df_read_file` | 读文件内容（text/base64，2MB 上限，超限提示下载链接） | files:read |
| `df_rename_file` | 重命名 | files:write |
| `df_move_file` | 移动到新父目录 | files:write |
| `df_copy_file` | 复制文件到目标目录 | files:write |
| `df_delete_file` | 软删除到回收站 | files:write |
| `df_restore_file` | 从回收站恢复 | files:write |
| `df_list_trash` | 列出回收站条目（个人/团队） | files:read |
| `df_list_versions` | 列出全部历史版本 | files:read |
| `df_restore_version` | 回滚 current_version 到既有版本 | files:write |
| `df_search_files` | 全文检索（名称 + 文本内容；未启用时返回明确错误） | files:read |
| `df_list_shares` | 列出我创建的分享 | files:read |
| `df_create_share` | 创建分享（public token / private 授权，密码/有效期可选） | files:write |
| `df_revoke_share` | 撤销分享 | files:write |
| `df_list_teams` | 列出所属团队（定位 team_id） | files:read |
| `df_resolve_path` | 按路径定位文件返回 file_id（如 `docs/报告.md`） | files:read |
| `df_download_url` | 返回下载/预览 API 路径（大文件引导） | files:read |

说明：

- `scope` 参数统一为 `personal`（缺省，个人空间）/ `team`（需 `team_id`）
- **drawio / excalidraw 无需专用工具**：`.drawio` 即 XML、`.excalidraw` 即 JSON，AI 直接 `df_read_file` 读文本、编辑后 `df_write_version` 写回即为新版本
- 写入走完整上传管线：Office 格式校验、病毒扫描（ClamAV）、存储配额、扩展名黑名单、MIME 按扩展名推断，全部生效；同名冲突报错可换名重试
- 搜索（`df_search_files`）依赖部署启用全文检索；未启用时 `tools/list` 中标注「当前部署未启用」，调用返回结构化错误

## JSON-RPC 错误码

| code | 含义 |
| --- | --- |
| `-32700` | 请求体不是合法 JSON（parse error） |
| `-32600` | 不是合法的 JSON-RPC 2.0 请求 |
| `-32601` | 方法不存在 |
| `-32602` | 参数非法（含未知工具名、参数校验失败） |
| `-32603` | 服务端内部错误 |
| `-32001` | 认证失败（Bearer 凭证缺失/无效，HTTP 401） |
| `-32003` | PAT scope 不足（如只读 PAT 调写工具） |

工具执行的业务错误（文件不存在、重名冲突、越权、配额超限等）按 MCP 规范以 `result.isError=true` 返回，`content[0].text` 为面向 AI 的可读说明。

## 安全说明

- **鉴权**：与 REST API 完全同源——PAT（`dfpat_` 前缀，哈希存储、可撤销、可限定 scope、支持过期）或 HS256 access token；未认证返回 401 + `-32001`
- **权限模型零旁路**：所有工具直接调用现有服务层（`authorizeFile*`、团队角色 CanRead/CanWrite/CanDelete、路径级 ACL、PAT scope），MCP 层不做任何额外放行
- **PAT scope**：与 REST `RequireScope` 语义一致——限定 scope 的 PAT 按 `files:read`/`files:write` 精确匹配；JWT 与未限定 scope 的 PAT 不受限
- **上传管线**：写入必经 office 校验 / 病毒扫描 / 配额 / 黑名单，与网页端上传同一链路
- **CSRF**：`/mcp` 挂根路由、不携带 Cookie 会话，仅接受 Bearer 凭证，天然免疫 CSRF
- **限流**：按 IP 60 次/分钟；请求体上限 64MB（内容大小仍受单文件上限与配额约束）
- **最小权限建议**：为 AI 单独创建限定 `files:read`（或按需 `files:write`）的 PAT，用完即撤销
- **审计**：MCP 调用复用服务层权限与数据一致性保证；当前写操作不另记审计事件（后续版本可补）
