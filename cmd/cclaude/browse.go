package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// 目录浏览器（2026-07-31）。
//
// `t 其他位置` 原来只能手打完整路径——在 Windows 上打
// `C:\TestProjects\SyntheticProject` 既慢又容易打错，而打错只会得到一句
// "不是一个存在的目录"，还得从头再来。
//
// 现在可以一级一级走。仍保留直接输入：路径已经在剪贴板里时，粘贴比走目录快。

// browseState是浏览器的当前位置与列表，抽出来是为了让导航逻辑可测
// （真正难写对的是"上一级""盘符根""权限不足"这些边界，不是画界面）。
type browseState struct {
	dir     string
	entries []browseEntry
	sel     int
}

type browseEntry struct {
	name  string
	path  string
	isUp  bool
	isErr bool
}

// newBrowseState列出 dir 下的子目录。
//
// **读不动也要能显示**：权限不足的目录（Windows 上的 System Volume Information、
// macOS 的受保护目录）不该让整个浏览器卡住或退出，列一行说明、允许返回上一级。
func newBrowseState(dir string) browseState {
	dir = filepath.Clean(dir)
	st := browseState{dir: dir}
	if parent := filepath.Dir(dir); parent != dir {
		st.entries = append(st.entries, browseEntry{name: "..", path: parent, isUp: true})
	} else if runtime.GOOS == "windows" {
		// 已经在盘符根：往上一级＝回到盘符列表。
		st.entries = append(st.entries, browseEntry{name: "..（选择磁盘）", path: driveListSentinel, isUp: true})
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		st.entries = append(st.entries, browseEntry{name: "（读不了这个目录：" + errBrief(err) + "）", isErr: true})
		return st
	}
	var dirs []browseEntry
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, browseEntry{name: e.Name(), path: filepath.Join(dir, e.Name())})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].name) < strings.ToLower(dirs[j].name)
	})
	st.entries = append(st.entries, dirs...)
	return st
}

// driveListSentinel是"显示盘符列表"这个伪目录。
const driveListSentinel = "\x00drives"

// newDriveList列出 Windows 上存在的盘符。
func newDriveList() browseState {
	st := browseState{dir: driveListSentinel}
	for c := 'A'; c <= 'Z'; c++ {
		p := string(c) + `:\`
		if _, err := os.Stat(p); err == nil {
			st.entries = append(st.entries, browseEntry{name: p, path: p})
		}
	}
	if len(st.entries) == 0 {
		st.entries = append(st.entries, browseEntry{name: "（找不到任何磁盘）", isErr: true})
	}
	return st
}

func errBrief(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

// enter进入选中的条目，返回新状态。选中的是错误行时原地不动。
func (s browseState) enter() browseState {
	if s.sel < 0 || s.sel >= len(s.entries) {
		return s
	}
	e := s.entries[s.sel]
	if e.isErr {
		return s
	}
	if e.path == driveListSentinel {
		return newDriveList()
	}
	return newBrowseState(e.path)
}

// up回到上一级。
func (s browseState) up() browseState {
	if s.dir == driveListSentinel {
		return s
	}
	parent := filepath.Dir(s.dir)
	if parent == s.dir {
		if runtime.GOOS == "windows" {
			return newDriveList()
		}
		return s
	}
	return newBrowseState(parent)
}

func (s browseState) clampSel() browseState {
	if s.sel < 0 {
		s.sel = 0
	}
	if s.sel >= len(s.entries) {
		s.sel = len(s.entries) - 1
	}
	if s.sel < 0 {
		s.sel = 0
	}
	return s
}

// current返回"选定当前目录"时应当采用的路径。
// 在盘符列表里没有"当前目录"可言，返回空表示不能选。
func (s browseState) current() string {
	if s.dir == driveListSentinel {
		return ""
	}
	return s.dir
}

// browseForDir让用户走目录树选一个目录。返回空字符串表示取消。
func browseForDir(io_ pickerIO, rd *bufio.Reader, start string) string {
	if start == "" {
		if h, err := os.UserHomeDir(); err == nil {
			start = h
		} else {
			start = "."
		}
	}
	st := newBrowseState(start)
	for {
		st = st.clampSel()
		renderBrowse(io_, st)
		b, err := rd.ReadByte()
		if err != nil {
			clearBrowse(io_, st)
			return ""
		}
		prev := st
		switch b {
		case '\r', '\n':
			st = st.enter()
			st.sel = 0
		case 's', 'S', ' ':
			if cur := st.current(); cur != "" {
				clearBrowse(io_, prev)
				return cur
			}
		case 'p', 'P':
			clearBrowse(io_, prev)
			line, _ := readLineRaw(io_, rd, "直接输入路径（留空取消）: ")
			p := strings.Trim(strings.TrimSpace(line), "\"'")
			if p == "" {
				st = newBrowseState(st.dir)
				break
			}
			if abs, err := filepath.Abs(p); err == nil {
				if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
					return abs
				}
			}
			fmt.Fprintf(io_.out, "  %s不是一个存在的目录：%s%s\r\n", fgDim, p, reset)
			st = newBrowseState(st.dir)
		case 127, 8: // Backspace＝上一级
			st = st.up()
			st.sel = 0
		case 3, 0x1b: // Ctrl-C / Esc（含方向键序列的开头）
			if b == 0x1b && rd.Buffered() > 0 {
				if c, _ := rd.ReadByte(); c == '[' {
					d, _ := rd.ReadByte()
					switch d {
					case 'A':
						st.sel--
					case 'B':
						st.sel++
					case 'C': // →＝进入
						st = st.enter()
						st.sel = 0
					case 'D': // ←＝上一级
						st = st.up()
						st.sel = 0
					}
					clearBrowse(io_, prev)
					continue
				}
			}
			clearBrowse(io_, prev)
			return ""
		case 'k':
			st.sel--
		case 'j':
			st.sel++
		}
		clearBrowse(io_, prev)
	}
}

func renderBrowse(io_ pickerIO, st browseState) {
	loc := st.dir
	if loc == driveListSentinel {
		loc = "选择磁盘"
	}
	fmt.Fprintf(io_.out, "%s%s  选择目录%s  %s%s%s\r\n", clrLine, fgAccent, reset, fgDim, loc, reset)
	fmt.Fprintf(io_.out, "%s%s  ↑/↓ 移动 · Enter/→ 进入 · ←/Backspace 上一级 · s 选定当前目录 · p 输入路径 · Esc 取消%s\r\n",
		clrLine, fgDim, reset)
	fmt.Fprintf(io_.out, "%s\r\n", clrLine)
	for i, e := range st.entries {
		line := "  " + e.name
		if e.isUp {
			line = "  " + e.name
		}
		if i == st.sel {
			fmt.Fprintf(io_.out, "%s%s%s%s\r\n", clrLine, invert, padTo(line, 78), reset)
			continue
		}
		fmt.Fprintf(io_.out, "%s%s\r\n", clrLine, line)
	}
}

func browseHeaderLines() int { return 3 }

func clearBrowse(io_ pickerIO, st browseState) {
	n := len(st.entries) + browseHeaderLines()
	fmt.Fprintf(io_.out, esc+"[%dA", n)
}
