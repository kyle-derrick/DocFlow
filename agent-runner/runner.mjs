#!/usr/bin/env node
// agent-runner：DocFlow Agent 镜像 ENTRYPOINT（Dockerfile.agent，纯 Node
// 无第三方依赖）。平台（internal/agent）以如下契约驱动本进程：
//   - env DOCFLOW_PROMPT_FILE：任务 prompt 文件（只读挂载 /run/docflow/prompt）
//   - env DOCFLOW_WORKSPACE：工作目录（快照导出的临时 workspace，rw 挂载）
//   - env DOCFLOW_AI_SOCK / DOCFLOW_AI_TOKEN（可选）：平台 AI IPC unix
//     socket 与一次性任务令牌——容器 NetworkMode=none 断网仍可经 unix
//     domain socket 调平台默认对话模型（POST /chat，Bearer 令牌）
//   - 产物同步：收尾将 git 基线后的变更（git status --porcelain → A/M/D）
//     写入 /workspace/.docflow-changes.json，平台侧按其构造 diff
//     （agent.sync_mode=git；文件自身加入 .gitignore 防自递归）
//
// 运行模式：
//   1. AI 工具循环（DOCFLOW_AI_TOKEN 存在）：模型每轮回 JSON——
//      {"action":"run","cmd":"shell 命令"} 或 {"action":"done","summary":"..."}；
//      上限 24 轮，命令超时 120s，stdout/stderr 合并截 8000 字符回喂。
//   2. 降级模式（无令牌）：不调 AI，仅顺序执行 prompt 中的 ```run 代码块
//      （每块一条 shell 命令）——镜像无 token 也可用。
//
// 无论成败 exit 0（平台以 .docflow-changes.json 与任务状态判定结果，
// 不以容器退出码区分成败）。
import { exec, execFile } from 'node:child_process';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import http from 'node:http';
import { promisify } from 'node:util';

const execAsync = promisify(exec);
const execFileAsync = promisify(execFile);

const PROMPT_FILE = process.env.DOCFLOW_PROMPT_FILE || '/run/docflow/prompt';
const WORKSPACE = process.env.DOCFLOW_WORKSPACE || '/workspace';
const AI_SOCK = process.env.DOCFLOW_AI_SOCK || '';
const AI_TOKEN = process.env.DOCFLOW_AI_TOKEN || '';
const CHANGES_FILE = '.docflow-changes.json';
const MAX_ROUNDS = 24;
const CMD_TIMEOUT_MS = 120_000;
const OUTPUT_LIMIT = 8_000;

// 默认忽略清单（与平台 diffIgnoreDirs/diffIgnoreFiles 一致：依赖/版本库/
// 构建缓存/IDE 配置不入产物 diff），另加清单文件自身防自递归。
const DEFAULT_GITIGNORE = [
  'node_modules/', '.git/', '.hg/', '.svn/', '.venv/', 'venv/',
  '__pycache__/', '.mypy_cache/', '.pytest_cache/', 'dist/', 'build/',
  'out/', 'target/', '.next/', '.nuxt/', '.cache/', '.gradle/', '.idea/',
  '.vscode/', 'coverage/', '.terraform/', 'bower_components/', 'vendor/pkg/',
  '.DS_Store', 'Thumbs.db', '*.pyc', CHANGES_FILE,
].join('\n') + '\n';

const SYSTEM_PROMPT = [
  'You are DocFlow Agent, an autonomous coding assistant running inside a sandboxed container.',
  'The container has NO network access. You interact with the outside world only by executing local shell commands.',
  'A git baseline commit of the initial workspace has been created for you; your file changes are tracked automatically.',
  'PROTOCOL: reply with ONE JSON object and nothing else. Two actions are allowed:',
  '  {"action":"run","cmd":"<single shell command to execute in the workspace>"}',
  '  {"action":"done","summary":"<short summary of what you accomplished>"}',
  'Rules:',
  '- Inspect the workspace first (e.g. ls, find, cat) before editing files.',
  '- One command per turn; it runs with a 120s timeout in ' + WORKSPACE + '.',
  '- Command output (stdout+stderr, truncated) is fed back to you in the next turn.',
  '- When the task is complete, or you cannot make progress, respond with the "done" action.',
].join('\n');

function log(...args) { process.stderr.write(args.join(' ') + '\n'); }

// runCommand 执行单条 shell 命令（/bin/sh -c），stdout/stderr 合并截断。
async function runCommand(cmd) {
  try {
    const { stdout, stderr } = await execAsync(cmd, {
      cwd: WORKSPACE, timeout: CMD_TIMEOUT_MS, maxBuffer: 4 << 20,
      env: { ...process.env, HOME: '/tmp' },
    });
    return `exit code 0\n${truncate(stdout + (stderr ? '\n[stderr]\n' + stderr : ''))}`;
  } catch (err) {
    const out = ((err.stdout || '') + (err.stderr ? '\n[stderr]\n' + err.stderr : '')) || String(err.message || err);
    const code = err.killed ? 'timeout' : (err.code ?? '?');
    return `exit code ${code}\n${truncate(out)}`;
  }
}

function truncate(text) {
  const s = String(text);
  return s.length > OUTPUT_LIMIT ? s.slice(0, OUTPUT_LIMIT) + `\n...(truncated, ${s.length} chars total)` : s;
}

async function git(args) {
  const { stdout } = await execFileAsync('git', args, { cwd: WORKSPACE, timeout: 60_000 });
  return stdout;
}

// aiChat 经 unix socket 调平台 AI（POST /chat）。非 2xx 抛错（含 429/502）。
function aiChat(messages, maxTokens) {
  return new Promise((resolve, reject) => {
    const body = JSON.stringify({ messages, max_tokens: maxTokens || 0 });
    const req = http.request({
      socketPath: AI_SOCK, path: '/chat', method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Content-Length': Buffer.byteLength(body),
        Authorization: 'Bearer ' + AI_TOKEN,
      },
      timeout: 180_000,
    }, (res) => {
      let data = '';
      res.setEncoding('utf8');
      res.on('data', (c) => { data += c; });
      res.on('end', () => {
        if (res.statusCode < 200 || res.statusCode >= 300) {
          reject(new Error(`ai /chat HTTP ${res.statusCode}: ${data.slice(0, 200)}`));
          return;
        }
        try {
          const parsed = JSON.parse(data);
          resolve({ content: String(parsed.content || ''), model: String(parsed.model || '') });
        } catch (e) {
          reject(new Error('ai /chat returned invalid JSON: ' + data.slice(0, 200)));
        }
      });
    });
    req.on('timeout', () => req.destroy(new Error('ai /chat timeout')));
    req.on('error', reject);
    req.end(body);
  });
}

// extractAction 从模型回复中抽取 JSON 动作：直接解析失败时剥掉 markdown
// 代码围栏 / 前后噪声后按首尾大括号截取再解析。
function extractAction(text) {
  const attempt = (s) => {
    try {
      const obj = JSON.parse(s);
      if (obj && typeof obj === 'object' && typeof obj.action === 'string') return obj;
    } catch { /* fallthrough */ }
    return null;
  };
  let direct = attempt(text.trim());
  if (direct) return direct;
  const stripped = text.replace(/```(?:json)?/gi, '');
  const start = stripped.indexOf('{');
  const end = stripped.lastIndexOf('}');
  if (start >= 0 && end > start) direct = attempt(stripped.slice(start, end + 1));
  return direct;
}

// gitBaseline 打基线：init + 默认 .gitignore（缺失时）+ baseline 提交。
async function gitBaseline() {
  const gitignorePath = join(WORKSPACE, '.gitignore');
  if (!existsSync(gitignorePath)) {
    writeFileSync(gitignorePath, DEFAULT_GITIGNORE);
  } else {
    const current = readFileSync(gitignorePath, 'utf8');
    // 既有 .gitignore 也确保忽略清单文件自身（幂等：无该行才追加）。
    if (!current.split('\n').some((l) => l.trim() === CHANGES_FILE)) {
      writeFileSync(gitignorePath, current.replace(/\n*$/, '\n') + CHANGES_FILE + '\n');
    }
  }
  if (!existsSync(join(WORKSPACE, '.git'))) await git(['init', '-q']);
  await git(['add', '-A']);
  await git(['-c', 'user.email=agent@docflow', '-c', 'user.name=agent', 'commit', '-q', '--allow-empty', '-m', 'baseline']);
}

// aiLoop AI 工具循环（协议见 SYSTEM_PROMPT）；任何 AI 失败即中断循环，
// 已产生的文件变更仍在收尾时同步。
async function aiLoop(prompt) {
  const messages = [
    { role: 'system', content: SYSTEM_PROMPT },
    { role: 'user', content: prompt },
  ];
  for (let round = 1; round <= MAX_ROUNDS; round++) {
    let reply;
    try {
      reply = await aiChat(messages, 0);
    } catch (err) {
      log(`agent-runner: ai round ${round} failed: ${err.message || err}`);
      return;
    }
    const action = extractAction(reply.content);
    if (!action) {
      messages.push({ role: 'assistant', content: reply.content });
      messages.push({ role: 'user', content: 'Invalid reply: your previous message was not a single JSON object. Respond ONLY with {"action":"run","cmd":"..."} or {"action":"done","summary":"..."}.' });
      continue;
    }
    if (action.action === 'done') {
      log(`agent-runner: done — ${String(action.summary || '').slice(0, 500)}`);
      return;
    }
    if (action.action === 'run' && typeof action.cmd === 'string' && action.cmd.trim() !== '') {
      const output = await runCommand(action.cmd);
      messages.push({ role: 'assistant', content: JSON.stringify({ action: 'run', cmd: action.cmd }) });
      messages.push({ role: 'user', content: `COMMAND OUTPUT (${action.cmd.slice(0, 200)})\n${output}\nContinue with the next JSON action.` });
      continue;
    }
    messages.push({ role: 'assistant', content: reply.content });
    messages.push({ role: 'user', content: 'Invalid action. Respond ONLY with {"action":"run","cmd":"..."} or {"action":"done","summary":"..."}.' });
  }
  log(`agent-runner: reached max rounds (${MAX_ROUNDS})`);
}

// degradedRun 降级模式：无 AI 令牌时仅执行 prompt 中的 ```run 代码块
// （每块一条命令，顺序执行）。
async function degradedRun(prompt) {
  const blocks = [...prompt.matchAll(/```run[ \t]*\r?\n([\s\S]*?)```/g)].map((m) => m[1].trim()).filter(Boolean);
  if (blocks.length === 0) log('agent-runner: no ai token and no ```run blocks in prompt; nothing to execute');
  for (const cmd of blocks) {
    log(`agent-runner: run block: ${cmd.slice(0, 200)}`);
    const output = await runCommand(cmd);
    log(output);
  }
}

// unquoteGitPath 解析 git status --porcelain 的 C 风格引号路径
// （"a\"b" / "\346\226\207" 八进制转义等）。
function unquoteGitPath(path) {
  if (path.length < 2 || !path.startsWith('"') || !path.endsWith('"')) return path;
  const inner = path.slice(1, -1);
  let out = '';
  for (let i = 0; i < inner.length; i++) {
    const ch = inner[i];
    if (ch !== '\\' || i + 1 >= inner.length) { out += ch; continue; }
    const next = inner[++i];
    switch (next) {
      case 'a': out += '\x07'; break;
      case 'b': out += '\x08'; break;
      case 'f': out += '\x0c'; break;
      case 'n': out += '\n'; break;
      case 'r': out += '\r'; break;
      case 't': out += '\t'; break;
      case 'v': out += '\x0b'; break;
      case '\\': out += '\\'; break;
      case '"': out += '"'; break;
      default:
        // 八进制转义（最多 3 位）。
        if (next >= '0' && next <= '7') {
          let oct = next;
          while (oct.length < 3 && inner[i + 1] >= '0' && inner[i + 1] <= '7') oct += inner[++i];
          out += String.fromCharCode(parseInt(oct, 8));
        } else {
          out += next;
        }
    }
  }
  return out;
}

// parsePorcelain 解析 `git status --porcelain`：XY PATH（重命名
// "R  old -> new" 取新路径并映射为 M）。X（已暂存状态）映射 A/M/D，
// R→M；其余（?/!/空等）忽略。
function parsePorcelain(stdout) {
  const changes = [];
  const seen = new Set();
  for (const line of stdout.split('\n')) {
    if (line.length < 4) continue;
    const x = line[0];
    let pathPart = line.slice(3);
    if (pathPart.includes(' -> ')) pathPart = pathPart.split(' -> ').pop();
    const path = unquoteGitPath(pathPart.trim());
    let status;
    if (x === 'A') status = 'A';
    else if (x === 'M') status = 'M';
    else if (x === 'D') status = 'D';
    else if (x === 'R') status = 'M';
    else continue;
    if (!path || seen.has(path + status)) continue;
    seen.add(path + status);
    changes.push({ path, status });
  }
  changes.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  return changes;
}

// finalize 收尾：git add -A + status --porcelain → 写变更清单。
async function finalize() {
  let changes = [];
  try {
    await git(['add', '-A']);
    const status = await git(['status', '--porcelain']);
    changes = parsePorcelain(status);
  } catch (err) {
    log(`agent-runner: finalize failed: ${err.message || err}`);
  }
  try {
    writeFileSync(join(WORKSPACE, CHANGES_FILE), JSON.stringify({ changes }, null, 2) + '\n');
  } catch (err) {
    log(`agent-runner: unable to write ${CHANGES_FILE}: ${err.message || err}`);
  }
}

async function main() {
  let prompt = '';
  try {
    // 行尾归一化：Windows CRLF 的 \r 会混入命令与文件名（如
    // `> out.txt\r`），统一转 \n。
    prompt = readFileSync(PROMPT_FILE, 'utf8').replace(/\r\n?/g, '\n');
  } catch (err) {
    log(`agent-runner: unable to read prompt (${PROMPT_FILE}): ${err.message || err}`);
  }
  try {
    await gitBaseline();
  } catch (err) {
    log(`agent-runner: git baseline failed: ${err.message || err}`);
  }
  if (prompt) {
    if (AI_SOCK && AI_TOKEN) {
      await aiLoop(prompt);
    } else {
      log('agent-runner: no DOCFLOW_AI_TOKEN, degraded mode (```run blocks only)');
      await degradedRun(prompt);
    }
  }
  await finalize();
}

main().catch((err) => {
  log('agent-runner: unexpected failure: ' + (err && (err.stack || err.message || String(err))));
}).then(() => {
  // 契约：无论成败 exit 0（见文件头注释）。
  process.exit(0);
});
