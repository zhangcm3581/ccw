package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// 同步目录（2026-07-31）。
//
// 此前 cclaude 同步的是**你碰巧所在的那个目录**，于是"在哪跑"变成了一个必须
// 记住的隐式状态：在 ~/code 跑过，再进 ~/work 跑，是两个云端工作区，而命令行
// 上看不出任何区别。
//
// 现在有一个固定的落点：桌面上的「cclaude 同步目录」。丢进去的文件夹自动成为
// 项目；不想搬家的目录可以**就地登记**，文件一个都不动（registry.go）。
//
// 目录名与桌面位置都可以用 CCLAUDE_SYNC_DIR 覆盖——这是给不用桌面的人、
// 以及测试用的。

// SyncDirName是桌面上那个文件夹的名字。与安装脚本里创建的必须一致。
const SyncDirName = "cclaude 同步目录"

// syncRoot返回同步目录的绝对路径。CCLAUDE_SYNC_DIR优先。
func syncRoot() string {
	if v := os.Getenv("CCLAUDE_SYNC_DIR"); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	return filepath.Join(desktopDir(), SyncDirName)
}

// desktopDir定位桌面。
//
// **OneDrive 重定向要认**：Windows 上桌面常被 OneDrive 接管，此时
// %USERPROFILE%\Desktop 可能不存在或不是用户实际看到的那个桌面。按存在性
// 依次探测，都不存在时退回 ~/Desktop（随后会被创建）。
//
// 中文版 Windows 的桌面文件夹在磁盘上仍然叫 Desktop，显示名才是"桌面"，
// 所以不需要为语言做分支。
func desktopDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	candidates := []string{filepath.Join(home, "Desktop")}
	if runtime.GOOS == "windows" {
		// OneDrive 在前：它接管之后，旧的 ~/Desktop 往往还留着一个空壳，
		// 把文件放进去用户在桌面上根本看不见。
		candidates = append([]string{
			filepath.Join(home, "OneDrive", "Desktop"),
			filepath.Join(home, "OneDrive - Personal", "Desktop"),
		}, candidates...)
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	return filepath.Join(home, "Desktop")
}

// ensureSyncRoot建好同步目录并返回它。
//
// **每次启动都建**：用户把桌面上那个文件夹删掉是完全正常的操作，
// 下次运行必须自己长回来，而不是报一句"目录不存在"。
func ensureSyncRoot() (string, error) {
	root := syncRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

// resolvePath尽量解开符号链接。
//
// **必须解**：macOS 的 /tmp 与 /var 是符号链接、Windows 有 junction、
// OneDrive 会把桌面重定向走。os.Getwd() 返回的是解析后的真实路径，而登记表里
// 存的可能是没解析的写法——不归一化就匹配不上，表现是"明明在自己的工程里，
// 每次还是弹选择器"。
//
// **路径不存在时不能直接放弃**：登记过但已被删除的项目、以及同步目录下
// 还没建出来的子目录，都要能正确参与比较。做法是向上找到第一个存在的祖先，
// 解析它，再把剩下的部分拼回去——这样 <root>/sub 与 <解析后的root>/sub
// 仍然对得上。
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	rest := ""
	cur := abs
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur { // 到根了还解不开：原样返回
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// samePathTree在解开符号链接之后判断 p 是否在 root 之内。
func samePathTree(root, p string) bool {
	return underRoot(resolvePath(root), resolvePath(p))
}

// underRoot报告 p 是否在 root 之内（或就是 root）。
// 两边都先规范化，避免 C:\Users\x 与 C:/Users/x 这类写法差异导致误判。
func underRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// ".." 开头说明跳出了 root。用分隔符判断，避免把 "..foo" 误当成越界。
	return rel != ".." && !hasDotDotPrefix(rel)
}

func hasDotDotPrefix(rel string) bool {
	if len(rel) < 3 {
		return rel == ".."
	}
	return rel[0] == '.' && rel[1] == '.' && (rel[2] == filepath.Separator || rel[2] == '/')
}
