package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"ccw/internal/quota"
)

// 状态栏说"剩多少"——使用者关心的是还能干多久，不是已经用了多少。
func TestQuotaStatusShowsRemaining(t *testing.T) {
	got := quotaStatus(quota.Decision{FiveHourUsed: 250, SevenDayUsed: 0}, 1000, 10000)
	if !strings.Contains(got, "剩75%") {
		t.Errorf("5h 用了 250/1000 应显示剩75%%：%s", got)
	}
	if !strings.Contains(got, "剩100%") {
		t.Errorf("7d 没用应显示剩100%%：%s", got)
	}
	if !strings.Contains(got, "5h") || !strings.Contains(got, "7d") {
		t.Errorf("两个窗口都要显示：%s", got)
	}
}

// 超额要显眼。
func TestQuotaStatusMarksOver(t *testing.T) {
	got := quotaStatus(quota.Decision{Over: true, FiveHourUsed: 1000}, 1000, 10000)
	if !strings.Contains(got, "受限") {
		t.Errorf("超额应标出来：%s", got)
	}
	if !strings.Contains(got, "剩0%") {
		t.Errorf("用满应显示剩0%%：%s", got)
	}
}

// 限额为 0（未配置）时不能除零，也不该显示一个假的百分比。
func TestQuotaStatusHandlesZeroLimit(t *testing.T) {
	got := quotaStatus(quota.Decision{}, 0, 0)
	if strings.Contains(got, "%") {
		t.Errorf("没有限额时不该编一个百分比：%s", got)
	}
}

// 用量超过限额时不能算出负数或超过 100%。
func TestQuotaStatusClamps(t *testing.T) {
	got := quotaStatus(quota.Decision{FiveHourUsed: 5000, SevenDayUsed: -10}, 1000, 10000)
	if strings.Contains(got, "剩-") {
		t.Errorf("不该出现负剩余：%s", got)
	}
	for _, bad := range []string{"剩101%", "剩200%"} {
		if strings.Contains(got, bad) {
			t.Errorf("剩余不该超过100%%：%s", got)
		}
	}
}

// 进度条的格子数恒定，否则状态栏宽度会跳。
func TestQuotaStatusBarWidthIsStable(t *testing.T) {
	for _, used := range []int64{0, 1, 500, 999, 1000} {
		s := pctLeft("5h", used, 1000)
		n := strings.Count(s, "▓") + strings.Count(s, "░")
		if n != 10 {
			t.Errorf("used=%d 时格子数为 %d，应恒为 10：%s", used, n, s)
		}
	}
}

// 账号池耗尽时项目自己的用量可能还很低——只写"受限"会让状态栏同时说
// "剩90%"和"受限"，看的人无从理解。必须写清楚是被什么拦的。
func TestQuotaStatusExplainsWhyLimited(t *testing.T) {
	// 项目用量很低，但账号池耗尽
	got := quotaStatus(quota.Decision{
		Over: true, Reason: "pool_exhausted", FiveHourUsed: 100, SevenDayUsed: 100,
	}, 1000, 10000)
	if !strings.Contains(got, "剩90%") {
		t.Errorf("项目级百分比应如实显示：%s", got)
	}
	if !strings.Contains(got, "账号池") {
		t.Errorf("必须说明是账号池拦的，否则和'剩90%%'自相矛盾：%s", got)
	}

	for reason, want := range map[string]string{
		"five_hour_limit": "5h",
		"seven_day_limit": "7d",
		"pool_exhausted":  "账号池",
	} {
		g := quotaStatus(quota.Decision{Over: true, Reason: reason}, 1000, 10000)
		if !strings.Contains(g, "受限·"+want) {
			t.Errorf("reason=%q 应显示 受限·%s，got %s", reason, want, g)
		}
	}
	// 认不出的原因不该拼出一个空的"受限·"
	g := quotaStatus(quota.Decision{Over: true, Reason: "something_new"}, 1000, 10000)
	if strings.Contains(g, "受限·") {
		t.Errorf("认不出的原因应只显示'受限'：%s", g)
	}
}

// statusLine 渲染的是 Claude 自己的界面，用 ANSI 而不是 tmux 的 #[fg=] 格式。
// 混用的表现是把 "#[fg=green]" 这几个字原样打在状态行上。
func TestQuotaStatusUsesANSINotTmuxFormat(t *testing.T) {
	got := quotaStatus(quota.Decision{Over: true, Reason: "pool_exhausted"}, 1000, 10000)
	if strings.Contains(got, "#[") {
		t.Errorf("不该再有 tmux 格式串（会被原样打出来）：%q", got)
	}
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("应使用 ANSI 转义上色：%q", got)
	}
	// 单行：statusLine 每个换行都会多渲染一行，把 Claude 的界面往上顶
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("状态行必须是单行：%q", got)
	}
}

// managed-settings 里的命令在文件不存在时必须退出 0。
// 新会话在第一次额度循环（≤30秒）之前一定没有这个文件——**每次都会走到**。
func TestManagedSettingsStatusLineToleratesMissingFile(t *testing.T) {
	b, err := os.ReadFile("../../deploy/claude-managed-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		StatusLine struct {
			Type            string `json:"type"`
			Command         string `json:"command"`
			RefreshInterval int    `json:"refreshInterval"`
		} `json:"statusLine"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("managed-settings 不是合法 JSON：%v", err)
	}
	if cfg.StatusLine.Type != "command" {
		t.Errorf("type 应为 command，got %q", cfg.StatusLine.Type)
	}
	if !strings.Contains(cfg.StatusLine.Command, quotaFile) {
		t.Errorf("命令应读 %s，got %q", quotaFile, cfg.StatusLine.Command)
	}
	if !strings.Contains(cfg.StatusLine.Command, "|| true") {
		t.Error("文件不存在时必须退出 0；新会话的头 30 秒一定没有这个文件")
	}
	if cfg.StatusLine.RefreshInterval <= 0 {
		t.Error("要定时刷新，否则空闲时额度永远停在打开那一刻")
	}
}
