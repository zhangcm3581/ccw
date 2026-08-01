package main

import (
	"fmt"
	"io"
)

// 一块可重绘的屏幕区域（2026-08-01，code review 后重写）。
//
// 原来三个界面各写各的清屏：先打 N 行、再 `ESC[NA` 移回去。这套记账在
// **行数会变**的时候一定出错，而它恰好在三处都出错了：
//
//  1. 选择器按 d 移出一个项目后，新一帧少一行，旧的最后一行没人擦，
//     永远留在屏幕上；
//  2. 按 n / t 之后弹输入提示，提示自己又多打了几行，而循环末尾仍按
//     旧行数往回移——光标停在比起点更高的位置，下一帧直接盖掉 shell 提示符
//     和更早的输出；
//  3. 目录浏览的 p 分支清了两次，同款错位。
//
// 这正是用户刚抱怨过的那类"旧内容留在屏幕上"，只不过这次是自己造的。
//
// 现在只记一件事：**上一帧实际写了多少行**。重绘时移回那一帧的开头，
// 然后 `ESC[J` 把从光标到屏幕末尾全部擦掉——行数变少、中途插了提示，
// 都一并解决，不需要谁去算差值。
type screen struct {
	w     io.Writer
	drawn int
}

func newScreen(w io.Writer) *screen { return &screen{w: w} }

// begin回到上一帧的开头并擦掉它。第一帧时什么都不做。
func (s *screen) begin() {
	if s.drawn > 0 {
		fmt.Fprintf(s.w, esc+"[%dA", s.drawn)
	}
	fmt.Fprint(s.w, esc+"[J") // 从光标擦到屏幕末尾
	s.drawn = 0
}

// line写一行并计数。**必须用它而不是直接 Fprintf**，否则行数对不上。
func (s *screen) line(format string, a ...any) {
	fmt.Fprintf(s.w, format, a...)
	fmt.Fprint(s.w, "\r\n")
	s.drawn++
}

// done擦掉最后一帧，把光标留在区域起点。
// 界面退出后由调用方接着打自己的输出，不会与残留的界面重叠。
func (s *screen) done() {
	s.begin()
	s.drawn = 0
}

// external告诉screen：外部代码往屏幕上又写了 n 行（例如 raw 模式下的输入提示）。
// 不记的话下一次 begin 会少移 n 行，于是擦不干净。
func (s *screen) external(n int) { s.drawn += n }
