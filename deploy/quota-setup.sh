#!/usr/bin/env bash
#
# ⚠️ 当前布局下本脚本不生效，不要接入任何部署流程。
#
# 本脚本创建的卷名是 <slug>-workspace，而 compose 实际使用的卷带项目前缀
# （deploy_<slug>-workspace）——两者不是同一个卷。因此脚本会正常退出并打印
# "capped at NN GiB"，容器却仍挂着不受约束的普通命名卷，且没有任何报错。
#
# 只有切到 external 卷布局后才可启用，见 docs/superpowers/plans/2026-07-26-compose-render-plan.md
# §4.2（方案B）：compose 用 external: true + 显式 name: 对齐本脚本的卷名，
# 且本脚本必须前移到 `docker compose up` 之前执行（否则 external 卷不存在，compose 直接失败）。
# 用户 2026-07-26 已定沿用普通命名卷（同文档 §4.4），故当前不启用；替代防线见
# docs/superpowers/specs/2026-07-25-ccw-console-fleet-design.md §12.1 的 N4 改写说明。
#
# 文件系统硬配额（Task 13）：给每个项目的 workspace 一个固定大小的 ext4 loop 文件系统，
# 防止 Claude 绕过同步接口把宿主机磁盘写满。用法：./quota-setup.sh <slug> <size_gib>
set -euo pipefail
SLUG="${1:?usage: quota-setup.sh <slug> <size_gib>}"
SIZE_GIB="${2:?usage: quota-setup.sh <slug> <size_gib>}"
IMG_DIR="/srv/ccw/quota"; MNT="/srv/ccw/workspaces/${SLUG}"; IMG="${IMG_DIR}/${SLUG}.img"
mkdir -p "$IMG_DIR" "$MNT"
if [ ! -f "$IMG" ]; then
  truncate -s "${SIZE_GIB}G" "$IMG"
  mkfs.ext4 -q -F "$IMG"
fi
if ! mountpoint -q "$MNT"; then
  mount -o loop "$IMG" "$MNT"
  chown 1001:1001 "$MNT"
fi
grep -qF "$MNT" /etc/fstab || echo "$IMG $MNT ext4 loop 0 0" >> /etc/fstab
VOL="${SLUG}-workspace"
docker volume inspect "$VOL" >/dev/null 2>&1 || \
  docker volume create --driver local --opt type=none --opt o=bind --opt device="$MNT" "$VOL" >/dev/null
echo "done: ${SLUG} workspace capped at ${SIZE_GIB}GiB"
