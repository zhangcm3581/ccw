// Command ccw-statusline是Claude Code的状态行脚本（2026-08-01）。
//
// Claude 从 stdin 传一份会话 JSON，把这个程序的 stdout 渲染在它 footer 的上一行。
// 配置在 /etc/claude-code/managed-settings.json 的 statusLine。
//
// **为什么是程序而不是 shell 脚本**：1/8 格精度的渐进进度条、倒计时格式、
// 数据缺失时的降级，用 shell 写既难读也难验；写成 Go 才能对这几段逻辑做单测。
// 顺带省掉往镜像里装 jq。
//
// **5h/7d 来自 Claude 自己**（rate_limits 是上游账号的真实额度与真实重置时间）。
// 注意那是**整个账号**的用量：同一节点上全部项目共用一个上游账号，
// 所以标签写成「账号5h/账号7d」——不标清楚的话，看的人会以为那是自己项目的额度。
//
// 而真正会关掉终端的是**本项目**的内部额度闸门，与账号用量可以完全不一致
// （账号还剩 80%，你的项目却已经到顶）。worker-agent 每 30 秒把本项目的受限状态
// 写进容器的 /tmp/ccw-project-quota，这里读它——**只在受限时多出一段**，
// 平时不占宽度。
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
	now := time.Now()
	// 顺手把账号的真实额度落一份快照。**先渲染再写**——写失败绝不能影响状态行，
	// 它是每 10 秒都在跑的常驻 UI。
	line := render(p, now)
	fmt.Print(line)
	writeAccountSnapshot(p, now)
}

// accountSnapshotPath是账号真实额度快照的落点。
//
// **这是这套系统唯一能拿到 Claude 真实用量的地方**：官方 CLI 没有 usage 命令，
// 而 rate_limits 只随会话的 statusline JSON 送进来。写一份出来，
// worker-agent 才能用它反推账号池上限（否则档位百分比只能靠猜）。
//
// 落 /tmp：每容器一份，不进任何卷。
const accountSnapshotPath = "/tmp/ccw-account-usage"

// accountSnapshotPathVar让测试改写落点；生产恒为上面那个常量。
var accountSnapshotPathVar = accountSnapshotPath

// writeAccountSnapshot把账号级 rate_limits 写成一行。
//
// 数据不全时**不写**：留着上一份旧快照，比写一份半截的更有用——
// 读的一方靠 at= 判断新鲜度，而一份缺字段的快照会让它误以为拿到了新数据。
func writeAccountSnapshot(p payload, now time.Time) {
	five, seven := winOf(p, false), winOf(p, true)
	if five == nil || five.UsedPercentage == nil || seven == nil || seven.UsedPercentage == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "at=%d five_hour_pct=%.2f seven_day_pct=%.2f",
		now.Unix(), clampPct(*five.UsedPercentage), clampPct(*seven.UsedPercentage))
	if five.ResetsAt != nil {
		fmt.Fprintf(&b, " five_hour_resets=%d", *five.ResetsAt)
	}
	if seven.ResetsAt != nil {
		fmt.Fprintf(&b, " seven_day_resets=%d", *seven.ResetsAt)
	}
	// 写失败就算了：状态行不该因为写不了一个临时文件而报错或变形。
	_ = os.WriteFile(accountSnapshotPathVar, []byte(b.String()), 0o644)
}

// render拼出整行。
func render(p payload, now time.Time) string {
	segs := []string{
		bold(modelName(p.Model.DisplayName)),
		contextSeg(p),
		limitSeg("账号5h", winOf(p, false), colMagenta, now, false),
		limitSeg("账号7d", winOf(p, true), colAqua, now, true),
	}
	// 本项目受限时才追加一段。读不到文件（新会话的头 30 秒、或没启用闸门）
	// 一律当作未受限——**宁可不提示，也不能凭空说人受限了**。
	if seg := projectLimitSeg(readProjectQuota()); seg != "" {
		segs = append(segs, seg)
	}
	sep := fmt.Sprintf(" \x1b[38;5;%dm│\x1b[0m ", colGray)
	return strings.Join(segs, sep)
}

// projectQuotaPath是 worker-agent 写入本项目受限状态的位置。
const projectQuotaPath = "/tmp/ccw-project-quota"

func readProjectQuota() string {
	b, err := os.ReadFile(projectQuotaPath)
	if err != nil {
		return ""
	}
	return string(b)
}

// projectLimitSeg把 "over=1 reason=xxx" 翻成一段提示；未受限返回空。
//
// **原因必须显示**：pool_exhausted 是账号池被别的项目吃光了，
// five_hour_limit 是你自己这个项目到顶了——两者该找谁、怎么办完全不同。
func projectLimitSeg(raw string) string {
	if !strings.Contains(raw, "over=1") {
		return ""
	}
	label := "本项目受限"
	switch {
	case strings.Contains(raw, "reason=five_hour_limit"):
		label = "本项目5h已满"
	case strings.Contains(raw, "reason=seven_day_limit"):
		label = "本项目7d已满"
	case strings.Contains(raw, "reason=pool_exhausted"):
		label = "账号池已满"
	}
	return "\x1b[1;31m" + label + "\x1b[0m"
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
