#!/bin/bash
# 生产前核对 - 栈状态探查：容器健康 + /ready 六项 + caddy 入口
cd /mnt/d/data/code/git/own/DocFlow
echo '=== containers ==='
docker ps -a --format 'table {{.Names}}\t{{.Status}}' | grep -E 'docflow|NAMES'
echo '=== /ready (direct 18081/18082) ==='
for p in 18081 18082; do
  r=$(curl -s -m 3 "http://127.0.0.1:${p}/ready")
  [ -n "$r" ] && echo "${p}: ${r}"
done
echo '=== /ready via caddy :80 ==='
curl -s -m 3 -w '\nhttp=%{http_code}\n' http://127.0.0.1/ready
