#!/usr/bin/env bash
# 在 Docker 里交叉编译客户端——**不需要本机装 Go**。
#
# 用途：Console 主机上直接构建产物，省掉「开发机编译 → 传到 Console」这一步。
# 没有 SSH 密钥（比如只用 EC2 Instance Connect 连机器）时特别有用。
#
# 用法（在仓库根目录）：
#   VERSION=v0.1.0 ./scripts/build-release-docker.sh
#   VERSION=v0.1.0 TARGETS="darwin/arm64 darwin/amd64 windows/amd64" \
#     DIST_DIR=/srv/ccw-console/dist ./scripts/build-release-docker.sh
set -euo pipefail
cd "$(dirname "$0")/.."

: "${VERSION:?用法: VERSION=v0.1.0 ./scripts/build-release-docker.sh}"
DIST="${DIST_DIR:-dist}"
GO_IMAGE="golang:1.22-bookworm"   # 与 deploy/versions.lock 的 go-build-image 一致

mkdir -p "$DIST" 2>/dev/null || true
[ -d "$DIST" ] || { echo "错误：输出目录 ${DIST} 不存在且无法创建（父目录可能属于 root）"; exit 1; }
DIST_ABS="$(cd "$DIST" && pwd)"

# 开跑前就确认输出目录可写：否则要等完镜像拉取与依赖下载才在最后一刻失败。
# 典型场景：目录是 sudo mkdir 建的、属主 root，而容器以当前用户身份写入。
if ! touch "$DIST_ABS/.ccw-write-test" 2>/dev/null; then
  echo "错误：输出目录不可写 —— $DIST_ABS"
  echo "  当前属主：$(stat -c '%U:%G' "$DIST_ABS" 2>/dev/null || stat -f '%Su:%Sg' "$DIST_ABS")"
  echo "  当前用户：$(id -un):$(id -gn)"
  echo
  echo "修复：sudo chown -R \"\$USER\":\"\$USER\" $DIST_ABS"
  exit 1
fi
rm -f "$DIST_ABS/.ccw-write-test"

# -u 让产物属主是当前用户而不是 root（否则后续 cp/发布都要 sudo）；
# HOME/GOCACHE/GOMODCACHE 指到 /tmp，因为容器内当前用户没有家目录。
docker run --rm \
  -v "$PWD":/src -w /src \
  -v "$DIST_ABS":/out \
  -u "$(id -u):$(id -g)" \
  -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod \
  -e VERSION="$VERSION" -e DIST_DIR=/out -e TARGETS="${TARGETS:-}" \
  "$GO_IMAGE" ./scripts/build-release.sh

echo
echo "产物在 $DIST_ABS"
