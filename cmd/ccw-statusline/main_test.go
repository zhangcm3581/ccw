package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, s string) payload {
	t.Helper()
	var p payload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// 可见字符（剥掉 ANSI）——断言排版时只看它。
func visible(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// 进度条恒为 8 格——宽度会跳的话整行都在抖。
func TestBarIsAlwaysEightCells(t *testing.T) {
	for _, f := range []float64{0, 0.001, 0.0625, 0.5, 0.99, 1, 1.5, -1} {
		got := len([]rune(visible(bar(f, colCyan))))
		if got != barCells {
			t.Errorf("frac=%v 渲染出 %d 格，应恒为 %d：%q", f, got, barCells, visible(bar(f, colCyan)))
		}
	}
	if got := len([]rune(visible(emptyBar()))); got != barCells {
		t.Errorf("降级轨道也应是 %d 格，got %d", barCells, got)
	}
}

// 1/8 精度：不足一格时必须出过渡字符，而不是整格跳。
func TestBarUsesEighthPrecision(t *testing.T) {
	// 1/8 格 = 1/64 的比例
	one := visible(bar(1.0/64, colCyan))
	if !strings.ContainsRune(one, '▏') {
		t.Errorf("1/8 格应画成 ▏，got %q", one)
	}
	// 7/8 格
	seven := visible(bar(7.0/64, colCyan))
	if !strings.ContainsRune(seven, '▉') {
		t.Errorf("7/8 格应画成 ▉，got %q", seven)
	}
	// 整格：不该出现过渡字符
	fullCell := visible(bar(1.0/8, colCyan))
	for _, r := range partials {
		if strings.ContainsRune(fullCell, r) {
			t.Errorf("整格不该有过渡字符 %c：%q", r, fullCell)
		}
	}
	// 满条：全是填实的格，没有过渡字符
	full := visible(bar(1, colCyan))
	if strings.TrimSpace(full) != "" {
		t.Errorf("满条应全部由背景填实（可见字符全是空格），got %q", full)
	}
}

// 颜色：填充用 accent 背景，空槽用灰底，过渡格是 accent 前景 + 灰底。
func TestBarColors(t *testing.T) {
	half := bar(0.5, colMagenta)
	if !strings.Contains(half, fmt.Sprintf("\x1b[48;5;%dm", colMagenta)) {
		t.Error("填充段应使用 accent 背景")
	}
	if !strings.Contains(half, fmt.Sprintf("\x1b[48;5;%dm", colGray)) {
		t.Error("空槽应使用灰底轨道")
	}
	partial := bar(3.0/64, colAqua)
	if !strings.Contains(partial, fmt.Sprintf("\x1b[48;5;%dm\x1b[38;5;%dm", colGray, colAqua)) {
		t.Errorf("过渡格应是 accent 前景 + 灰底：%q", partial)
	}
	if !strings.HasSuffix(bar(0.5, colCyan), "\x1b[0m") {
		t.Error("必须复位，否则后面的文字会继承背景色")
	}
}

// 倒计时格式。
func TestCountdown(t *testing.T) {
	cases := []struct {
		d         time.Duration
		allowDays bool
		want      string
	}{
		{3*time.Hour + 25*time.Minute, false, "3h 25m"},
		{45 * time.Minute, false, "0h 45m"},
		{30 * time.Hour, false, "30h 0m"},                // 5h 段不用天
		{30 * time.Hour, true, "1d 6h 0m"},               // 7d 段超过 24h 用天
		{23*time.Hour + 59*time.Minute, true, "23h 59m"}, // 不足 24h 仍用小时
		{-time.Hour, true, ""},                           // 过期不显示负数
		{0, true, ""},
	}
	for _, c := range cases {
		if got := countdown(c.d, c.allowDays); got != c.want {
			t.Errorf("countdown(%v, %v) = %q, want %q", c.d, c.allowDays, got, c.want)
		}
	}
}

// 整行：四段、三个分隔符、context 显示已用、额度显示剩余。
func TestRenderFourSegments(t *testing.T) {
	now := time.Unix(1738400000, 0)
	p := parse(t, `{
	  "model": {"display_name": "Opus 4.7"},
	  "context_window": {"used_percentage": 25},
	  "rate_limits": {
	    "five_hour": {"used_percentage": 23.5, "resets_at": 1738425600},
	    "seven_day": {"used_percentage": 41.2, "resets_at": 1738857600}
	  }
	}`)
	v := visible(render(p, now))

	if n := strings.Count(v, "│"); n != 3 {
		t.Errorf("四段之间应有 3 个分隔符，got %d：%s", n, v)
	}
	if !strings.Contains(v, "Opus 4.7") {
		t.Errorf("缺模型名：%s", v)
	}
	if !strings.Contains(v, "context") || !strings.Contains(v, "25%") {
		t.Errorf("context 应显示已用 25%%：%s", v)
	}
	// 5h 用了 23.5% → 剩 77%（四舍五入）
	if !strings.Contains(v, "77%") {
		t.Errorf("5h 应显示剩余 77%%（100-23.5）：%s", v)
	}
	// 7d 用了 41.2% → 剩 59%
	if !strings.Contains(v, "59%") {
		t.Errorf("7d 应显示剩余 59%%（100-41.2）：%s", v)
	}
	if !strings.Contains(v, "7h 6m") {
		t.Errorf("5h 倒计时应为 7h 6m：%s", v)
	}
	if !strings.Contains(v, "5d 7h 6m") {
		t.Errorf("7d 倒计时超过 24h 应带天：%s", v)
	}
}

// 数据缺失时降级成灰条 + "--"，而不是整段消失或显示 0%。
// **0% 与"不知道"是两回事**：前者说"额度用光了"，后者说"没数据"。
func TestRenderDegradesOnMissingData(t *testing.T) {
	v := visible(render(parse(t, `{"model":{"display_name":"Sonnet"}}`), time.Now()))
	if n := strings.Count(v, "--"); n != 3 {
		t.Errorf("context/5h/7d 三段都应降级为 --，got %d：%s", n, v)
	}
	if strings.Contains(v, "0%") {
		t.Errorf("没数据不该显示 0%%（那是「用光了」的意思）：%s", v)
	}
	if n := strings.Count(v, "│"); n != 3 {
		t.Errorf("降级时段数不该变：%s", v)
	}
}

// 坏 JSON 也要出一行：状态行是常驻 UI，整行消失比显示 -- 更难排查。
func TestRenderSurvivesGarbage(t *testing.T) {
	var p payload
	_ = json.Unmarshal([]byte(`{not json`), &p)
	v := visible(render(p, time.Now()))
	if v == "" {
		t.Fatal("坏输入也应输出一行")
	}
	if !strings.Contains(v, "Claude") {
		t.Errorf("模型名缺失应有兜底，got %s", v)
	}
}

// used_percentage 缺失但 token 数在时，用 token 算——同一个对象里有现成数据，
// 没有理由降级成 --。
func TestContextFallsBackToTokenCount(t *testing.T) {
	p := parse(t, `{"context_window":{"total_input_tokens":50000,"context_window_size":200000}}`)
	v := visible(render(p, time.Now()))
	if !strings.Contains(v, "25%") {
		t.Errorf("50000/200000 应算出 25%%：%s", v)
	}
}

// 越界的百分比不能画出超长的条或负数。
func TestPercentagesAreClamped(t *testing.T) {
	p := parse(t, `{"context_window":{"used_percentage":150},
	 "rate_limits":{"five_hour":{"used_percentage":-20},"seven_day":{"used_percentage":250}}}`)
	v := visible(render(p, time.Now()))
	if strings.Contains(v, "150%") || strings.Contains(v, "-") {
		t.Errorf("百分比应被钳位：%s", v)
	}
	if !strings.Contains(v, "100%") || !strings.Contains(v, "0%") {
		t.Errorf("应钳到 100%% 与 0%%：%s", v)
	}
}

// 离谱的 resets_at 不能撑爆整行。
//
// 实测：用一个远未来的时间戳会渲染出 "2281787h 22m"，把前面几段挤出屏幕。
// 5 小时窗口最多 5 小时后重置、7 天窗口最多 7 天——超出这个量级只可能是
// 坏数据或时钟偏移，宁可不显示倒计时。
func TestCountdownRejectsAbsurdValues(t *testing.T) {
	for _, d := range []time.Duration{
		9 * 24 * time.Hour,
		365 * 24 * time.Hour,
		2281787 * time.Hour,
	} {
		if got := countdown(d, true); got != "" {
			t.Errorf("countdown(%v) = %q，超出合理量级应不显示", d, got)
		}
	}
	// 边界内的仍要显示
	if got := countdown(7*24*time.Hour, true); got == "" {
		t.Error("7 天整仍应显示")
	}
}

// displayWidth按终端列数算宽度：CJK 占两列。
// 之前这里用 rune 数，把中文标签少算了一半——量错了的守卫不如没有。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r > 0x2e80 {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// 整行宽度要可控——状态行被截断的话反而什么都看不清。
//
// 阈值按真实终端取：120 列是常见的宽终端，留出余量到 110。
// 这条守的是**失控增长**（比如坏的 resets_at 曾渲染出 "2281787h 22m"），
// 不是逐字节抠——为省几列去改用户指定的格式不值得。
func TestRenderStaysReasonablyShort(t *testing.T) {
	now := time.Unix(1738400000, 0)
	p := parse(t, `{
	  "model":{"display_name":"Sonnet 4.6"},
	  "context_window":{"used_percentage":88},
	  "rate_limits":{
	    "five_hour":{"used_percentage":5,"resets_at":1738417800},
	    "seven_day":{"used_percentage":9,"resets_at":1738900000}
	  }}`)
	v := visible(render(p, now))
	if n := displayWidth(v); n > 110 {
		t.Errorf("整行 %d 列，太宽会被截断：%s", n, v)
	}
	// 受限提示是额外一段，加上它也不能失控
	full := v + " │ " + visible(projectLimitSeg("over=1 reason=five_hour_limit"))
	if n := displayWidth(full); n > 130 {
		t.Errorf("带受限提示时 %d 列，过宽：%s", n, full)
	}
}

// 账号级与项目级是两回事，标签必须说清楚。
//
// 状态行上的 5h/7d 来自 Claude 的 rate_limits——那是**整个账号**的用量，
// 同节点全部项目共用。不标"账号"的话，看的人会以为那是自己项目的额度，
// 于是出现"明明还剩 80%，终端却被关了"这种无法理解的现象。
func TestLimitSegmentsAreLabelledAsAccountLevel(t *testing.T) {
	p := parse(t, `{"rate_limits":{"five_hour":{"used_percentage":20},"seven_day":{"used_percentage":30}}}`)
	v := visible(render(p, time.Now()))
	for _, want := range []string{"账号5h", "账号7d"} {
		if !strings.Contains(v, want) {
			t.Errorf("应标明是账号级用量，缺 %q：%s", want, v)
		}
	}
}

// 只在本项目受限时多出一段；平时不占宽度。
func TestProjectLimitSegment(t *testing.T) {
	if got := projectLimitSeg(""); got != "" {
		t.Errorf("读不到文件应视为未受限，got %q", got)
	}
	if got := projectLimitSeg("over=0 reason="); got != "" {
		t.Errorf("未受限不该有提示，got %q", got)
	}
	// **原因要分清**：账号池被别人吃光，和自己项目到顶，处理方式完全不同
	cases := map[string]string{
		"over=1 reason=five_hour_limit": "本项目5h已满",
		"over=1 reason=seven_day_limit": "本项目7d已满",
		"over=1 reason=pool_exhausted":  "账号池已满",
		"over=1 reason=":                "本项目受限",
	}
	for raw, want := range cases {
		if got := visible(projectLimitSeg(raw)); got != want {
			t.Errorf("projectLimitSeg(%q) = %q, want %q", raw, got, want)
		}
	}
}

// 坏内容不能被当成"受限"——凭空说人受限比不提示更糟。
func TestProjectLimitSegIgnoresGarbage(t *testing.T) {
	for _, raw := range []string{"garbage", "over=", "overover", "\x00\x01"} {
		if got := projectLimitSeg(raw); got != "" {
			t.Errorf("%q 不该判成受限，got %q", raw, got)
		}
	}
}
