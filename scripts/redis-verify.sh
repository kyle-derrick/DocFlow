#!/bin/bash
# Redis 双实例验证：起 redis + backend x2（18081/18082 直连）→ wscheck 跨实例断言
set -e
export PATH=/opt/go/bin:$PATH
cd /mnt/d/data/code/git/own/DocFlow

echo '=== [1] up redis (full profile 单服务) ==='
docker compose -f docker-compose.yml -f scripts/compose-scale.yml \
  --profile minimal --profile search --profile full up -d redis postgres 2>&1 | tail -2

echo '=== [2] up backend x2 + caddy ==='
docker compose -f docker-compose.yml -f scripts/compose-scale.yml \
  --profile minimal --profile search --profile full up -d --scale backend=2 2>&1 | tail -3

echo '=== [3] wait both instances ready ==='
for port in 18081 18082; do
  ok=0
  for i in $(seq 1 60); do
    if curl -sf -o /dev/null "http://127.0.0.1:$port/ready"; then ok=1; break; fi
    sleep 2
  done
  [ "$ok" = 1 ] && echo "instance :$port ready" || { echo "FATAL :$port not ready"; exit 1; }
done

echo '=== [4] seed admin ==='
docker compose -f docker-compose.yml -f scripts/compose-scale.yml run --rm seed 2>&1 | tail -1

echo '=== [5] wscheck (cross-instance) ==='
go run ./cmd/wscheck

echo '=== [6] restore single instance ==='
docker compose -f docker-compose.yml -f scripts/compose-scale.yml \
  --profile minimal --profile search --profile full up -d --scale backend=1 2>&1 | tail -2
