package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var errNotTTY = errors.New("cclaude: 当前不是交互式终端，跳过项目选择器")

type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyEnter
	keyQuit
	keyNew
	keyOther
	keyForget
)

// readKey读一个按键。方向键是 ESC [ A/B 三字节序列。
//
// **裸 ESC 也当退出**：raw mode 下按 Esc 只来一个 0x1b，后面没有字节。
// 这里用"缓冲区里还有没有待读字节"来区分它与方向键序列的开头——
// bufio.Reader.Buffered()>0 说明是成串到达的转义序列。
func readKey(r *bufio.Reader) (key, error) {
	b, err := r.ReadByte()
	if err != nil {
		return keyNone, err
	}
	switch b {
	case '\r', '\n':
		return keyEnter, nil
	case 'q', 'Q', 3: // 3 = Ctrl-C
		return keyQuit, nil
	case 'n', 'N':
		return keyNew, nil
	case 't', 'T':
		return keyOther, nil
	case 'd', 'D':
		return keyForget, nil
	case 'k':
		return keyUp, nil
	case 'j':
		return keyDown, nil
	case 0x1b:
		if r.Buffered() == 0 {
			return keyQuit, nil // 裸 Esc
		}
		if c, err := r.ReadByte(); err == nil && c == '[' {
			if d, err := r.ReadByte(); err == nil {
				switch d {
				case 'A':
					return keyUp, nil
				case 'B':
					return keyDown, nil
				}
			}
		}
	}
	return keyNone, nil
}

// promptNewProject在同步目录里新建一个文件夹并返回它。
func promptNewProject(io_ pickerIO, rd *bufio.Reader, root string) (project, error) {
	name, err := readLineRaw(io_, rd, "新项目名称（留空取消）: ")
	if err != nil || strings.TrimSpace(name) == "" {
		return project{}, err
	}
	name = sanitizeFolderName(name)
	if name == "" {
		return project{}, nil
	}
	p := filepath.Join(root, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		return project{}, err
	}
	return project{Name: name, Path: p}, nil
}

// promptRegisterPath让用户挑一个已有目录并就地登记。
//
// **走目录树而不是手打路径**：在 Windows 上打
// `C:\TestProjects\SyntheticProject` 既慢又容易打错，而打错只得到一句
// "不是一个存在的目录"、还得从头再来。浏览器里仍留着 p 直接输入，
// 路径已经在剪贴板里时粘贴更快。
func promptRegisterPath(io_ pickerIO, rd *bufio.Reader, cfgDir, def string) (project, error) {
	abs := browseForDir(io_, rd, def)
	if abs == "" {
		return project{}, nil
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return project{}, nil
	}
	if err := registerProject(cfgDir, abs); err != nil {
		return project{}, err
	}
	return project{Name: filepath.Base(abs), Path: abs}, nil
}

// readLineRaw在 raw mode 下读一行（自己处理回显与退格）。
// 不能用 bufio.ReadString：raw mode 里终端不回显，用户看不见自己打的字。
func readLineRaw(io_ pickerIO, rd *bufio.Reader, prompt string) (string, error) {
	fmt.Fprintf(io_.out, "\r\n%s  %s%s", clrLine, prompt, reset)
	var sb []rune
	for {
		b, err := rd.ReadByte()
		if err != nil {
			return "", err
		}
		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(io_.out, "\r\n")
			return string(sb), nil
		case b == 3 || b == 0x1b: // Ctrl-C / Esc
			fmt.Fprint(io_.out, "\r\n")
			return "", nil
		case b == 127 || b == 8: // Backspace
			if len(sb) > 0 {
				sb = sb[:len(sb)-1]
				fmt.Fprintf(io_.out, "\r%s  %s%s", clrLine, prompt, string(sb))
			}
		case b >= 0x20:
			sb = append(sb, rune(b))
			fmt.Fprintf(io_.out, "%c", b)
		}
	}
}

// sanitizeFolderName去掉在 Windows 上非法的文件名字符。
// 不做音译或截断——只挡住会让创建失败的那几个。
func sanitizeFolderName(s string) string {
	s = strings.TrimSpace(s)
	bad := `<>:"/\|?*`
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || strings.ContainsRune(bad, r) {
			continue
		}
		out = append(out, r)
	}
	return strings.TrimSpace(strings.Trim(string(out), "."))
}

var _ io.Reader = (*strings.Reader)(nil)
