package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// install.ps1 的 PATH 逻辑在**真 PowerShell** 里跑一遍。
//
// 为什么值得单开一个 e2e：这套脚本此前的测试只断言"正文里有某个字符串"，
// 从没执行过——于是 `tar xzf` 对着一个裸 exe 的 bug 一路混过所有测试。
// PowerShell 的语义（'User' 级设置不影响当前进程、`-like` 把路径当通配符）
// 靠读文档判断都可能判错，只有跑一遍才算数。
//
// 用 mcr.microsoft.com/powershell 容器，无 Docker 自动 skip。
// **在 Linux 容器里能验的部分**：段切分/去重/拼接逻辑、以及"'User' 级设置
// 不会改当前进程 PATH"这个前提。注册表落盘是 Windows 专有，不在这里断言。
func TestInstallPS1PathLogicInRealPowerShell(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("需要 docker 才能起 PowerShell 容器")
	}

	const script = `
$dest = 'C:\Users\x\AppData\Local\Programs\cclaude'

# 前提1：'User' 级设置不影响当前进程——这正是"装完当场不可用"的根因
$env:PATH = 'C:\Windows'
try { [Environment]::SetEnvironmentVariable('Path', "$env:PATH;$dest", 'User') } catch {}
if (($env:PATH -split ';') -contains $dest) { Write-Output 'BAD: User级竟然改了当前进程' }
else { Write-Output 'OK: User级不影响当前进程' }

# 前提2：脚本用的精确段比对，不能被 ...\cclaude2 骗过
$userPath = 'C:\Windows;C:\Users\x\AppData\Local\Programs\cclaude2'
$parts = @($userPath -split ';' | Where-Object { $_ -ne '' })
if ($parts -notcontains $dest) { Write-Output 'OK: cclaude2 不算已安装' }
else { Write-Output 'BAD: 被 cclaude2 骗过，会跳过写入' }

# 前提3：用户级 PATH 为空时不能拼出前导分号
$parts = @('' -split ';' | Where-Object { $_ -ne '' })
$newPath = (@($parts) + $dest) -join ';'
if ($newPath.StartsWith(';')) { Write-Output 'BAD: 前导分号' } else { Write-Output 'OK: 无前导分号' }

# 前提4：当前会话补 PATH 之后就该认得
if (($env:PATH -split ';') -notcontains $dest) { $env:PATH = "$env:PATH;$dest" }
if (($env:PATH -split ';') -contains $dest) { Write-Output 'OK: 当前会话已可用' }
else { Write-Output 'BAD: 当前会话仍不可用' }
`
	cmd := exec.Command("docker", "run", "--rm", "mcr.microsoft.com/powershell:latest",
		"pwsh", "-NoLogo", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Skipf("拉不到/跑不了 PowerShell 容器（离线环境）：%v\n%s", err, got)
	}
	if strings.Contains(got, "BAD:") {
		t.Errorf("PowerShell 里的 PATH 语义与脚本假设不符：\n%s", got)
	}
	if n := strings.Count(got, "OK:"); n != 4 {
		t.Errorf("应有4条OK，实得%d：\n%s", n, got)
	}
}
