#!/usr/bin/env bash
# 把 src/*.mmd 渲染成同目录下的 SVG 与 PNG。
#
# 依赖：Node（用 npx 拉 mermaid-cli，首次需要联网）。
# 改了 .mmd 之后跑一次；产物与源码一起提交，读者不需要任何工具即可查看。
set -euo pipefail
cd "$(dirname "$0")"
VER="@mermaid-js/mermaid-cli@11.4.2"
for f in src/*.mmd; do
  n=$(basename "$f" .mmd)
  for ext in svg png; do
    npx --yes "$VER" -i "$f" -o "$n.$ext" \
      -c mermaid-config.json -p puppeteer-config.json -b "#ffffff" --scale 2 >/dev/null
  done
  echo "已渲染 $n"
done
