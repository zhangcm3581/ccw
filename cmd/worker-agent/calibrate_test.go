package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseAccountSnapshot(t *testing.T) {
	now := time.Unix(1738400600, 0)
	raw := "at=1738400000 five_hour_pct=11.50 seven_day_pct=51.00 five_hour_resets=1738425600 seven_day_resets=1738857600"
	s, ok := parseAccountSnapshot(raw, now)
	if !ok {
		t.Fatal("应能解析")
	}
	if s.FiveHourPct != 11.5 || s.SevenDayPct != 51 {
		t.Errorf("百分比解析错：%+v", s)
	}
	if s.FiveHourReset != 1738425600 {
		t.Errorf("重置时间解析错：%+v", s)
	}
}

// **太旧的快照必须丢掉**：它只在有人开着会话时更新，拿一周前的百分比
// 去除以今天的累计量，结果毫无意义——而那正是校准的分母。
func TestParseAccountSnapshotRejectsStale(t *testing.T) {
	now := time.Unix(1738400000, 0)
	old := "at=" + itoa64(now.Add(-2*time.Hour).Unix()) + " five_hour_pct=11 seven_day_pct=51"
	if _, ok := parseAccountSnapshot(old, now); ok {
		t.Error("两小时前的快照不该被采用")
	}
	fresh := "at=" + itoa64(now.Add(-time.Minute).Unix()) + " five_hour_pct=11 seven_day_pct=51"
	if _, ok := parseAccountSnapshot(fresh, now); !ok {
		t.Error("一分钟前的快照应可用")
	}
	// 未来时间：时钟异常，同样不可信
	future := "at=" + itoa64(now.Add(time.Hour).Unix()) + " five_hour_pct=11 seven_day_pct=51"
	if _, ok := parseAccountSnapshot(future, now); ok {
		t.Error("来自未来的快照不该被采用")
	}
}

func TestParseAccountSnapshotRejectsGarbage(t *testing.T) {
	now := time.Unix(1738400000, 0)
	for _, raw := range []string{
		"", "garbage", "at=abc five_hour_pct=1 seven_day_pct=1",
		"at=1738400000 five_hour_pct=xx seven_day_pct=1",
		"at=1738400000 five_hour_pct=1",   // 缺 7d
		"five_hour_pct=1 seven_day_pct=1", // 缺 at
	} {
		if _, ok := parseAccountSnapshot(raw, now); ok {
			t.Errorf("%q 不该被采用", raw)
		}
	}
}

type fakeCalStore struct {
	five, seven    int64
	used5, used7   int64
	wrote5, wrote7 int64
	writes         int
}

func (f *fakeCalStore) AccountPoolLimits(context.Context, string) (int64, int64, error) {
	return f.five, f.seven, nil
}
func (f *fakeCalStore) PoolUsed(_ context.Context, _ string, since time.Time) (int64, error) {
	// 靠 since 区分是 5h 还是 7d 窗口
	if time.Since(since) > 24*time.Hour {
		return f.used7, nil
	}
	return f.used5, nil
}
func (f *fakeCalStore) SetAccountPoolLimits(_ context.Context, _ string, a, b int64) error {
	f.wrote5, f.wrote7, f.writes = a, b, f.writes+1
	return nil
}

func TestCalibratePoolWritesBack(t *testing.T) {
	now := time.Now()
	st := &fakeCalStore{used5: 1_052_098, used7: 3_053_579}
	got5, _, err := calibratePool(context.Background(), st, "acct",
		accountSnapshot{At: now, FiveHourPct: 11, SevenDayPct: 51}, now)
	if err != nil || got5 == 0 {
		t.Fatalf("应写回：%v %v", got5, err)
	}
	// 首次标定直接采用估计值：1052098/0.11 ≈ 9.56M
	if st.wrote5 < 9_500_000 || st.wrote5 > 9_600_000 {
		t.Errorf("5h 池上限应约 9.56M，got %d", st.wrote5)
	}
	if st.wrote7 < 5_900_000 || st.wrote7 > 6_100_000 {
		t.Errorf("7d 池上限应约 5.99M，got %d", st.wrote7)
	}
}

// 两个窗口都不满足防呆时**不写库**——避免每 30 秒一次无意义的 UPDATE。
func TestCalibratePoolSkipsWhenUnreliable(t *testing.T) {
	now := time.Now()
	st := &fakeCalStore{five: 10_000_000, seven: 10_000_000, used5: 10, used7: 10}
	got5, _, err := calibratePool(context.Background(), st, "acct",
		accountSnapshot{At: now, FiveHourPct: 1, SevenDayPct: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got5 != 0 || st.writes != 0 {
		t.Errorf("数据不可信时不该写库，writes=%d", st.writes)
	}
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// 写快照的（状态行）与读快照的（worker-agent、Console）必须用同一个路径。
// 不一致的表现是"状态行明明写了、校准永远不发生"，而且两边都不报错。
func TestSnapshotPathMatchesWriter(t *testing.T) {
	b, err := os.ReadFile("../ccw-statusline/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `accountSnapshotPath = "`+snapshotPath+`"`) {
		t.Errorf("状态行写的路径与这里读的 %q 不一致", snapshotPath)
	}
	c, err := os.ReadFile("../../internal/provision/diagnostics.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c), snapshotPath) {
		t.Errorf("Console 读的路径与 %q 不一致", snapshotPath)
	}
}
