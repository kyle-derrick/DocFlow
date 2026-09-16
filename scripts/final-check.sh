#!/bin/bash
set -e
export PATH=/opt/go/bin:$PATH
cd "$(dirname "$0")/.."
echo '=== go vet ==='
go vet ./... && echo VET_OK
echo '=== unit tests (changed pkgs + full) ==='
go test ./... 2>&1 | grep -v '^ok ' | head -10 || true
echo TESTS_DONE
