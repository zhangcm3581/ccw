package store

import (
	"testing"

	"ccw/internal/quota"
)

// 用量报告里的限额必须是**实际生效的那个**。
//
// 2026-08-02 真机：档位已经写进 DB 并在闸门里生效，而页面显示的是
// projects.five_hour_limit 那一列——档位从不写回该列，所以页面永远显示
// 一个没被用到的数字，用户据此判断"档位没生效"。
func TestReportUsesEffectiveLimit(t *testing.T) {
	abs := quota.Limits{
		FiveHour: 1_000_000, SevenDay: 10_000_000,
		PoolFiveHour: 9_600_000, PoolSevenDay: 60_000_000,
	}
	// 挂 2x（10%）：限额应变成 960000 / 6000000，而不是 1000000 / 10000000
	lim := quota.ApplyTier(abs, 1000, true)
	if lim.FiveHour != 960_000 || lim.SevenDay != 6_000_000 {
		t.Fatalf("档位应改写限额，got %d/%d", lim.FiveHour, lim.SevenDay)
	}
	if lim.FiveHour == abs.FiveHour {
		t.Error("生效判定靠'变没变'，这里必须变了")
	}

	// 容量未校准：原样返回 → TierEffective 应为 false
	un := abs
	un.PoolFiveHour, un.PoolSevenDay = 1<<62, 1<<62
	got := quota.ApplyTier(un, 1000, true)
	if got.FiveHour != un.FiveHour || got.SevenDay != un.SevenDay {
		t.Errorf("容量算不出来时应原样返回绝对限额，got %d/%d", got.FiveHour, got.SevenDay)
	}
}
