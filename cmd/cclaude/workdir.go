package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errUserQuit = errors.New("cclaude: 用户取消")

// resolveWorkDir决定这次要同步哪个目录。
//
// 顺序刻意如此：
//
//  1. --dir 显式指定 → 就用它。脚本与 CI 不该被一个交互界面挡住。
//  2. 当前目录已经属于某个项目（同步目录里的、或就地登记过的）→ 直接用，
//     不弹选择器。在自己的工程里敲 cclaude 就该立刻开始工作。
//  3. 其余 → 弹选择器。当前目录看着像个工程时，顶部预置一行
//     「登记当前目录」并选中，回车即用。
//
// 非交互终端（管道/CI/重定向）走不到选择器，回退到当前目录——
// 与改造前的行为一致，不会让既有脚本忽然卡住等按键。
func resolveWorkDir(cfgDir, dirFlag string, openCloud func() error) (string, error) {
	if dirFlag != "" {
		abs, err := filepath.Abs(dirFlag)
		if err != nil {
			return "", err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("cclaude: --dir 指向的不是一个目录：%s", abs)
		}
		return abs, nil
	}

	root, err := ensureSyncRoot()
	if err != nil {
		return "", fmt.Errorf("cclaude: 建不了同步目录 %s：%w", syncRoot(), err)
	}
	cwd, _ := os.Getwd()

	if p, ok := findProjectFor(listProjects(root, cfgDir), cwd); ok {
		return p.Path, nil
	}

	io_ := stdPickerIO()
	for {
		act, err := runPicker(io_, root, cfgDir, currentDirHint(root, cwd))
		if err != nil {
			if errors.Is(err, errNotTTY) {
				return cwd, nil // 非交互：维持旧行为
			}
			return "", err
		}
		if act.Cloud {
			// 管理云端是一段插曲：回来继续选项目，而不是退出。
			if openCloud != nil {
				if err := openCloud(); err != nil {
					fmt.Fprintf(os.Stderr, "管理云端：%v\n", err)
				}
			}
			continue
		}
		if act.Quit || act.Project.Path == "" {
			return "", errUserQuit
		}
		return openProject(act.Project)
	}
}

// openProject建好本地目录并返回它。登记过但被删的项目在这里长回来，
// 随后的同步会把云端文件拉下来。
func openProject(p project) (string, error) {
	if err := os.MkdirAll(p.Path, 0o755); err != nil {
		return "", err
	}
	return p.Path, nil
}

// currentDirHint决定要不要在选择器里预置「登记当前目录」。
//
// **不是什么目录都配**：在 C:\、家目录、或同步目录自身里敲 cclaude 时，
// 把整棵树登记成一个项目是灾难性的——那会把几十 GB 无关文件推上云端。
// 只有看起来是"某个具体工程"的目录才提示。
func currentDirHint(root, cwd string) string {
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	if samePathTree(root, abs) {
		return "" // 同步目录本身：里面的子文件夹才是项目
	}
	if home, err := os.UserHomeDir(); err == nil && resolvePath(home) == resolvePath(abs) {
		return ""
	}
	// 盘符根 / 文件系统根：Dir(x) == x 是根目录的判据。
	if filepath.Dir(abs) == abs {
		return ""
	}
	return abs
}
