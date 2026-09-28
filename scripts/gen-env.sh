#!/usr/bin/env bash
# .env 初始化器（bash 版，与 gen-env.ps1 同构）：从模板生成 .env，密钥按需
# 「随机生成 / 手动输入」。
# 用法：
#   ./scripts/gen-env.sh                     # 交互式（每项可选生成/手动）
#   ./scripts/gen-env.sh --auto              # 全自动（全部随机生成，无人值守）
#   ./scripts/gen-env.sh --template deploy/env/.env.single-node.example
#   ./scripts/gen-env.sh --patch             # 现有 .env 仅替换占位符/补密钥
#   --force：允许覆盖已存在的 .env
# 生成规则见 gen-env.ps1 头注释（PG 密码 20 位字母数字并同步重写 DATABASE_URL；
# JWT 类 32 字节 URL-safe；SEED_ADMIN_* 给默认值可改）。
set -euo pipefail
cd "$(dirname "$0")/.."

TEMPLATE='.env.example'
AUTO=0
PATCH=0
FORCE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --auto) AUTO=1 ;;
    --patch) PATCH=1 ;;
    --force) FORCE=1 ;;
    --template) TEMPLATE="$2"; shift ;;
    *) echo "未知参数：$1（支持 --auto/--patch/--force/--template <path>）" >&2; exit 1 ;;
  esac
  shift
done

[[ -f "$TEMPLATE" ]] || { echo "模板不存在：$TEMPLATE" >&2; exit 1; }
EXISTS=0; [[ -f .env ]] && EXISTS=1
if [[ $EXISTS -eq 1 && $PATCH -eq 0 && $FORCE -eq 0 ]]; then
  echo '.env 已存在（不覆盖）。替换占位符请用 --patch；重建请用 --force。' >&2
  exit 1
fi
if [[ $PATCH -eq 1 && $EXISTS -eq 0 ]]; then
  echo '--patch 需要已存在的 .env。' >&2
  exit 1
fi

gen_password() { # 20 位字母数字（去易混字符）
  LC_ALL=C tr -dc 'A-HJ-NP-Za-km-z2-9' </dev/urandom | head -c 20 || true
}
gen_secret32() { # 32 字节 → URL-safe base64（无 +/）
  openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'
}

ask_secret() { # $1=标签 $2=hint $3=optional(1/0) $4=生成函数名；值经 stdout 返回
  #（标签/提示走 stderr，避免被 $() 捕获）；空输出 = 跳过（仅 optional）。
  local label="$1" hint="$2" optional="${3:-0}" genfn="$4" v manual
  echo "" >&2
  echo "$label" >&2
  [[ -n "$hint" ]] && echo "  $hint" >&2
  if [[ $optional -eq 1 ]]; then
    read -r -p "  回车 = 生成 / s = 跳过（留空）/ m = 手动输入：" v
    if [[ "$v" == "s" ]]; then return 0; fi
  else
    read -r -p "  回车 = 生成随机值 / m = 手动输入：" v
  fi
  if [[ -z "$v" ]]; then "$genfn"; return 0; fi
  if [[ "$v" != "m" ]]; then printf '%s\n' "$v"; return 0; fi
  read -r -p "  请输入（明文显示）：" manual >&2
  printf '%s\n' "$manual"
}

if [[ $PATCH -eq 1 ]]; then TEXT="$(cat .env)"; else TEXT="$(cat "$TEMPLATE")"; fi
MADE=()

set_kv() { # 追加或整行替换
  local k="$1" v="$2"
  if grep -qE "^$k=" <<<"$TEXT"; then
    TEXT="$(sed -E "s|^$k=.*$|$k=$v|" <<<"$TEXT")"
  else
    TEXT="$TEXT
$k=$v"
  fi
}

# 1) PostgreSQL 密码 + DATABASE_URL 联动
PG_HINT='自用建议直接生成；需接入既有数据库时手动输入'
PG_PASS="$(gen_password)"
if [[ $AUTO -eq 0 ]]; then
  PG_PASS="$(ask_secret 'PostgreSQL 密码（POSTGRES_PASSWORD，同步重写 DATABASE_URL）' "$PG_HINT" 0 gen_password)"
  [[ -z "$PG_PASS" ]] && PG_PASS="$(gen_password)"
fi
set_kv POSTGRES_PASSWORD "$PG_PASS"
set_kv DATABASE_URL "postgres://docflow:${PG_PASS}@postgres:5432/docflow?sslmode=disable"
MADE+=('POSTGRES_PASSWORD + DATABASE_URL')

# 2) JWT_SECRET
JWT="$(gen_secret32)"
if [[ $AUTO -eq 0 ]]; then
  JWT="$(ask_secret 'JWT_SECRET（会话签发密钥，≥32 字节）' '' 0 gen_secret32)"
  [[ -z "$JWT" ]] && JWT="$(gen_secret32)"
fi
[[ ${#JWT} -lt 32 ]] && { echo 'JWT_SECRET 至少 32 字节。' >&2; exit 1; }
set_kv JWT_SECRET "$JWT"
MADE+=('JWT_SECRET')

# 3) 初始管理员（AUTO = 默认值；交互 = 回车默认）
ask_default() { # $1 标签 $2 默认值
  local v
  if [[ $AUTO -eq 1 ]]; then printf '%s\n' "$2"; return 0; fi
  read -r -p "  $1（回车 = 默认 $2）：" v
  printf '%s\n' "${v:-$2}"
}
echo ''
EMAIL="$(ask_default '初始管理员邮箱（SEED_ADMIN_EMAIL）' 'admin@docflow.local')"
USERNAME="$(ask_default '初始管理员用户名（SEED_ADMIN_USERNAME）' 'admin')"
ADMIN_PASS="$(gen_password)"
if [[ $AUTO -eq 0 ]]; then
  ADMIN_PASS="$(ask_secret '初始管理员密码（SEED_ADMIN_PASSWORD，首次登录后可改）' '' 0 gen_password)"
  [[ -z "$ADMIN_PASS" ]] && ADMIN_PASS="$(gen_password)"
fi
set_kv SEED_ADMIN_EMAIL "$EMAIL"
set_kv SEED_ADMIN_USERNAME "$USERNAME"
set_kv SEED_ADMIN_PASSWORD "$ADMIN_PASS"
MADE+=('SEED_ADMIN_EMAIL/USERNAME/PASSWORD')

# 4) ONLYOFFICE_JWT_SECRET（可选；s = 跳过留空）
OO="$(gen_secret32)"
if [[ $AUTO -eq 0 ]]; then
  OO="$(ask_secret 'ONLYOFFICE_JWT_SECRET（full profile 才需要；minimal 可跳过）' '' 1 gen_secret32)"
fi
if [[ -n "$OO" ]]; then
  set_kv ONLYOFFICE_JWT_SECRET "$OO"
  MADE+=('ONLYOFFICE_JWT_SECRET')
fi

printf '%s' "$TEXT" > .env

echo ''
echo '==== .env 已生成 ===='
echo "文件：$PWD/.env（已 gitignore，请妥善备份）"
[[ ${#MADE[@]} -gt 0 ]] && echo "本次写入：${MADE[*]}"
echo ''
echo '初始管理员：'
echo "  邮箱/用户名：$EMAIL / $USERNAME"
echo "  密码：$ADMIN_PASS"
echo '  （首次登录后建议立即在「设置 → 账号安全」修改。）'
[[ -z "$OO" ]] && echo '  ONLYOFFICE_JWT_SECRET 未设置：minimal 不受影响；启用 full 时重跑 --patch 补齐。'
exit 0
