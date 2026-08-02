package quota

import "testing"

// 从未标定过时直接采用估计值。
func TestCalibrateFirstTimeAdoptsEstimate(t *testing.T) {
	// 真机数据：账号 11%，我们记了 1,052,098 单位
	got, ok := CalibratePool(CalibrateInput{Current: 0, PoolUsed: 1_052_098, AccountPct: 11})
	if !ok {
		t.Fatal("首次应校准")
	}
	if got < 9_500_000 || got > 9_600_000 {
		t.Errorf("池上限应约 9.56M，got %d", got)
	}
}

// 已有值时只缓慢移动——单次异常快照不该把基准一把改掉。
func TestCalibrateMovesGradually(t *testing.T) {
	got, ok := CalibratePool(CalibrateInput{Current: 10_000_000, PoolUsed: 2_000_000, AccountPct: 10})
	if !ok {
		t.Fatal("应校准")
	}
	// 估计值 20M，当前 10M，一步只走 20% → 12M
	if got != 12_000_000 {
		t.Errorf("应只走 20%%（10M→12M），got %d", got)
	}
	// 反复校准会收敛过去，但不会一步到位
	for i := 0; i < 20; i++ {
		got, _ = CalibratePool(CalibrateInput{Current: got, PoolUsed: 2_000_000, AccountPct: 10})
	}
	if got < 19_000_000 || got > 20_000_000 {
		t.Errorf("多轮后应收敛到约 20M，got %d", got)
	}
}

// **三条防呆**，每条都对应一种会算出离谱值的真实情形。
func TestCalibrateRefusesUnreliableInput(t *testing.T) {
	base := CalibrateInput{Current: 10_000_000, PoolUsed: 1_000_000, AccountPct: 20}

	// 百分比太小：窗口刚开始时必然经过，分母噪声被放大
	in := base
	in.AccountPct = 1
	if _, ok := CalibratePool(in); ok {
		t.Error("账号百分比过低时不该校准")
	}
	// 我们这边几乎没记到量
	in = base
	in.PoolUsed = 10
	if _, ok := CalibratePool(in); ok {
		t.Error("累计量过小时不该校准")
	}
	// 0% —— 除零
	in = base
	in.AccountPct = 0
	if got, ok := CalibratePool(in); ok || got != base.Current {
		t.Errorf("0%% 不该校准且应保持原值，got %d %v", got, ok)
	}
	// 负数与异常值
	for _, pct := range []float64{-5, -100} {
		in = base
		in.AccountPct = pct
		if _, ok := CalibratePool(in); ok {
			t.Errorf("百分比 %v 不该校准", pct)
		}
	}
}

// 估计值与当前值一致时不写库——避免每 30 秒一次无意义的 UPDATE。
func TestCalibrateNoOpWhenConverged(t *testing.T) {
	cur := int64(10_000_000)
	if got, ok := CalibratePool(CalibrateInput{Current: cur, PoolUsed: 1_000_000, AccountPct: 10}); ok || got != cur {
		t.Errorf("已收敛不该再写，got %d %v", got, ok)
	}
}
