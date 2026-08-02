package main

import (
	"strings"
	"testing"

	"ccw/internal/quota"
)

// 写进容器那一行的格式：状态行按 over=1 与 reason= 解析，两边必须对得上。
func TestProjectQuotaLine(t *testing.T) {
	if got := projectQuotaLine(quota.Decision{}, quota.Limits{}); !strings.HasPrefix(got, "over=0 reason=") {
		t.Errorf("未受限应写 over=0，got %q", got)
	}
	got := projectQuotaLine(quota.Decision{Over: true, Reason: "five_hour_limit"}, quota.Limits{})
	if !strings.Contains(got, "over=1") || !strings.Contains(got, "reason=five_hour_limit") {
		t.Errorf("受限时应带出原因，got %q", got)
	}
	// **单行**：状态行把每个换行渲染成一行，多一个就把界面往上顶
	if strings.ContainsAny(projectQuotaLine(quota.Decision{Over: true, Reason: "x"}, quota.Limits{}), "\n\r") {
		t.Error("必须是单行")
	}
}

// 带上 used/limit：状态行要显示的是**分配给这个项目的那一份**用了多少。
func TestProjectQuotaLineCarriesAllocation(t *testing.T) {
	got := projectQuotaLine(
		quota.Decision{FiveHourUsed: 250_000, SevenDayUsed: 1_200_000},
		quota.Limits{FiveHour: 3_168_000, SevenDay: 19_800_000})
	for _, want := range []string{
		"five_used=250000", "five_limit=3168000",
		"seven_used=1200000", "seven_limit=19800000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q：%s", want, got)
		}
	}
}
