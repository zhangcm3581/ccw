#!/usr/bin/env bash
# 卸载 ccw 部署，用于彻底重装。
#
# 用法（在 deploy/ 目录执行）：
#   ./uninstall.sh                 停服务 + 删所有数据卷（含数据库、Claude凭据、workspace）
#   ./uninstall.sh --purge-images  以上 + 删除本地构建的镜像
#
# ⚠ 会删除全部数据卷（数据库、已登录的Claude凭据、项目workspace、同步状态）。
#   重装本就要全新开始，这是预期行为。如需保留数据库，改用 `docker compose down`（不加 -v）。
set -euo pipefail
cd "$(dirname "$0")"

MODE="${1:-}"

echo "==> 停止并删除容器、网络、数据卷"
docker compose down -v --remove-orphans

# 早期版本用的是独立 claude 卷（project-a-claude / project-b-claude），
# 新版已改为共享卷 claude-shared，上面的 down -v 不会删到这些遗留卷，显式清理：
echo "==> 清理早期版本遗留的独立 Claude 卷（若有）"
docker volume ls -q | grep -E 'project-[ab]-claude$' | xargs -r docker volume rm 2>/dev/null || true

if [ "$MODE" = "--purge-images" ]; then
  echo "==> 删除本地构建的镜像"
  docker compose down --rmi local 2>/dev/null || true
  docker rmi ccw-claude:latest 2>/dev/null || true
fi

echo "==> 卸载完成。"
echo "    残留检查（应为空）："
docker compose ps -a 2>/dev/null || true
echo "    卷检查："
docker volume ls | grep -E 'ccw|claude|project-[ab]|caddy' || echo "      （无残留卷）"
