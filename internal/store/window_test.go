package store

import (
	"testing"
	"time"
)

// windowStart 决定"账号重置之后，被断开的项目多久恢复"。
//
// 关键场景（用户 2026-08-03 提的）：A 用满自己那份被断开 → 没有会话 →
// **没人回传新的 resets_at** → 只剩一个已经过去的旧值。
// 那个旧值仍然足以说明"重置已经发生"，它之前的用量属于上一个窗口。
// 不用它的话，A 要等自己那 5 小时慢慢滑完才恢复，而不是在重置那一刻。
func TestWindowStart(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	const win = 5 * time.Hour
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	cases := []struct {
		name string
		in   *time.Time
		want time.Time
	}{
		{"没拿到过快照 → 零值（调用方退回滚动）", nil, time.Time{}},
		{"重置在未来 2h → 窗口起点是 3h 前", at(2 * time.Hour), now.Add(-3 * time.Hour)},
		{"刚过去 1 分钟 → 起点就是那一刻，重置前的用量全部出局",
			at(-time.Minute), now.Add(-time.Minute)},
		{"过去 2h → 起点是那一刻（窗口只有 2h 长，正确）",
			at(-2 * time.Hour), now.Add(-2 * time.Hour)},
		{"快照太旧（8h 前）→ 退回滚动，否则把 8h 用量塞进 5h 窗口",
			at(-8 * time.Hour), now.Add(-win)},
	}
	for _, c := range cases {
		got := windowStart(c.in, now, win)
		if !got.Equal(c.want) {
			t.Errorf("%s：got %v, want %v", c.name, got, c.want)
		}
	}
}

// 恢复的那一刻：重置前用满、重置后没用 → 窗口起点跳到重置点，用量归零。
func TestProjectRecoversAtReset(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	reset := now.Add(-time.Minute) // 一分钟前刚重置
	start := windowStart(&reset, now, 5*time.Hour)

	// 重置前 10 分钟产生的用量，必须落在窗口外
	before := now.Add(-10 * time.Minute)
	if !before.Before(start) {
		t.Errorf("重置前的用量应落在窗口外：用量时刻 %v，窗口起点 %v", before, start)
	}
	// 重置后产生的落在窗口内
	after := now.Add(-30 * time.Second)
	if after.Before(start) {
		t.Errorf("重置后的用量应计入：用量时刻 %v，窗口起点 %v", after, start)
	}
}

// 7 天窗口走同一个函数，只是长度不同——但"太旧"那条边界跟着长度走，
// 用 5h 的直觉去想 7d 会判断错：一个 8 小时前的 resets_at 对 5h 窗口是
// "太旧、退回滚动"，对 7d 窗口却仍然有效、必须采用。
func TestWindowStartSevenDay(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	const win = 7 * 24 * time.Hour
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	cases := []struct {
		name string
		in   *time.Time
		want time.Time
	}{
		{"重置在未来 19h → 起点是 6天5小时前", at(19 * time.Hour), now.Add(19*time.Hour - win)},
		{"刚过去 1 分钟 → 起点就是那一刻", at(-time.Minute), now.Add(-time.Minute)},
		// **这一条是 5h 直觉会判断错的地方**
		{"过去 8 小时 → 对 7d 窗口仍然有效，必须采用", at(-8 * time.Hour), now.Add(-8 * time.Hour)},
		{"过去 3 天 → 仍在 7 天之内，采用", at(-3 * 24 * time.Hour), now.Add(-3 * 24 * time.Hour)},
		{"过去 9 天 → 超出一个窗口，退回滚动", at(-9 * 24 * time.Hour), now.Add(-win)},
		{"没拿到过快照 → 零值", nil, time.Time{}},
	}
	for _, c := range cases {
		if got := windowStart(c.in, now, win); !got.Equal(c.want) {
			t.Errorf("%s：got %v, want %v", c.name, got, c.want)
		}
	}
}

// 两个窗口各用各的长度。
//
// **这条必须打在成对计算上，不能各自调 windowStart 传自己的长度**——
// 真正容易写错的是"哪个窗口配哪个长度"，而那种错误只在运行时表现为
// "7 天窗口按 5 小时截断"：大量用量被误判成已滑出，闸门形同虚设，且不报错。
// 第一版测试就是分别调纯函数写的，把 7d 的长度改成 5h 它照样通过。
func TestBothWindowsUseTheirOwnLength(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * time.Hour) // 对 5h 太旧、对 7d 有效

	w := windowsFrom(&old, &old, now)
	five, seven := w.FiveHourStart, w.SevenDayStart

	if !five.Equal(now.Add(-5 * time.Hour)) {
		t.Errorf("5h 窗口应退回滚动，got %v", five)
	}
	if !seven.Equal(old) {
		t.Errorf("7d 窗口应采用该重置点，got %v", seven)
	}
	if five.Equal(seven) {
		t.Error("两个窗口不该算出同一个起点——说明长度被写死了")
	}
}
