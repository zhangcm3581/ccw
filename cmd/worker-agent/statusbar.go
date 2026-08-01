package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"ccw/internal/quota"
)

// tmux 状态栏里的额度显示（2026-08-01）。
//
// 用量此前只在 cclaude 启动时打一行就没了，跑到一半根本不知道还剩多少——
// 而这套系统的核心约束恰恰是额度（整机共用一个上游账号）。
//
// **走 Claude Code 的 statusLine**（2026-08-01 改）：先前挂在 tmux 的
// status-right 上，位置离 Claude 的界面太远。现在 worker-agent 把一行文本写进
// 容器的 /tmp/ccw-quota，Claude 由 /etc/claude-code/managed-settings.json 配置
// 每 10 秒 `cat` 它，渲染在自己 footer 的上一行。
//
// **仍然不让客户端去画**：客户端与终端之间只有一条字节流，插进去的东西会和
// Claude 的 TUI 抢同一块屏幕（这一轮已经为重绘冲突付过两次代价）。
//
// 数据来自已经在跑的 30 秒额度循环——那里本来就算好了 5h/7d 用量，
// 不为状态栏多查一次库。

// quotaFile是容器内的落点。**/tmp 而不是任何卷**：它天然是每容器一份
// （＝每项目一份），而 /home/claude 是全部项目共享的卷，写在那里会串项目。
const quotaFile = "/tmp/ccw-quota"

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
		b.WriteString("  \x1b[1;31m受限")
		if why := overReason(d.Reason); why != "" {
			b.WriteString("·" + why)
		}
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// overReason把受限原因翻成一个词。
//
// **必须显示原因**：账号池耗尽时项目自己的用量可能还很低，只写"受限"的话
// 状态栏会同时说"剩90%"和"受限"，看的人无从理解——那两个数字是项目级的，
// 而拦住他的是整机共用的那个上游账号（设计§7.3：一台机器只授权一次，
// 全部项目共用同一个账号的额度）。
func overReason(reason string) string {
	switch reason {
	case "pool_exhausted":
		return "账号池"
	case "five_hour_limit":
		return "5h"
	case "seven_day_limit":
		return "7d"
	}
	return ""
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
	color := "\x1b[32m" // 绿
	switch {
	case left <= 0.1:
		color = "\x1b[31m" // 红
	case left <= 0.25:
		color = "\x1b[33m" // 黄
	}
	return fmt.Sprintf("%s %s%s\x1b[90m%s\x1b[0m 剩%d%%",
		label, color, strings.Repeat("▓", full), strings.Repeat("░", cells-full), int(left*100+0.5))
}

// setStatusBar把额度写进容器里的 quotaFile，供 Claude 的 statusLine 读取。
//
// **文本走 stdin 而不是命令行**：它含 ANSI 转义与百分号，拼进 sh -c 里要
// 处理引号转义，而 stdin 完全没有这个问题（与推送源码包同款理由）。
// 失败**只记日志不中断**：状态行是锦上添花，不能因为它挂了就影响
// "超额就关终端"那条主链路。
func setStatusBar(ctx context.Context, container, projectID, text string) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container,
		"sh", "-c", "cat > "+quotaFile)
	// **不写尾部换行**：statusLine 把每一行渲染成一行，多一个换行就多出
	// 一条空行，把 Claude 的界面往上顶。
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
