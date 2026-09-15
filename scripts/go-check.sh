#!/bin/bash
export PATH=/opt/go/bin:$PATH
cd /mnt/d/data/code/git/own/DocFlow || exit 1
echo '=== go vet ==='
go vet ./... && echo VET_OK
echo '=== go test all ==='
go test ./... 2>&1 | grep -v '^ok' | head -20
go test ./... 2>&1 | grep -c '^ok'
echo '=== frontend build (node:20-alpine) ==='
cd frontend || exit 1
docker run --rm -v /mnt/d/data/code/git/own/DocFlow/frontend:/app -w /app -e CI=1 node:20-alpine \
  sh -c 'npm ci --no-audit --no-fund 2>&1 | tail -2 && npm run build 2>&1 | tail -6'
