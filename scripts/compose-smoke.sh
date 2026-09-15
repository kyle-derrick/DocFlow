#!/bin/bash
# compose 栈（caddy :80）上的全量冒烟：适配 base 与 Origin 后复用 runtime-smoke.sh
set -e
cd /mnt/d/data/code/git/own/DocFlow
tr -d '\r' < scripts/runtime-smoke.sh | \
  sed -e 's|base=http://127.0.0.1:18080|base=http://127.0.0.1|' \
      -e 's|Origin: http://127.0.0.1:18080|Origin: http://127.0.0.1|' > /tmp/smoke-compose.sh
bash /tmp/smoke-compose.sh 2>&1 | tee /tmp/smoke-compose-result.txt | tail -3
grep -c '^PASS' /tmp/smoke-compose-result.txt || true
grep '^FAIL' /tmp/smoke-compose-result.txt || echo ALL_PASS_COMPOSE
