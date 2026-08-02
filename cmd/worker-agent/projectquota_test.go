package main

import (
	"strings"
	"testing"

	"ccw/internal/quota"
)

// 写进容器那一行的格式：状态行按 over=1 与 reason= 解析，两边必须对得上。
func TestProjectQuotaLine(t *testing.T) {
	if got := projectQuotaLine(quota.Decision{}); got != "over=0 reason=" {
		t.Errorf("未受限应写 over=0，got %q", got)
	}
	got := projectQuotaLine(quota.Decision{Over: true, Reason: "five_hour_limit"})
	if !strings.Contains(got, "over=1") || !strings.Contains(got, "reason=five_hour_limit") {
		t.Errorf("受限时应带出原因，got %q", got)
	}
	// **单行**：状态行把每个换行渲染成一行，多一个就把界面往上顶
	if strings.ContainsAny(projectQuotaLine(quota.Decision{Over: true, Reason: "x"}), "\n\r") {
		t.Error("必须是单行")
	}
}
