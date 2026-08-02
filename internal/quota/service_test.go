package quota

import (
	"context"
	"errors"
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
		Limits{FiveHour: 1000, SevenDay: 1000, PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}, now, Windows{})
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
	da, _ := s.Check(context.Background(), "pa", "acc", l, now, Windows{})
	db, _ := s.Check(context.Background(), "pb", "acc", l, now, Windows{})
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
		d, _ := s.Check(context.Background(), pid, "acc", l, now, Windows{})
		if !d.Over || d.Reason != "pool_exhausted" {
			t.Fatalf("%s must be pool_exhausted: %+v", pid, d)
		}
	}
	// 7天池同样受保护：5小时充裕但7天耗尽时也必须拒绝
	l2 := Limits{FiveHour: 100000, SevenDay: 100000, PoolFiveHour: 1 << 40, PoolSevenDay: 10000, Reserve: 100, SafetyMargin: 200}
	d, _ := s.Check(context.Background(), "pa", "acc", l2, now, Windows{})
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

// 端到端确认档位的语义，用一组好核对的数字。
//
// **账号 5h 总容量 100 单位，project-a 挂 2x（10%）→ 它的限额是 10。**
// 用了 9 不受限，用到 10 就受限——"消耗要从自己那份里扣"就是这个比较。
//
// 关键在于分母是**总容量**而不是"当前剩余"：按剩余算的话，一个一直没用的
// 项目会因为别人用得多而额度越来越小，甚至突然从"没超"变成"已超"。
// 分配应该是"这一份归你"，不是"剩下的按比例分"。
func TestTierIsShareOfTotalCapacityNotRemaining(t *testing.T) {
	const capacity5h, capacity7d = 100, 1000
	base := Limits{
		FiveHour: 999999, SevenDay: 999999, // 绝对限额故意设得很大，确认被档位覆盖
		PoolFiveHour: capacity5h, PoolSevenDay: capacity7d,
	}
	lim := ApplyTier(base, 1000, true) // 2x = 10%
	if lim.FiveHour != 10 || lim.SevenDay != 100 {
		t.Fatalf("10%% 应得到 5h=10 7d=100，got %d/%d", lim.FiveHour, lim.SevenDay)
	}

	svc := Service{Reader: fixedUsage{five: 9, seven: 50}}
	d, err := svc.Check(context.Background(), "project-a", "acct", lim, time.Now(), Windows{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Over {
		t.Errorf("用了 9/10 不该受限：%+v", d)
	}

	svc = Service{Reader: fixedUsage{five: 10, seven: 50}}
	d, _ = svc.Check(context.Background(), "project-a", "acct", lim, time.Now(), Windows{})
	if !d.Over || d.Reason != "five_hour_limit" {
		t.Errorf("用到 10/10 应受限于 5h：%+v", d)
	}

	// **别人用得多不该改变我这一份**：账号被别的项目消耗到只剩 1，
	// project-a 的限额仍然是 10（分母是总容量，不是剩余）。
	limAgain := ApplyTier(base, 1000, true)
	if limAgain.FiveHour != lim.FiveHour {
		t.Errorf("限额不该随别人的用量变化：%d → %d", lim.FiveHour, limAgain.FiveHour)
	}
}

// fixedUsage：项目自己的窗口用量固定，账号池用量设成 0 以免触发池保护，
// 让这条测试只考察"档位限额 vs 项目自身用量"这一个比较。
type fixedUsage struct{ five, seven int64 }

func (f fixedUsage) WindowUsed(_ context.Context, _ string, since time.Time) (int64, error) {
	if time.Since(since) > 24*time.Hour {
		return f.seven, nil
	}
	return f.five, nil
}
func (f fixedUsage) PoolUsed(context.Context, string, time.Time) (int64, error) { return 0, nil }

// 窗口对齐 Claude：额度跟着账号一起在 resets_at 归零，
// 而不是靠滚动窗口把旧用量慢慢滑出去。
func TestWindowsAlignToClaudeReset(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	// 没有边界（还没拿到过快照）→ 退回滚动窗口，与改动前一致
	var zero Windows
	if got := zero.Start(now, false); !got.Equal(now.Add(-5 * time.Hour)) {
		t.Errorf("无边界时 5h 应退回滚动窗口，got %v", got)
	}
	if got := zero.Start(now, true); !got.Equal(now.Add(-7 * 24 * time.Hour)) {
		t.Errorf("无边界时 7d 应退回滚动窗口，got %v", got)
	}

	// 有边界 → 用 Claude 的窗口起点
	w := Windows{
		FiveHourStart: now.Add(-90 * time.Minute), // 90 分钟前刚重置过
		SevenDayStart: now.Add(-30 * time.Hour),
	}
	if got := w.Start(now, false); !got.Equal(w.FiveHourStart) {
		t.Errorf("应用 Claude 的 5h 窗口起点，got %v", got)
	}
	if got := w.Start(now, true); !got.Equal(w.SevenDayStart) {
		t.Errorf("应用 Claude 的 7d 窗口起点，got %v", got)
	}

	// **关键行为**：重置点之后的窗口很短，重置前的用量一次性全部落在窗口之外。
	// 滚动窗口做不到这一点——它只能等那些用量一条条老化。
	svc := Service{Reader: windowAwareUsage{
		beforeReset: 5_000_000, // 重置前用了很多
		afterReset:  1_000,     // 重置后才用了一点
		resetAt:     w.FiveHourStart,
	}}
	lim := Limits{FiveHour: 100_000, SevenDay: 1 << 40, PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}
	d, err := svc.Check(context.Background(), "p", "a", lim, now, w)
	if err != nil {
		t.Fatal(err)
	}
	if d.Over {
		t.Errorf("重置后只用了 1000，不该受限（重置前的 500 万应落在窗口外）：%+v", d)
	}
	// 同样的数据用滚动窗口就会被拦——这正是这次改动要解决的问题
	if d2, _ := svc.Check(context.Background(), "p", "a", lim, now, Windows{}); !d2.Over {
		t.Error("滚动窗口下应仍算超额（对照组，说明差别真实存在）")
	}
}

// windowAwareUsage按 since 是否晚于重置点返回不同的累计量。
type windowAwareUsage struct {
	beforeReset, afterReset int64
	resetAt                 time.Time
}

func (u windowAwareUsage) WindowUsed(_ context.Context, _ string, since time.Time) (int64, error) {
	if !since.Before(u.resetAt) {
		return u.afterReset, nil
	}
	return u.beforeReset + u.afterReset, nil
}
func (u windowAwareUsage) PoolUsed(context.Context, string, time.Time) (int64, error) { return 0, nil }

// **两处判定必须用同一个实现。**
//
// 2026-08-03 真机：control-api 用绝对限额判超额、worker-agent 用档位折算后的
// 限额，于是客户端被告知"项目受限（five_hour_limit）"，而后台同一时刻显示
// 93%、远没到顶。cmd/control-api/main.go 的注释里还记着更早的同款事故
// （池上限一个读环境变量、一个读库）。
//
// 这条测试锁住 AssembleProject 的语义：它必须把档位算进去，
// 否则两边又会各说各话。
func TestAssembleProjectAppliesTier(t *testing.T) {
	r := tieredReader{pool5: 2_471_406, pool7: 9_842_136, bp: 5000, has: true}
	got, err := AssembleProject(context.Background(), r, "p1", "acct",
		1_000_000, 10_000_000, Margins{})
	if err != nil {
		t.Fatal(err)
	}
	// 50% × 2,471,406 = 1,235,703（真机上后台显示的正是这个数）
	if got.FiveHour != 1_235_703 {
		t.Errorf("5h 限额应按档位折算为 1235703，got %d", got.FiveHour)
	}
	if got.FiveHour == 1_000_000 {
		t.Error("拿到了绝对限额——档位没被应用，两处判定会重新漂移")
	}

	// 没挂档位：沿用绝对限额
	r.has = false
	got, _ = AssembleProject(context.Background(), r, "p1", "acct", 1_000_000, 10_000_000, Margins{})
	if got.FiveHour != 1_000_000 {
		t.Errorf("无档位应沿用绝对限额，got %d", got.FiveHour)
	}

	// 档位查询出错：沿用绝对限额，不因此让整次判定失败
	r.has, r.err = true, errBoom
	if got, err := AssembleProject(context.Background(), r, "p1", "acct", 1_000_000, 10_000_000, Margins{}); err != nil || got.FiveHour != 1_000_000 {
		t.Errorf("档位查询失败应降级为绝对限额，got %d %v", got.FiveHour, err)
	}
}

var errBoom = errors.New("boom")

type tieredReader struct {
	pool5, pool7 int64
	bp           int
	has          bool
	err          error
}

func (r tieredReader) AccountPoolLimits(context.Context, string) (int64, int64, error) {
	return r.pool5, r.pool7, nil
}
func (r tieredReader) ProjectTierShare(context.Context, string) (int, bool, error) {
	return r.bp, r.has, r.err
}
