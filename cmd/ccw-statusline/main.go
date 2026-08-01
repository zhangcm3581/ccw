// Command ccw-statusline是Claude Code的状态行脚本（2026-08-01）。
//
// Claude 从 stdin 传一份会话 JSON，把这个程序的 stdout 渲染在它 footer 的上一行。
// 配置在 /etc/claude-code/managed-settings.json 的 statusLine。
//
// **为什么是程序而不是 shell 脚本**：1/8 格精度的渐进进度条、倒计时格式、
// 数据缺失时的降级，用 shell 写既难读也难验；写成 Go 才能对这几段逻辑做单测。
// 顺带省掉往镜像里装 jq。
//
// **数据全部来自 Claude 自己**（rate_limits 是上游账号的真实额度与真实重置时间），
// 不是本仓库那套内部额度估算——后者仍在服务端执行闸门，只是不在这行显示。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

// 配色（256 色）。分隔符与空槽都用 238 灰。
const (
	colGray    = 238
	colCyan    = 81  // context
	colMagenta = 213 // 5 小时窗口
	colAqua    = 120 // 7 天窗口
)

// barCells是进度条宽度（格）。
const barCells = 8

// partials是 1/8 精度的过渡字符：▏▎▍▌▋▊▉（U+258F..U+2589）。
// 第 n 个表示填了 n/8 格。满格不用它们——满格由背景色填实。
var partials = []rune{'▏', '▎', '▍', '▌', '▋', '▊', '▉'}

type payload struct {
	Model struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	ContextWindow *struct {
		UsedPercentage    *float64 `json:"used_percentage"`
		TotalInputTokens  *int64   `json:"total_input_tokens"`
		ContextWindowSize *int64   `json:"context_window_size"`
	} `json:"context_window"`
	RateLimits *struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
	} `json:"rate_limits"`
}

type window struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *int64   `json:"resets_at"`
}

func main() {
	b, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	var p payload
	// **解析失败也要出一行**：状态行是常驻 UI，因为一次坏输入就整行消失
	// 比显示"--"更让人摸不着头脑。
	_ = json.Unmarshal(b, &p)
	fmt.Print(render(p, time.Now()))
}

// render拼出整行。
func render(p payload, now time.Time) string {
	segs := []string{
		bold(modelName(p.Model.DisplayName)),
		contextSeg(p),
		limitSeg("5h", winOf(p, false), colMagenta, now, false),
		limitSeg("7d", winOf(p, true), colAqua, now, true),
	}
	sep := fmt.Sprintf(" \x1b[38;5;%dm│\x1b[0m ", colGray)
	return strings.Join(segs, sep)
}

func winOf(p payload, seven bool) *window {
	if p.RateLimits == nil {
		return nil
	}
	if seven {
		return p.RateLimits.SevenDay
	}
	return p.RateLimits.FiveHour
}

// modelName取模型简称。空值时不留一个空段——那会让分隔符挤在一起。
func modelName(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return s
	}
	return "Claude"
}

// contextSeg：加粗标签 + 已用比例条 + 已用百分比。
//
// **context 显示已用**（与额度相反）：上下文是"填满就该清理"，
// 看的人关心的是它涨到哪儿了。
func contextSeg(p payload) string {
	label := bold("context")
	pct, ok := contextUsed(p)
	if !ok {
		return label + " " + emptyBar() + " --"
	}
	return fmt.Sprintf("%s %s %d%%", label, bar(pct/100, colCyan), int(math.Round(pct)))
}

// contextUsed取已用百分比。used_percentage 缺失时用 token 数兜底算——
// 两个字段都在同一个对象里，只有一个能用时没有理由降级成"--"。
func contextUsed(p payload) (float64, bool) {
	c := p.ContextWindow
	if c == nil {
		return 0, false
	}
	if c.UsedPercentage != nil {
		return clampPct(*c.UsedPercentage), true
	}
	if c.TotalInputTokens != nil && c.ContextWindowSize != nil && *c.ContextWindowSize > 0 {
		return clampPct(float64(*c.TotalInputTokens) / float64(*c.ContextWindowSize) * 100), true
	}
	return 0, false
}

// limitSeg：剩余比例条 + 剩余百分比 + 到重置的倒计时。
//
// **额度显示剩余**（与 context 相反）：使用者关心的是还能干多久。
func limitSeg(label string, w *window, accent int, now time.Time, allowDays bool) string {
	if w == nil || w.UsedPercentage == nil {
		return label + " " + emptyBar() + " --"
	}
	left := clampPct(100 - *w.UsedPercentage)
	out := fmt.Sprintf("%s %s %d%%", label, bar(left/100, accent), int(math.Round(left)))
	if w.ResetsAt != nil {
		if cd := countdown(time.Unix(*w.ResetsAt, 0).Sub(now), allowDays); cd != "" {
			out += " " + cd
		}
	}
	return out
}

// bar渲染 8 格、1/8 精度的渐进进度条。
//
// 已填满的格用 accent **背景**整段填实（一个空格 + 背景色）；空格用灰底做轨道；
// 中间那一格用 accent 前景 + 灰底画对应的 ▏..▉——这样填充边缘是连续的，
// 而不是整格整格地跳。
func bar(frac float64, accent int) string {
	if frac < 0 || math.IsNaN(frac) {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	// 总共 barCells*8 个 1/8 子格。
	eighths := int(math.Round(frac * float64(barCells) * 8))
	full := eighths / 8
	rem := eighths % 8
	if full > barCells {
		full, rem = barCells, 0
	}

	var b strings.Builder
	if full > 0 {
		fmt.Fprintf(&b, "\x1b[48;5;%dm%s", accent, strings.Repeat(" ", full))
	}
	used := full
	if rem > 0 && used < barCells {
		fmt.Fprintf(&b, "\x1b[48;5;%dm\x1b[38;5;%dm%c", colGray, accent, partials[rem-1])
		used++
	}
	if used < barCells {
		fmt.Fprintf(&b, "\x1b[48;5;%dm%s", colGray, strings.Repeat(" ", barCells-used))
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// emptyBar是数据缺失时的降级轨道：整条灰底，不画任何填充。
func emptyBar() string {
	return fmt.Sprintf("\x1b[48;5;%dm%s\x1b[0m", colGray, strings.Repeat(" ", barCells))
}

// maxCountdown是倒计时的合理上限。
//
// 5 小时窗口最多 5 小时后重置、7 天窗口最多 7 天，超出这个量级的只可能是
// 坏数据或时钟偏移。**必须挡住**：实测用一个远未来的 resets_at 会渲染出
// "2281787h 22m"，把整行撑爆、把前面几段挤出屏幕。宁可不显示倒计时。
const maxCountdown = 8 * 24 * time.Hour

// countdown把到重置的时长格式化。
//
// allowDays 且超过 24 小时用「Nd Mh Km」，其余一律「Xh Ym」。
// 已经过期（负数）返回空——与其显示一个负的倒计时，不如不显示：
// 那说明这份数据本身已经过时了。
func countdown(d time.Duration, allowDays bool) string {
	if d <= 0 || d > maxCountdown {
		return ""
	}
	total := int(d / time.Minute)
	if allowDays && d >= 24*time.Hour {
		days := total / (24 * 60)
		hours := (total % (24 * 60)) / 60
		mins := total % 60
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	return fmt.Sprintf("%dh %dm", total/60, total%60)
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func bold(s string) string { return "\x1b[1m" + s + "\x1b[0m" }
