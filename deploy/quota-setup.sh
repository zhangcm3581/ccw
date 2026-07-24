#!/usr/bin/env bash
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
