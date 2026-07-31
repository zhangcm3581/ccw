package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// 项目选择器（2026-07-31）。
//
// 不引入 TUI 框架：这个界面只有一个列表和几个按键，为它拖一个依赖进来
// 与仓库「版本固定、依赖尽量少」的取向不符，也多一份要跟着升级的东西。
// 直接用 ANSI 序列画，raw mode 读键。
//
// **终端不是 TTY 时不进选择器**（管道、CI、重定向）：那时没人能按键，
// 转而回退到"用当前目录"，与旧行为一致。

const (
	esc      = "\x1b"
	clrLine  = esc + "[2K"
	hideCur  = esc + "[?25l"
	showCur  = esc + "[?25h"
	fgDim    = esc + "[2m"
	fgAccent = esc + "[38;5;209m" // 与官网同色系的珊瑚色
	invert   = esc + "[7m"
	reset    = esc + "[0m"
)

// pickAction是选择器的结果。
type pickAction struct {
	Project project
	// Quit为true表示用户按了 Esc/q。
	Quit bool
}

// pickerIO把选择器要用的终端能力抽出来，便于测试。
type pickerIO struct {
	in      io.Reader
	out     io.Writer
	isTTY   bool
	makeRaw func() (func(), error)
}

func stdPickerIO() pickerIO {
	return pickerIO{
		in:    os.Stdin,
		out:   os.Stdout,
		isTTY: term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())),
		makeRaw: func() (func(), error) {
			st, err := term.MakeRaw(int(os.Stdin.Fd()))
			if err != nil {
				return func() {}, err
			}
			return func() { term.Restore(int(os.Stdin.Fd()), st) }, nil
		},
	}
}

// runPicker显示项目列表并返回用户的选择。
//
// root是同步目录，cfgDir是 ~/.ccw。cwdHint非空时，列表顶部多一行
// 「登记当前目录」——在一个还没登记的工程里直接跑 cclaude 时，
// 这一行是选中态，回车即用，不必先去按 t 再把路径打一遍。
func runPicker(io_ pickerIO, root, cfgDir, cwdHint string) (pickAction, error) {
	if !io_.isTTY {
		return pickAction{}, errNotTTY
	}
	restore, err := io_.makeRaw()
	if err != nil {
		return pickAction{}, err
	}
	defer restore()
	fmt.Fprint(io_.out, hideCur)
	defer fmt.Fprint(io_.out, showCur)

	sel := 0
	rd := bufio.NewReader(io_.in)
	for {
		items := listProjects(root, cfgDir)
		rows := buildRows(items, cwdHint)
		if sel >= len(rows) {
			sel = len(rows) - 1
		}
		if sel < 0 {
			sel = 0
		}
		render(io_.out, rows, sel, root)

		key, err := readKey(rd)
		if err != nil {
			return pickAction{Quit: true}, nil
		}
		switch key {
		case keyUp:
			sel--
		case keyDown:
			sel++
		case keyQuit:
			clear(io_.out, len(rows))
			return pickAction{Quit: true}, nil
		case keyNew:
			clear(io_.out, len(rows))
			p, err := promptNewProject(io_, rd, root)
			if err == nil && p.Path != "" {
				return pickAction{Project: p}, nil
			}
		case keyOther:
			clear(io_.out, len(rows))
			p, err := promptRegisterPath(io_, rd, cfgDir, cwdHint)
			if err == nil && p.Path != "" {
				return pickAction{Project: p}, nil
			}
		case keyForget:
			if len(rows) > 0 && sel < len(rows) && !rows[sel].isAction {
				_ = forgetProject(cfgDir, rows[sel].proj.Path)
			}
		case keyEnter:
			if len(rows) == 0 {
				continue
			}
			r := rows[sel]
			clear(io_.out, len(rows))
			if r.isAction { // 「登记当前目录」
				if err := registerProject(cfgDir, cwdHint); err != nil {
					return pickAction{}, err
				}
				return pickAction{Project: project{Name: filepath.Base(cwdHint), Path: cwdHint}}, nil
			}
			return pickAction{Project: r.proj}, nil
		}
		if sel < 0 {
			sel = 0
		}
		moveUp(io_.out, len(rows))
	}
}

type row struct {
	proj     project
	isAction bool
	label    string
	hint     string
}

func buildRows(items []project, cwdHint string) []row {
	var rows []row
	if cwdHint != "" {
		rows = append(rows, row{isAction: true, label: "登记当前目录", hint: cwdHint})
	}
	for _, p := range items {
		hint := ""
		switch {
		case p.Missing:
			hint = p.Path + "  （本地已删除，选中即从云端拉回）"
		case p.External:
			hint = p.Path
		}
		rows = append(rows, row{proj: p, label: p.Name, hint: hint})
	}
	return rows
}

func render(w io.Writer, rows []row, sel int, root string) {
	fmt.Fprintf(w, "%s%s  选择项目%s\r\n", clrLine, fgAccent, reset)
	fmt.Fprintf(w, "%s%s  ↑/↓ 选择 · Enter 打开 · n 新建 · t 其他位置 · d 移出列表 · Esc/q 退出%s\r\n",
		clrLine, fgDim, reset)
	fmt.Fprintf(w, "%s%s  项目在云端运行并实时同步 · 把文件夹拖进「%s」即自动加入%s\r\n",
		clrLine, fgDim, filepath.Base(root), reset)
	fmt.Fprintf(w, "%s\r\n", clrLine)
	if len(rows) == 0 {
		fmt.Fprintf(w, "%s%s  还没有项目。按 n 新建，或把文件夹拖进桌面的「%s」。%s\r\n",
			clrLine, fgDim, filepath.Base(root), reset)
		return
	}
	for i, r := range rows {
		mark := "■"
		if r.isAction {
			mark = "+"
		}
		line := fmt.Sprintf("  %s %s", mark, r.label)
		if r.hint != "" {
			line += fmt.Sprintf("   %s%s%s", fgDim, r.hint, reset)
		}
		if i == sel {
			fmt.Fprintf(w, "%s%s%s%s\r\n", clrLine, invert, padTo(line, 78), reset)
			continue
		}
		fmt.Fprintf(w, "%s%s\r\n", clrLine, line)
	}
}

// padTo把选中行补到固定宽度，让反显是一整条而不是参差不齐的一段。
// 按显示宽度估算：CJK 占两列。
func padTo(s string, width int) string {
	w := 0
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r > 0x2e80 {
			w += 2
		} else {
			w++
		}
	}
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func headerLines() int { return 4 }

func moveUp(w io.Writer, n int) { fmt.Fprintf(w, esc+"[%dA", n+headerLines()) }

func clear(w io.Writer, n int) {
	for i := 0; i < n+headerLines(); i++ {
		fmt.Fprint(w, clrLine+"\r\n")
	}
	fmt.Fprintf(w, esc+"[%dA", n+headerLines())
}
