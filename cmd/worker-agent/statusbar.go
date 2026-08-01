package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"ccw/internal/quota"
	"ccw/internal/terminal"
)

// tmux 状态栏里的额度显示（2026-08-01）。
//
// 用量此前只在 cclaude 启动时打一行就没了，跑到一半根本不知道还剩多少——
// 而这套系统的核心约束恰恰是额度（整机共用一个上游账号）。
//
// **挂在 tmux 的 status-right 上**，不是让客户端去画：客户端与终端之间只有
// 一条字节流，插进去的任何东西都会和 Claude 的 TUI 抢同一块屏幕。tmux 的
// 状态栏是它自己管理的一行，天然不冲突，而且断线重连后还在。
//
// 数据来自已经在跑的 30 秒额度循环——那里本来就算好了 5h/7d 用量，
// 不为状态栏多查一次库。

// quotaStatus把额度决定渲染成状态栏文本。
//
// **说"剩多少"而不是"用了多少"**：使用者关心的是还能干多久。
// 单位统一叫"内部额度单位"的口径在这里体现为不标百分号以外的任何单位——
// spec §10 明确禁止把它标成官方订阅百分比。
func quotaStatus(d quota.Decision, fiveHourLimit, sevenDayLimit int64) string {
	var b strings.Builder
	b.WriteString(pctLeft("5h", d.FiveHourUsed, fiveHourLimit))
	b.WriteString("  ")
	b.WriteString(pctLeft("7d", d.SevenDayUsed, sevenDayLimit))
	if d.Over {
		b.WriteString("  #[fg=red,bold]受限#[default]")
	}
	return b.String()
}

// pctLeft渲染一段"标签 ▓▓▓░░ 剩NN%"。
func pctLeft(label string, used, limit int64) string {
	if limit <= 0 {
		return label + " —"
	}
	left := float64(limit-used) / float64(limit)
	if left < 0 {
		left = 0
	}
	if left > 1 {
		left = 1
	}
	const cells = 10
	full := int(left*cells + 0.5)
	color := "green"
	switch {
	case left <= 0.1:
		color = "red"
	case left <= 0.25:
		color = "yellow"
	}
	return fmt.Sprintf("%s #[fg=%s]%s#[fg=colour238]%s#[default] 剩%d%%",
		label, color, strings.Repeat("▓", full), strings.Repeat("░", cells-full), int(left*100+0.5))
}

// setStatusBar把额度写进该项目 tmux server 的全局 status-right。
//
// socket 名就是 project id（terminal.Names 的约定），所以一次设置对这个项目
// 的全部工作区会话都生效。失败**只记日志不中断**：状态栏是锦上添花，
// 不能因为它挂了就影响额度执行那条主链路。
func setStatusBar(ctx context.Context, container, projectID, text string) error {
	socket, _ := terminal.Names(projectID, "x")
	cmd := exec.CommandContext(ctx, "docker", "exec", container,
		"tmux", "-L", socket, "set-option", "-g", "status-right", text)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// 会话还没建起来时 tmux server 不存在，这是常态，不值得报警。
		if strings.Contains(string(out), "no server running") {
			return nil
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
