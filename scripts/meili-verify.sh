#!/bin/bash
# meili 联动验证：清库 + seed + 全量冒烟（search 段走真实 meili）
set -e
cd /mnt/d/data/code/git/own/DocFlow
echo '--- meili health ---'
docker compose exec -T meilisearch sh -c \
  "wget -qO- --header 'Authorization: Bearer verify-meili-master-key-0123456789abcdef' http://127.0.0.1:7700/health"
echo
echo '--- meili indexes ---'
docker compose exec -T meilisearch sh -c \
  "wget -qO- --header 'Authorization: Bearer verify-meili-master-key-0123456789abcdef' 'http://127.0.0.1:7700/indexes?limit=5'"
echo
tr -d '\r' < scripts/prod-smoke-reset.sh > /tmp/prod-smoke-reset.sh && bash /tmp/prod-smoke-reset.sh 2>&1 | tail -4
