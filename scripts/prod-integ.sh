#!/bin/bash
# 生产前核对 - 串行跑全部集成验证脚本
cd /mnt/d/data/code/git/own/DocFlow
for s in meili-verify s3-verify smtp-verify clamav-verify clamav-eicar onlyoffice-verify redis-verify; do
  echo "##### ${s}"
  tr -d '\r' < "scripts/${s}.sh" > /tmp/v.sh && bash /tmp/v.sh 2>&1 | tail -8
  echo
done
