#!/usr/bin/env bash
# 生成节点源码包（本地开发用；生产由deploy/Dockerfile.console在镜像里生成）。
#
# 纳管时这个包被推送到节点并解开，节点靠它构建control-api/worker-agent/项目镜像。
# 排除规则必须与Dockerfile.console保持一致。
set -euo pipefail
cd "$(dirname "$0")/.."
OUT="${1:-node-src.tar.gz}"
tar czf "$OUT" \
  --exclude=./.git --exclude=./docs --exclude=./dist \
  --exclude=./.github --exclude=./tests --exclude="./$OUT" \
  -C . .
echo "已生成 ${OUT}（$(du -h "$OUT" | cut -f1)）"
echo "本地运行Console时：export CCW_NODE_SRC=$(pwd)/${OUT}"
