# cclaude 安装脚本（Windows PowerShell）
#
#   irm {{.Site}}/install.ps1 | iex
#
# 做的事：探测架构 → 从本站下载 → 校验 SHA256 → 装进用户级 PATH。
# 校验和内嵌在脚本里（由 Console 按当前已发布版本渲染）。
$ErrorActionPreference = 'Stop'

$Version = '{{.Version}}'
$Base    = '{{.Site}}'

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { 'amd64' }
  'ARM64' { 'arm64' }
  default { throw "cclaude: 不支持的架构 $($env:PROCESSOR_ARCHITECTURE)" }
}

$file = $null
$sum  = $null
{{range .Arts}}{{if eq .OS "windows"}}if ($arch -eq '{{.Arch}}') { $file = '{{.Filename}}'; $sum = '{{.SHA256}}' }
{{end}}{{end}}
if (-not $file) { throw "cclaude: 本站当前版本没有 windows/$arch 的产物" }

# 装到用户目录，不需要管理员权限
$dest = Join-Path $env:LOCALAPPDATA 'Programs\cclaude'
New-Item -ItemType Directory -Force -Path $dest | Out-Null

$old = $null
$exePath = Join-Path $dest 'cclaude.exe'
if (Test-Path $exePath) {
  try { $old = (& $exePath --version 2>$null) -split ' ' | Select-Object -Last 1 } catch { $old = $null }
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
try {
  Write-Host "cclaude $Version · windows/$arch"
  Write-Host "下载 $Base/dist/$file"
  $pkg = Join-Path $tmp $file
  Invoke-WebRequest -Uri "$Base/dist/$file" -OutFile $pkg -UseBasicParsing

  $got = (Get-FileHash -Path $pkg -Algorithm SHA256).Hash.ToLower()
  if ($got -ne $sum.ToLower()) {
    throw "cclaude: 校验和不符，已中止`n  期望 $sum`n  实际 $got"
  }

  # 产物就是 exe 本身（scripts/build-release.sh 直接 go build 出来，不打包），不解压。
  #
  # **正在运行的 exe 会被 Windows 锁住**，覆盖会失败。默认的 $ErrorActionPreference='Stop'
  # 会把它抛成一段 .NET 异常，看的人不知道该做什么——所以自己接住，说清楚。
  $exe = Join-Path $dest 'cclaude.exe'
  try {
    Copy-Item $pkg $exe -Force
  } catch {
    throw "cclaude: 写入 $exe 失败。若 cclaude 正在运行，请先关掉那个窗口再重试。（$($_.Exception.Message)）"
  }
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# 旧版本（如果有）：重装最该看到的就是"从哪个版本换到了哪个版本"。
# 取不到就当没有——绝不因为版本探测失败而中止安装。
if ($old -and $old -ne $Version) {
  Write-Host "已从 $old 升级到 $Version"
} elseif ($old) {
  Write-Host "已重新安装 $Version（与原有版本相同）"
}

# 加进用户级 PATH（不动系统 PATH，不需要管理员）。
#
# 按分号切开做**精确段比对**，不是 `-notlike "*$dest*"`：后者会把已有的
# `...\cclaude2` 之类当成"已经装过"而跳过写入，于是命令永远不可用——
# 连开新终端都救不回来，因为 PATH 里压根没加。
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath) { $userPath = '' }
$parts = @($userPath -split ';' | Where-Object { $_ -ne '' })
if ($parts -notcontains $dest) {
  # 用切好的段重新拼：用户级 PATH 为空时 "$userPath;$dest" 会留一个前导分号。
  [Environment]::SetEnvironmentVariable('Path', ((@($parts) + $dest) -join ';'), 'User')
}

# **当前会话也要立刻可用。**`irm ... | iex` 就跑在这个会话里，而 'User' 级
# 设置只对**将来启动的进程**生效——所以光设注册表的话，装完在同一个窗口敲
# cclaude 必然是"不是可识别的命令"。这一步就是那句"请新开一个终端"的替代品。
if (($env:PATH -split ';') -notcontains $dest) {
  $env:PATH = "$env:PATH;$dest"
}

# 同步目录：装完就在桌面上出现。**用 .NET 的 Desktop 已知文件夹**而不是
# 拼 $HOME\Desktop——桌面常被 OneDrive 重定向，拼出来的那个用户根本看不见。
$desktop = [Environment]::GetFolderPath('Desktop')
if (-not $desktop) { $desktop = Join-Path $env:USERPROFILE 'Desktop' }
$syncDir = if ($env:CCLAUDE_SYNC_DIR) { $env:CCLAUDE_SYNC_DIR } else { Join-Path $desktop 'cclaude 同步目录' }
try {
  New-Item -ItemType Directory -Force -Path $syncDir | Out-Null
  Write-Host "同步目录：$syncDir"
} catch {
  Write-Host '（同步目录建不了，首次运行 cclaude 时会再试一次）'
}

Write-Host "已安装到 $dest\cclaude.exe"

# **PATH 遮挡**：别处有一个更靠前的 cclaude 时，装了新的也还是跑旧的。
# 这种"升级了但没生效"极难自己看出来。
$found = Get-Command cclaude -ErrorAction SilentlyContinue
if ($found -and $found.Source -and $found.Source -ne $exePath) {
  Write-Host ''
  Write-Host "⚠ PATH 里更靠前的位置还有一个 cclaude：$($found.Source)" -ForegroundColor Yellow
  Write-Host "  现在敲 cclaude 跑的是它，不是刚装的这个。删掉它，或把 $dest 提到 PATH 前面。"
}
Write-Host '当前窗口已可用，新开的终端也会有。' -ForegroundColor Green
Write-Host ''
Write-Host '连接你的节点：' -NoNewline
Write-Host '  cclaude --api https://<你的接入域名>' -ForegroundColor Cyan
