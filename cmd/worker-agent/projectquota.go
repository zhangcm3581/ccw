package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"ccw/internal/quota"
)

// 把「本项目是否受限」写进容器，供状态行显示（2026-08-02）。
//
// 状态行上那两段 5h/7d 来自 Claude 自己的 rate_limits——那是**整个账号**的用量，
// 同节点全部项目共用。而真正会关掉你终端的是**本项目**的内部额度闸门，
// 两者可以完全不一致：账号还剩 80%，你的项目却已经到顶。
//
// 界面上不说明这一点就是在误导，所以额外给状态行一个信号：受限时才出现一段，
// 平时什么都不加（不占宽度）。
//
// 落点仍是 /tmp——每容器一份（＝每项目一份），不进任何卷。
const projectQuotaFile = "/tmp/ccw-project-quota"

// projectQuotaLine渲染写进容器的那一行。格式刻意简单到不需要 JSON 解析。
func projectQuotaLine(d quota.Decision) string {
	over := 0
	if d.Over {
		over = 1
	}
	return fmt.Sprintf("over=%d reason=%s", over, d.Reason)
}

// writeProjectQuota把状态写进容器。
//
// **每轮都写，包括没受限的时候**：只在受限时写的话，额度恢复之后那行提示
// 会一直留在屏幕上，反而更糟。
func writeProjectQuota(ctx context.Context, container string, d quota.Decision) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container,
		"sh", "-c", "cat > "+projectQuotaFile)
	cmd.Stdin = strings.NewReader(projectQuotaLine(d))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
