#!/usr/bin/env bash
# 发布流水线（console-fleet-design §3.2）：交叉编译六目标cclaude + 生成SHA256SUMS。
#
# 产物命名：cclaude_{version}_{os}_{arch}[.exe]——与ccw-console register-release
# 的扫描约定一致（cmd/ccw-console/register.go的artifactFilename），改一处必须同步另一处。
# 版本经-ldflags注入，cclaude --version可查（验收A3）。
# 二进制不注入任何域名（设计§6.7）：同一份产物对所有用户、所有节点通用。
set -euo pipefail

: "${VERSION:?用法: VERSION=v0.1.0 ./scripts/build-release.sh}"
DIST="${DIST_DIR:-dist}"
mkdir -p "$DIST"

# 默认编全六个目标；只需要部分平台时用 TARGETS 覆盖，例如：
#   TARGETS="darwin/arm64 darwin/amd64 windows/amd64" VERSION=v0.1.0 ./scripts/build-release.sh
# 没编的平台只会在 register-release 时打印一行警告，不影响发布。
# 注意 Mac 是两个目标：arm64（M系列）与 amd64（Intel）。
targets="${TARGETS:-}"
[ -n "$targets" ] || targets="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64"
for t in $targets; do
  os="${t%/*}"; arch="${t#*/}"
  out="$DIST/cclaude_${VERSION}_${os}_${arch}"
  [ "$os" = "windows" ] && out="${out}.exe"
  echo "build $out"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.buildVersion=${VERSION}" \
    -o "$out" ./cmd/cclaude
done

# 本地校验和文件（方便离线核对）；线上的/dist/SHA256SUMS由Console从数据库生成，
# 两者数值一致（register-release重新计算入库）。
(
  cd "$DIST"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum cclaude_"${VERSION}"_* > SHA256SUMS
  else
    shasum -a 256 cclaude_"${VERSION}"_* > SHA256SUMS
  fi
)

n=$(printf '%s\n' $targets | wc -l | tr -d ' ')
echo
echo "完成：$DIST/ 下 $n 个产物 + SHA256SUMS"
echo "下一步（在Console主机、产物同步到CCW_DIST_DIR之后）："
echo "  ccw-console register-release --version ${VERSION} --publish"
