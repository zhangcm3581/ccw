package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"ccw/internal/quota"
)

// 用真实账号用量校准池上限（2026-08-02）。
//
// 档位是「百分比 × 账号池上限」。池上限原本是个拍脑袋的数——那样"给你 33%"
// 到底是不是三分之一，谁也说不准。
//
// 唯一的真值来自 Claude 自己：状态行每 10 秒收到账号级 rate_limits 并写成
// /tmp/ccw-account-usage。这里把它读回来，反推池上限。
//
// **快照只在有人开着会话时更新**——官方 CLI 没有 usage 命令，这是唯一的入口。
// 所以太旧的快照要丢掉：拿一周前的百分比去除以今天的累计量，结果毫无意义。

// snapshotMaxAge是快照的有效期。
//
// 取 10 分钟：状态行每 10 秒写一次，会话开着时永远新鲜；
// 会话一关就迅速过期，不会拿旧数据去校准。
const snapshotMaxAge = 10 * time.Minute

// accountSnapshot是从容器里读回来的一份账号用量快照。
type accountSnapshot struct {
	At            time.Time
	FiveHourPct   float64
	SevenDayPct   float64
	FiveHourReset int64
	SevenDayReset int64
}

// readAccountSnapshot从容器里取快照。取不到（文件不存在、格式不对、太旧）返回 false。
func readAccountSnapshot(ctx context.Context, container string, now time.Time) (accountSnapshot, bool) {
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "cat", "/tmp/ccw-account-usage")
	out, err := cmd.Output()
	if err != nil {
		return accountSnapshot{}, false
	}
	return parseAccountSnapshot(string(out), now)
}

// parseAccountSnapshot解析 "at=… five_hour_pct=… seven_day_pct=…" 这一行。
func parseAccountSnapshot(raw string, now time.Time) (accountSnapshot, bool) {
	var s accountSnapshot
	var haveAt, have5, have7 bool
	for _, kv := range strings.Fields(strings.TrimSpace(raw)) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "at":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return s, false
			}
			s.At, haveAt = time.Unix(n, 0), true
		case "five_hour_pct":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return s, false
			}
			s.FiveHourPct, have5 = f, true
		case "seven_day_pct":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return s, false
			}
			s.SevenDayPct, have7 = f, true
		case "five_hour_resets":
			s.FiveHourReset, _ = strconv.ParseInt(v, 10, 64)
		case "seven_day_resets":
			s.SevenDayReset, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if !haveAt || !have5 || !have7 {
		return s, false
	}
	// **太旧就不算数**：快照只在有人开着会话时更新，拿一周前的百分比
	// 去除以今天的累计量，结果毫无意义。
	if now.Sub(s.At) > snapshotMaxAge || s.At.After(now.Add(time.Minute)) {
		return s, false
	}
	return s, true
}

// poolCalibrator是校准需要的存储能力。
type poolCalibrator interface {
	AccountPoolLimits(ctx context.Context, accountID string) (int64, int64, error)
	PoolUsed(ctx context.Context, accountID string, since time.Time) (int64, error)
	SetAccountPoolLimits(ctx context.Context, accountID string, fiveHour, sevenDay int64) error
}

// calibratePool用一份快照校准账号池上限。返回是否写了库。
func calibratePool(ctx context.Context, st poolCalibrator, accountID string,
	snap accountSnapshot, now time.Time) (bool, error) {
	cur5, cur7, err := st.AccountPoolLimits(ctx, accountID)
	if err != nil {
		return false, err
	}
	used5, err := st.PoolUsed(ctx, accountID, now.Add(-5*time.Hour))
	if err != nil {
		return false, err
	}
	used7, err := st.PoolUsed(ctx, accountID, now.Add(-7*24*time.Hour))
	if err != nil {
		return false, err
	}
	new5, ok5 := quota.CalibratePool(quota.CalibrateInput{Current: cur5, PoolUsed: used5, AccountPct: snap.FiveHourPct})
	new7, ok7 := quota.CalibratePool(quota.CalibrateInput{Current: cur7, PoolUsed: used7, AccountPct: snap.SevenDayPct})
	if !ok5 && !ok7 {
		return false, nil
	}
	if err := st.SetAccountPoolLimits(ctx, accountID, new5, new7); err != nil {
		return false, fmt.Errorf("写回池上限失败: %w", err)
	}
	return true, nil
}
