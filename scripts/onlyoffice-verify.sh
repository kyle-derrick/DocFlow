#!/bin/bash
set -e
cd "$(dirname "$0")/.."
C="docker compose -f docker-compose.yml -f scripts/compose-scale.yml --profile minimal --profile search --profile full --profile storage --profile antivirus"
echo '--- wait stack stable ---'
for i in $(seq 1 60); do curl -sf -o /dev/null http://127.0.0.1/ready && break; sleep 3; done
curl -sf -o /dev/null http://127.0.0.1/ready || { $C up -d 2>&1 | tail -2; for i in $(seq 1 60); do curl -sf -o /dev/null http://127.0.0.1/ready && break; sleep 3; done; }
echo '--- wait DS ready ---'
ok=0
for i in $(seq 1 40); do
  code=$($C exec -T onlyoffice wget -q -S -O /dev/null http://localhost/healthcheck 2>&1 | head -1 | grep -oE '[0-9]{3}' | head -1)
  [ "$code" = "200" ] && { ok=1; echo "DS ok (${i}x5s)"; break; }
  sleep 5
done
[ "$ok" = 1 ] || { echo DS_NOT_READY; exit 1; }
python3 scripts/onlyoffice-callback.py
