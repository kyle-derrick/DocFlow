#!/bin/bash
# 校验 .env.production.example：占位符替换后 compose config 必须通过
set -e
cd "$(dirname "$0")/.."
sed -e 's/=CHANGE_ME.*/=prodtest-0123456789abcdef/' \
    -e 's|^DATABASE_URL=.*|DATABASE_URL=postgres://docflow:prodtest-0123456789abcdef@postgres:5432/docflow?sslmode=disable|' \
    .env.production.example > /tmp/env.prodtest
docker compose --env-file /tmp/env.prodtest -f docker-compose.yml \
  --profile minimal --profile full --profile antivirus --profile search --profile storage \
  config -q && echo PROD_TEMPLATE_CONFIG_OK
rm -f /tmp/env.prodtest
