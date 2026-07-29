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

  Expand-Archive -Path $pkg -DestinationPath $tmp -Force
  $bin = Get-ChildItem -Path $tmp -Recurse -Filter 'cclaude*.exe' | Select-Object -First 1
  if (-not $bin) { throw 'cclaude: 压缩包里没找到 cclaude.exe' }
  Copy-Item $bin.FullName (Join-Path $dest 'cclaude.exe') -Force
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# 加进用户级 PATH（不动系统 PATH，不需要管理员）
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike "*$dest*") {
  [Environment]::SetEnvironmentVariable('Path', "$userPath;$dest", 'User')
  Write-Host ''
  Write-Host "已把 $dest 加进 PATH。**新开一个终端**后 cclaude 命令才可用。" -ForegroundColor Yellow
} else {
  Write-Host '运行 cclaude 即可开始。'
}
Write-Host "已安装到 $dest\cclaude.exe"
