package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 项目登记表（2026-07-31）。
//
// 项目有两种来源，选择器里都能看到：
//
//   - **同步目录里的子文件夹**：拖进去即是项目，不需要登记任何东西。
//     每次启动重新扫描，所以拖进/删掉立刻反映出来。
//   - **就地登记的外部目录**：C:\code 这类不想搬家的工程。文件一个都不动，
//     只在 ~/.ccw/projects.json 里记一行路径。
//
// 为什么不搬家：搬走一个工程目录会打断所有指向旧路径的东西——IDE 打开的工程、
// 终端历史、快捷方式、其他工具的配置。同步的落点由**工作区键**决定，
// 而键来自绝对路径，和目录摆在哪里无关；所以"统一放在同步目录下"是个
// 界面上的整理需求，不该用移动用户文件来实现。

type project struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// External为true表示这个项目不在同步目录里（就地登记）。
	// 不落盘——每次按当前同步目录重新判定，同步目录换了位置也不会记错。
	External bool `json:"-"`
	// Missing为true表示登记过但本地目录已经不在了。
	// 仍然显示在列表里：它在云端可能还有文件，选中即拉回来。
	Missing bool `json:"-"`
}

func projectsPath(dir string) string { return filepath.Join(dir, "projects.json") }

// loadRegistered读就地登记的项目。文件不存在＝还没登记过，不是错误。
func loadRegistered(dir string) []project {
	b, err := os.ReadFile(projectsPath(dir))
	if err != nil {
		return nil
	}
	var ps []project
	if json.Unmarshal(b, &ps) != nil {
		return nil
	}
	return ps
}

func saveRegistered(dir string, ps []project) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(projectsPath(dir), append(b, '\n'), 0o600)
}

// registerProject把一个外部目录记进登记表。已存在的路径不重复记。
func registerProject(dir, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	ps := loadRegistered(dir)
	for _, p := range ps {
		if sameProjectPath(p.Path, abs) {
			return nil
		}
	}
	return saveRegistered(dir, append(ps, project{Name: filepath.Base(abs), Path: abs}))
}

// forgetProject从登记表里去掉一个路径。**只删记录，不动文件**。
func forgetProject(dir, path string) error {
	ps := loadRegistered(dir)
	out := ps[:0]
	for _, p := range ps {
		if !sameProjectPath(p.Path, path) {
			out = append(out, p)
		}
	}
	return saveRegistered(dir, out)
}

// sameProjectPath比较两个项目路径。Windows 大小写不敏感，
// 而 C:\Code 与 C:\code 指的是同一个目录——不归一化会登记出两条。
func sameProjectPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if isCaseInsensitiveFS() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// listProjects汇总同步目录里的子文件夹与就地登记的项目。
//
// 同名时以同步目录里的为准：它是眼睛能直接看到的那个。
func listProjects(root, cfgDir string) []project {
	seen := map[string]bool{}
	var out []project

	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			// 只要目录；同步目录里可能有 README 之类的散落文件。
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := filepath.Join(root, e.Name())
			seen[normKey(p)] = true
			out = append(out, project{Name: e.Name(), Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })

	var ext []project
	for _, p := range loadRegistered(cfgDir) {
		if seen[normKey(p.Path)] {
			continue // 已经在同步目录里了，不重复显示
		}
		p.External = !underRoot(root, p.Path)
		if fi, err := os.Stat(p.Path); err != nil || !fi.IsDir() {
			p.Missing = true
		}
		if p.Name == "" {
			p.Name = filepath.Base(p.Path)
		}
		ext = append(ext, p)
	}
	sort.Slice(ext, func(i, j int) bool { return strings.ToLower(ext[i].Name) < strings.ToLower(ext[j].Name) })
	return append(out, ext...)
}

func normKey(p string) string {
	p = filepath.Clean(p)
	if isCaseInsensitiveFS() {
		return strings.ToLower(p)
	}
	return p
}

// findProjectFor在项目列表里找出包含 p 的那个（p 可以是项目里的子目录）。
// 命中时返回项目根，让"在工程的任意子目录里跑 cclaude"也能落到同一个工作区。
func findProjectFor(ps []project, p string) (project, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return project{}, false
	}
	best := project{}
	found := false
	for _, cand := range ps {
		if cand.Missing {
			continue
		}
		if samePathTree(cand.Path, abs) {
			// 取最深的那个：同步目录本身也可能被登记，那时子项目更贴切。
			if !found || len(cand.Path) > len(best.Path) {
				best, found = cand, true
			}
		}
	}
	return best, found
}
