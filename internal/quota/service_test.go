package quota

import (
	"context"
	"math"
	"testing"
	"time"
)

type evt struct {
	at time.Time
	n  int64
}

type memReader struct {
	events map[string][]evt
	pool   map[string][]evt
}

func (m *memReader) WindowUsed(_ context.Context, pid string, since time.Time) (int64, error) {
	var s int64
	for _, e := range m.events[pid] {
		if e.at.After(since) {
			s += e.n
		}
	}
	return s, nil
}

func (m *memReader) PoolUsed(_ context.Context, aid string, since time.Time) (int64, error) {
	var s int64
	for _, e := range m.pool[aid] {
		if e.at.After(since) {
			s += e.n
		}
	}
	return s, nil
}

func ago(now time.Time, d time.Duration, n int64) evt {
	return evt{at: now.Add(-d), n: n}
}

func TestWindowBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	r := &memReader{events: map[string][]evt{
		"pa": {ago(now, 4*time.Hour, 100), ago(now, 6*time.Hour, 500), ago(now, 8*24*time.Hour, 9000)},
	}}
	s := Service{Reader: r}
	d, err := s.Check(context.Background(), "pa", "acc",
		Limits{FiveHour: 1000, SevenDay: 1000, PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.FiveHourUsed != 100 { // 6小时前的不算
		t.Fatalf("5h window wrong: %d", d.FiveHourUsed)
	}
	if d.SevenDayUsed != 600 { // 8天前的不算
		t.Fatalf("7d window wrong: %d", d.SevenDayUsed)
	}
	if d.Over {
		t.Fatalf("must not be over: %+v", d)
	}
}

func TestProjectIsolationAOverBFree(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	r := &memReader{events: map[string][]evt{
		"pa": {ago(now, time.Hour, 2000)},
		"pb": {ago(now, time.Hour, 10)},
	}}
	s := Service{Reader: r}
	l := Limits{FiveHour: 1000, SevenDay: 100000, PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}
	da, _ := s.Check(context.Background(), "pa", "acc", l, now)
	db, _ := s.Check(context.Background(), "pb", "acc", l, now)
	if !da.Over || da.Reason != "five_hour_limit" {
		t.Fatalf("A must be over: %+v", da)
	}
	if db.Over {
		t.Fatalf("B must still be allowed: %+v", db)
	}
}

func TestPoolSafetyMarginStopsBoth(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	r := &memReader{
		events: map[string][]evt{"pa": {ago(now, time.Hour, 10)}, "pb": {ago(now, time.Hour, 10)}},
		pool:   map[string][]evt{"acc": {ago(now, time.Hour, 9800)}},
	}
	s := Service{Reader: r}
	// 5小时池剩余=10000-9800=200，不大于Reserve+SafetyMargin=300 → 双双拒绝
	l := Limits{FiveHour: 100000, SevenDay: 100000, PoolFiveHour: 10000, PoolSevenDay: 1 << 40, Reserve: 100, SafetyMargin: 200}
	for _, pid := range []string{"pa", "pb"} {
		d, _ := s.Check(context.Background(), pid, "acc", l, now)
		if !d.Over || d.Reason != "pool_exhausted" {
			t.Fatalf("%s must be pool_exhausted: %+v", pid, d)
		}
	}
	// 7天池同样受保护：5小时充裕但7天耗尽时也必须拒绝
	l2 := Limits{FiveHour: 100000, SevenDay: 100000, PoolFiveHour: 1 << 40, PoolSevenDay: 10000, Reserve: 100, SafetyMargin: 200}
	d, _ := s.Check(context.Background(), "pa", "acc", l2, now)
	if !d.Over || d.Reason != "pool_exhausted" {
		t.Fatalf("7d pool must also be protected: %+v", d)
	}
}

// 挂了档位的项目，限额由「比例 × 账号池上限」推导，而不是用写死的绝对值。
func TestApplyTier(t *testing.T) {
	base := Limits{FiveHour: 1_000_000, SevenDay: 10_000_000,
		PoolFiveHour: 9_600_000, PoolSevenDay: 60_000_000}

	// 7x = 33%
	got := ApplyTier(base, 3300, true)
	if got.FiveHour != 3_168_000 {
		t.Errorf("33%% × 9.6M 应为 3,168,000，got %d", got.FiveHour)
	}
	if got.SevenDay != 19_800_000 {
		t.Errorf("33%% × 60M 应为 19,800,000，got %d", got.SevenDay)
	}
	// 池上限不该被改动——它是全部档位共同的分母
	if got.PoolFiveHour != base.PoolFiveHour {
		t.Error("不该改动池上限")
	}

	// 没挂档位：沿用绝对限额，已有部署不因迁移换一套数
	if got := ApplyTier(base, 0, false); got.FiveHour != base.FiveHour {
		t.Errorf("无档位应沿用绝对限额，got %d", got.FiveHour)
	}
	// 比例非法时同样不动，宁可维持原状也不要算出 0（0 意味着立刻全员受限）
	if got := ApplyTier(base, 0, true); got.FiveHour != base.FiveHour {
		t.Errorf("比例为0时应沿用绝对限额，got %d", got.FiveHour)
	}
	if got := ApplyTier(base, -5, true); got.FiveHour != base.FiveHour {
		t.Errorf("负比例应沿用绝对限额，got %d", got.FiveHour)
	}
}

// 三个默认档位加起来是 68%，不是 100%——刻意留了余量。
// 这条测试记录该意图：哪天有人把它们改到超过 100%，说明分配方式变了，
// 该先想清楚而不是让闸门去承受超卖。
func TestDefaultTiersLeaveHeadroom(t *testing.T) {
	total := 1000 + 2500 + 3300 // 2x + 5x + 7x
	if total > 10000 {
		t.Errorf("默认档位合计 %d bp 已超过账号池，超卖时闸门会同时拦住多个项目", total)
	}
}

// **全新安装的默认状态就会触发这个 bug**：迁移 002 把 pool_*_limit 的默认值
// 设成 MaxInt64 当"无限制"哨兵，而 MaxInt64 × 3300 回绕之后 /10000 得 0——
// 挂个档位就把项目限额变成 0，立刻永久受限，且发生在校准还没跑起来的时候。
func TestApplyTierRefusesUncalibratedPool(t *testing.T) {
	base := Limits{FiveHour: 1_000_000, SevenDay: 10_000_000}

	// 未校准（MaxInt64 哨兵）
	l := base
	l.PoolFiveHour, l.PoolSevenDay = math.MaxInt64, math.MaxInt64
	got := ApplyTier(l, 3300, true)
	if got.FiveHour != base.FiveHour || got.SevenDay != base.SevenDay {
		t.Errorf("池未校准时应沿用绝对限额，got 5h=%d 7d=%d", got.FiveHour, got.SevenDay)
	}

	// 池为 0 或负数
	for _, pool := range []int64{0, -1} {
		l = base
		l.PoolFiveHour, l.PoolSevenDay = pool, pool
		if got := ApplyTier(l, 3300, true); got.FiveHour != base.FiveHour {
			t.Errorf("池=%d 时应沿用绝对限额，got %d", pool, got.FiveHour)
		}
	}

	// 只有一个窗口算不出来时，**整个不套用**——只换一半会得到一组自相矛盾的
	// 限额（5h 按档位、7d 按绝对值），排查时极难看懂。
	l = base
	l.PoolFiveHour, l.PoolSevenDay = 9_600_000, math.MaxInt64
	if got := ApplyTier(l, 3300, true); got.FiveHour != base.FiveHour {
		t.Errorf("任一窗口算不出来就该整个不套用，got 5h=%d", got.FiveHour)
	}

	// 正常校准过的池仍要正确套用
	l = base
	l.PoolFiveHour, l.PoolSevenDay = 9_600_000, 60_000_000
	if got := ApplyTier(l, 3300, true); got.FiveHour != 3_168_000 {
		t.Errorf("正常池应套用档位，got %d", got.FiveHour)
	}
}

// 溢出可能回绕成**正数**——那样得到的限额看着合法，实际是一个毫无关系的数。
//
// 不变式：一份份额永远不可能大于整个池，也不可能是 0。
// 用一批大到会溢出的池值扫一遍，比盯住某个具体输入更靠得住。
func TestApplyTierNeverExceedsPool(t *testing.T) {
	base := Limits{FiveHour: 1_000_000, SevenDay: 10_000_000}
	for _, pool := range []int64{
		math.MaxInt64, math.MaxInt64 / 2, math.MaxInt64 / 3,
		3_000_000_000_000_000_000, 1 << 62, 1 << 55, 9_600_000,
	} {
		for _, bp := range []int{1, 1000, 3300, 10000} {
			l := base
			l.PoolFiveHour, l.PoolSevenDay = pool, pool
			got := ApplyTier(l, bp, true)
			// 要么原样沿用绝对限额（算不出来），要么是一个不超过池的正数
			if got.FiveHour == base.FiveHour && got.SevenDay == base.SevenDay {
				continue
			}
			if got.FiveHour <= 0 || got.FiveHour > pool {
				t.Errorf("pool=%d bp=%d 算出 5h=%d：份额不能为0、也不能大于整个池",
					pool, bp, got.FiveHour)
			}
		}
	}
}
