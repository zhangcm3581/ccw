#!/bin/sh
# cclaude 安装脚本（macOS / Linux）
#
#   curl -fsSL {{.Site}}/install.sh | sh
#
# 做的事：探测平台 → 从本站下载对应产物 → 校验 SHA256 → 装进 PATH。
# **校验和内嵌在脚本里**（由 Console 按当前已发布版本渲染），不额外去取一份——
# 从同一个源同时拿产物与校验和，校验不了任何东西。
set -eu

VERSION='{{.Version}}'
BASE='{{.Site}}'

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) echo "cclaude: 不支持的系统 $(uname -s)。Windows 请用：" >&2
     echo "  PowerShell:  irm {{.Site}}/install.ps1 | iex" >&2
     echo "  cmd.exe:     powershell -c \"irm {{.Site}}/install.ps1 | iex\"" >&2
     exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "cclaude: 不支持的架构 $(uname -m)" >&2; exit 1 ;;
esac

FILE=""
SUM=""
{{range .Arts}}if [ "$OS" = "{{.OS}}" ] && [ "$ARCH" = "{{.Arch}}" ]; then FILE='{{.Filename}}'; SUM='{{.SHA256}}'; fi
{{end}}
if [ -z "$FILE" ]; then
  echo "cclaude: 本站当前版本没有 $OS/$ARCH 的产物" >&2
  exit 1
fi

# 装到哪：CCLAUDE_INSTALL_DIR 优先；否则能写 /usr/local/bin 就装那儿（全局可用），
# 再否则退到 ~/.local/bin。
# 不自动 sudo——脚本从管道里跑，弹密码提示是很糟的体验，也不该由它替你提权。
if [ -n "${CCLAUDE_INSTALL_DIR:-}" ]; then
  DEST="$CCLAUDE_INSTALL_DIR"
  mkdir -p "$DEST"
elif [ -w /usr/local/bin ] 2>/dev/null; then
  DEST=/usr/local/bin
else
  DEST="$HOME/.local/bin"
  mkdir -p "$DEST"
fi

# 旧版本（如果有）：重装最该看到的就是"从哪个版本换到了哪个版本"，
# 否则"到底换上了没有"完全看不出来。取不到就当没有，绝不因此中止安装。
OLD=""
if [ -x "$DEST/cclaude" ]; then
  OLD="$("$DEST/cclaude" --version 2>/dev/null | awk '{print $2}' || true)"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "cclaude $VERSION · $OS/$ARCH"
echo "下载 $BASE/dist/$FILE"
curl -fsSL -o "$TMP/$FILE" "$BASE/dist/$FILE"

# 校验：sha256sum(Linux) 与 shasum(macOS) 二选一，都没有就明确报错而不是跳过。
GOT=""
if command -v sha256sum >/dev/null 2>&1; then
  GOT="$(sha256sum "$TMP/$FILE" | cut -d' ' -f1)"
elif command -v shasum >/dev/null 2>&1; then
  GOT="$(shasum -a 256 "$TMP/$FILE" | cut -d' ' -f1)"
else
  echo "cclaude: 找不到 sha256sum 或 shasum，无法校验完整性，已中止" >&2
  exit 1
fi
if [ "$GOT" != "$SUM" ]; then
  echo "cclaude: 校验和不符，已中止" >&2
  echo "  期望 $SUM" >&2
  echo "  实际 $GOT" >&2
  exit 1
fi

# 产物就是可执行文件本身（scripts/build-release.sh 直接 go build 出来，不打包），
# 所以不解压——装错成"解压一个不是压缩包的文件"会在这里当场失败。
chmod +x "$TMP/$FILE"
mv "$TMP/$FILE" "$DEST/cclaude"

if [ -n "$OLD" ] && [ "$OLD" != "$VERSION" ]; then
  echo "已从 $OLD 升级到 $VERSION"
elif [ -n "$OLD" ]; then
  echo "已重新安装 ${VERSION}（与原有版本相同）"
fi
# 同步目录：装完就在桌面上出现，用户第一眼就知道文件该往哪放。
# 名字必须与客户端的 SyncDirName 一致（有测试比对）。
SYNC_DIR="${CCLAUDE_SYNC_DIR:-$HOME/Desktop/cclaude 同步目录}"
if mkdir -p "$SYNC_DIR" 2>/dev/null; then
  echo "同步目录：$SYNC_DIR"
else
  echo "（同步目录建不了，首次运行 cclaude 时会再试一次）"
fi

echo "已安装到 $DEST/cclaude"

# **PATH 遮挡**：别的位置有一个更靠前的 cclaude 时，装了新的也还是跑旧的。
# 这种"升级了但没生效"极难自己看出来——表现是明明重装过，行为还是老的
# （比如连新节点报 workspace required）。宁可吵一句。
hash -r 2>/dev/null || true
WHICH="$(command -v cclaude 2>/dev/null || true)"
if [ -n "$WHICH" ] && [ "$WHICH" != "$DEST/cclaude" ]; then
  echo
  echo "⚠ PATH 里更靠前的位置还有一个 cclaude：$WHICH"
  echo "  现在敲 cclaude 跑的是它，不是刚装的这个。删掉它，或把 $DEST 提到 PATH 前面。"
fi

case ":$PATH:" in
  *":$DEST:"*) echo "运行 cclaude 即可开始。" ;;
  *) echo
     echo "⚠ $DEST 不在 PATH 里。把下面这行加进 ~/.zshrc 或 ~/.bashrc："
     echo "    export PATH=\"$DEST:\$PATH\""
     echo "  然后重开终端，或先执行：export PATH=\"$DEST:\$PATH\"" ;;
esac
