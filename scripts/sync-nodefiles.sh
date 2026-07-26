#!/usr/bin/env bash
# 把deploy/下的节点编排文件同步到cmd/ccw-console/nodefiles/（go:embed需要包内路径）。
# 改了deploy/Caddyfile或任一Dockerfile之后跑一次；CI会校验两边一致。
set -euo pipefail
cd "$(dirname "$0")/.."
for f in Caddyfile Dockerfile.claude Dockerfile.control-api Dockerfile.worker-agent; do
  cp "deploy/$f" "cmd/ccw-console/nodefiles/$f"
  echo "synced $f"
done
