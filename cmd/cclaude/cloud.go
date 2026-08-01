package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"ccw/internal/control"
	syncpkg "ccw/internal/sync"
)

// 「管理云端」（2026-08-01）。
//
// 每个本地目录在云端各有一份副本。换过机器、试过几个目录、项目改过名之后，
// 云端会攒下一堆再也用不到的副本，把项目那 15 GiB 配额慢慢吃光——而在此之前
// 用户完全看不见它们的存在。这一屏把它们列出来、标明大小、允许删掉。
//
// **删掉的只是云端副本**：本地文件一个不动，下次打开那个项目会重新上传。
// 界面上必须写清楚这一点，否则"删除"两个字看着像要删代码。

type cloudRow struct {
	info   syncpkg.WorkspaceInfo
	marked bool
	// current为true表示这就是当前正在用的副本。仍然可以删（用户可能就是想清掉重来），
	// 但要标出来，避免手一抖把正在用的删了还不知道。
	current bool
}

// runCloudManager显示云端副本列表并执行删除。
func runCloudManager(io_ pickerIO, mgr syncpkg.CloudManager, curWS string, used, limit int64) error {
	infos, err := mgr.Workspaces()
	if err != nil {
		return err
	}
	rows := make([]cloudRow, 0, len(infos))
	for _, in := range infos {
		rows = append(rows, cloudRow{info: in, current: in.WS == curWS})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].info.Bytes > rows[j].info.Bytes })

	restore, err := io_.makeRaw()
	if err != nil {
		return err
	}
	defer restore()
	fmt.Fprint(io_.out, hideCur)
	defer fmt.Fprint(io_.out, showCur)

	rd := bufio.NewReader(io_.in)
	sc := newScreen(io_.out)
	sel := 0
	for {
		if sel < 0 {
			sel = 0
		}
		if sel >= len(rows) && len(rows) > 0 {
			sel = len(rows) - 1
		}
		sc.begin()
		renderCloud(sc, rows, sel, used, limit)

		b, err := rd.ReadByte()
		if err != nil {
			sc.done()
			return nil
		}
		switch b {
		case 'q', 'Q', 3:
			sc.done()
			return nil
		case 0x1b:
			if rd.Buffered() == 0 {
				sc.done()
				return nil
			}
			if c, _ := rd.ReadByte(); c == '[' {
				switch d, _ := rd.ReadByte(); d {
				case 'A':
					sel--
				case 'B':
					sel++
				}
			}
		case 'k':
			sel--
		case 'j':
			sel++
		case ' ':
			if sel < len(rows) {
				rows[sel].marked = !rows[sel].marked
			}
		case '\r', '\n':
			marked := markedRows(rows)
			if len(marked) == 0 {
				break
			}
			sc.done()
			var freed int64
			var failed []string
			for _, r := range marked {
				n, err := mgr.Purge(r.info.WS)
				if err != nil {
					failed = append(failed, r.info.WS)
					continue
				}
				freed += n
			}
			sc.line("  已删除 %d 个云端副本，释放 %s", len(marked)-len(failed), humanBytes(freed))
			if len(failed) > 0 {
				sc.line("  %s删除失败：%s%s", fgDim, strings.Join(failed, "、"), reset)
			}
			sc.line("  %s本地文件没有变化；下次打开这些项目会重新上传。%s", fgDim, reset)
			return nil
		}
	}
}

func markedRows(rows []cloudRow) []cloudRow {
	var out []cloudRow
	for _, r := range rows {
		if r.marked {
			out = append(out, r)
		}
	}
	return out
}

func renderCloud(sc *screen, rows []cloudRow, sel int, used, limit int64) {
	sc.line("%s%s  管理云端%s %s·%s 项目副本", clrLine, fgAccent, reset, fgDim, reset)
	pct := 0
	if limit > 0 {
		pct = int(float64(used) / float64(limit) * 100)
	}
	sc.line("%s%s    云端存储        %d%% · %s/%s%s",
		clrLine, fgDim, pct, humanBytes(used), humanBytes(limit), reset)
	sc.line("%s", clrLine)

	if len(rows) == 0 {
		sc.line("%s%s  云端还没有任何副本。%s", clrLine, fgDim, reset)
	}
	for i, r := range rows {
		mark := "○"
		if r.marked {
			mark = "●"
		}
		cursor := "  "
		if i == sel {
			cursor = "▸ "
		}
		line := fmt.Sprintf("%s%s %s  %s", cursor, mark, r.info.WS, humanBytes(r.info.Bytes))
		if r.current {
			line += "  （当前）"
		}
		if r.marked {
			sc.line("%s%s%s%s", clrLine, fgAccent, line, reset)
			continue
		}
		sc.line("%s%s", clrLine, line)
	}
	var sum int64
	for _, r := range rows {
		if r.marked {
			sum += r.info.Bytes
		}
	}
	sc.line("%s", clrLine)
	sc.line("%s  已选将释放 %s", clrLine, humanBytes(sum))
	sc.line("%s%s  ↑↓ 选择 · 空格 标记删除 · Enter 删除所选（下次需重新同步） · q/esc 返回选择器%s",
		clrLine, fgDim, reset)
}

// humanBytes按截图的口径显示：B / KB / MB / GB，1024 进制。
func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	v, exp := float64(n)/u, 0
	for v >= u && exp < 3 {
		v /= u
		exp++
	}
	unit := []string{"KB", "MB", "GB", "TB"}[exp]
	if v >= 100 {
		return fmt.Sprintf("%.0f%s", v, unit)
	}
	return fmt.Sprintf("%.1f%s", v, unit)
}

// openCloudManager开一条只用来管理云端副本的连接并显示那一屏。
//
// **单独连一次**：这一屏与常规同步循环无关，复用不了那条连接（它正忙着
// 每2秒一轮的同步），而连接令牌本来就是2分钟短期、允许重连的。
func openCloudManager(ctx context.Context, c control.Client, sessionToken string) error {
	conn, err := c.Connection(ctx, sessionToken)
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	mgr, err := syncpkg.DialCloud(ctx, conn.SyncURL, conn.SyncToken, deviceName(), syncpkg.WorkspaceKey(cwd))
	if err != nil {
		return err
	}
	defer mgr.Close()
	return runCloudManager(stdPickerIO(), mgr, syncpkg.WorkspaceKey(cwd), conn.DiskUsed, conn.DiskLimit)
}
