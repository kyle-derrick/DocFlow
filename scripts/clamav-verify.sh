#!/bin/bash
set -e
cd "$(dirname "$0")/.."
C="docker compose -f docker-compose.yml -f scripts/compose-scale.yml --profile minimal --profile search --profile full --profile storage --profile antivirus"
$C build backend 2>&1 | tail -2
$C up -d --force-recreate backend 2>&1 | tail -1
for i in $(seq 1 60); do curl -sf -o /dev/null http://127.0.0.1:18081/ready && break; sleep 2; done
curl -sf http://127.0.0.1:18081/ready; echo ' (clamav fixed ready)'
$C up -d caddy 2>&1 | tail -1
tr -d '\r' < scripts/clamav-eicar.sh > /tmp/clamav-eicar.sh && bash /tmp/clamav-eicar.sh
