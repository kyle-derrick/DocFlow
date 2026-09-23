#!/bin/sh
# DocFlow Agent 镜像入口：按平台注入的 DOCFLOW_HARNESS 分派成熟 agent
# harness——claude-code（Anthropic 协议 /v1/messages 工具透传）或 pi
#（badlogic/pi-mono，OpenAI 兼容 /v1/chat/completions 工具转发）；失败/
# 不可用/未注入回落自研 runner（AI 工具循环 / 降级执行 ```run 块）。
#
# 环境约定（平台 docker.go 注入）：
#   DOCFLOW_PROMPT_FILE  prompt 文件（只读挂载）
#   DOCFLOW_WORKSPACE    工作目录（/workspace，产物区）
#   DOCFLOW_AI_SOCK      平台 AI IPC socket（unix，只读挂载）
#   DOCFLOW_AI_TOKEN     一次性任务令牌（缺省=降级模式）
#   DOCFLOW_HARNESS      claude-code | pi | builtin（平台按默认 Provider
#                        协议自动路由后的终值；缺省自动探测已装 CLI）
#
# 接线原理：socat 把平台 unix socket 转成本机 TCP（NetworkMode=none 下
# 容器自身回环可用），harness 的 base URL 指向它——成熟 harness 的完整
# 工具循环（Bash/Read/Write/Edit、审批、恢复）以平台 AI 为模型后端在
# 断网沙箱内运行。
set -u
PROMPT_FILE="${DOCFLOW_PROMPT_FILE:-/run/docflow/prompt}"
WS="${DOCFLOW_WORKSPACE:-/workspace}"
HARNESS="${DOCFLOW_HARNESS:-}"
cd "$WS" || exit 0

baseline() {
  git init -q 2>/dev/null || return 0
  git config user.email agent@docflow
  git config user.name docflow-agent
  if [ ! -f .gitignore ]; then
    printf 'node_modules/\ndist/\nbuild/\nout/\n.cache/\n.next/\n.nuxt/\n.venv/\nvenv/\n__pycache__/\n.pytest_cache/\ncoverage/\n.git/\n.docflow-changes.json\n' > .gitignore
  fi
  git add -A 2>/dev/null
  git commit -qm baseline 2>/dev/null || true
}

run_claude() {
  command -v claude >/dev/null 2>&1 || return 1
  [ -n "${DOCFLOW_AI_TOKEN:-}" ] || return 1
  [ -S "${DOCFLOW_AI_SOCK:-}" ] || return 1
  # unix socket → 本机 TCP 回环（容器断网不影响自身 loopback）
  socat TCP-LISTEN:8100,bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:"$DOCFLOW_AI_SOCK" &
  SOCAT_PID=$!
  sleep 0.3
  export HOME=/tmp/agent-home
  mkdir -p "$HOME"
  # Claude Code：自定义网关 + 任务令牌（bearer）+ 禁遥测/更新/非必要流量
  export ANTHROPIC_BASE_URL="http://127.0.0.1:8100"
  export ANTHROPIC_AUTH_TOKEN="$DOCFLOW_AI_TOKEN"
  # 模型名须在其 catalog 内（网关忽略该字段、统一平台默认模型），用
  # 占位名 + 关闭未知模型窗口强校验。
  export ANTHROPIC_MODEL="claude-sonnet-4-5"
  export CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1
  export DISABLE_TELEMETRY=1 DISABLE_AUTOUPDATER=1 DISABLE_BUG_COMMAND=1
  export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
  export CLAUDE_CONFIG_DIR="$HOME/.claude"
  PROMPT_TEXT="$(cat "$PROMPT_FILE" 2>/dev/null | tr -d '\r' || true)"
  if [ -z "$PROMPT_TEXT" ]; then
    kill "$SOCAT_PID" 2>/dev/null
    return 1
  fi
  # headless 非交互：-p 打印模式 + 跳过权限确认（沙箱本身就是权限边界）
  claude -p "$PROMPT_TEXT" --dangerously-skip-permissions --model claude-sonnet-4-5
  rc=$?
  kill "$SOCAT_PID" 2>/dev/null
  return $rc
}

run_pi() {
  command -v pi >/dev/null 2>&1 || return 1
  [ -n "${DOCFLOW_AI_TOKEN:-}" ] || return 1
  [ -S "${DOCFLOW_AI_SOCK:-}" ] || return 1
  socat TCP-LISTEN:8100,bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:"$DOCFLOW_AI_SOCK" &
  SOCAT_PID=$!
  sleep 0.3
  export HOME=/tmp/agent-home
  mkdir -p "$HOME/.pi/agent/extensions"
  # 经官方扩展机制注册自定义 provider（openai-completions 协议指向
  # 本机网关）——内置 openai provider 有地区合规门禁（403），自定义
  # provider 无此限制；apiKey 为 env 变量名，运行时读任务令牌。
  cat > "$HOME/.pi/agent/extensions/docflow.ts" <<'EOF'
export default function (pi) {
  pi.registerProvider('docflow', {
    name: 'DocFlow Platform',
    baseUrl: 'http://127.0.0.1:8100/v1',
    apiKey: 'DOCFLOW_AI_TOKEN',
    api: 'openai-completions',
    models: [{
      id: 'docflow-platform',
      name: 'DocFlow Platform Model',
      reasoning: false,
      input: ['text'],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      contextWindow: 200000,
      maxTokens: 8192,
    }],
  })
}
EOF
  export OPENAI_API_KEY="$DOCFLOW_AI_TOKEN"
  PROMPT_TEXT="$(cat "$PROMPT_FILE" 2>/dev/null | tr -d '\r' || true)"
  if [ -z "$PROMPT_TEXT" ]; then
    kill "$SOCAT_PID" 2>/dev/null
    return 1
  fi
  # --print 无头一次性执行（print 模式工具自动执行）；--offline/
  # --no-session/--no-extensions 适配断网沙箱（-e 显式加载 docflow 扩展）。
  pi --print --provider docflow --model docflow-platform --no-session --offline -e "$HOME/.pi/agent/extensions/docflow.ts" "$PROMPT_TEXT"
  rc=$?
  kill "$SOCAT_PID" 2>/dev/null
  return $rc
}

baseline
harness_ok=0
if [ "$HARNESS" = "claude-code" ] || { [ -z "$HARNESS" ] && command -v claude >/dev/null 2>&1; }; then
  run_claude && harness_ok=1
elif [ "$HARNESS" = "pi" ] || { [ -z "$HARNESS" ] && command -v pi >/dev/null 2>&1; }; then
  run_pi && harness_ok=1
fi
if [ "$harness_ok" != "1" ]; then
  echo "agent: external harness unavailable (mode=${HARNESS:-auto}), fallback to builtin runner" >&2
  node /app/runner.mjs
fi
# 收尾：git 变更集（A/M/D）→ .docflow-changes.json（平台 git 式同步消费）
node /app/changes.mjs
exit 0
