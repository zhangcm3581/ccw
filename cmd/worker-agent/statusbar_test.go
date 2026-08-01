package main

import (
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
