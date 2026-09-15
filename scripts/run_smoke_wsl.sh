#!/usr/bin/env bash
# 修正冒烟脚本：用 python 生成 zip（WSL 无 zip 命令），然后执行。
sed -i 's|(cd /tmp/webpkg-src && zip -q /tmp/webpkg.zip index.html)|python3 /mnt/d/data/code/git/own/DocFlow/scripts/make_webpkg_zip.py|' /tmp/smoke-full.sh
bash /tmp/smoke-full.sh
