package quota

import "math"

// 账号池上限的自动校准（2026-08-02）。
//
// 档位是「百分比 × 账号池上限」，而池上限本来是个拍脑袋的数——那样"给你 33%"
// 到底是不是真的三分之一，谁也说不准。
//
// 唯一的真值来自 Claude 自己：状态行每 10 秒收到一次账号级 rate_limits，
// 会把真实百分比写成快照。有了它就能反推：
//
//	池上限 ≈ 我们记的池内累计单位 ÷ (真实账号百分比 / 100)
//
// 例：账号报 11%，我们记了 1,052,098 单位 → 池上限 ≈ 9,564,527 单位。

// CalibrateInput是一次校准所需的全部输入。
type CalibrateInput struct {
	// Current是当前记录的池上限。<=0 表示尚未标定过。
	Current int64
	// PoolUsed是同一窗口内我们记的池内累计（内部单位）。
	PoolUsed int64
	// AccountPct是 Claude 报的真实已用百分比（0–100）。
	AccountPct float64
}

// 校准的三条防呆，每条都对应一种会算出离谱值的真实情形：
const (
	// minPct：百分比太小的时候分母噪声被放大——账号报 1% 而我们记了 100 单位，
	// 反推出来的池上限会差出十倍。窗口刚开始时必然会经过这一段。
	minPct = 5.0
	// minUsed：我们这边几乎没记到量时，比值同样不可信。
	minUsed = 1000
	// maxStep：单次最多把上限朝估计值挪这么多。一次异常的快照
	// （比如账号在别处被大量使用）不该把上限一把改掉。
	maxStep = 0.2
)

// CalibratePool返回新的池上限，以及是否发生了调整。
//
// **只朝估计值缓慢移动**，不直接采用：单次快照可能受别处用量、时钟、
// 采集延迟影响，而池上限是所有档位的基准，抖动会让每个项目的限额跟着抖。
func CalibratePool(in CalibrateInput) (int64, bool) {
	if in.AccountPct < minPct || in.PoolUsed < minUsed || math.IsNaN(in.AccountPct) {
		return in.Current, false
	}
	est := float64(in.PoolUsed) / (in.AccountPct / 100)
	if math.IsInf(est, 0) || est <= 0 {
		return in.Current, false
	}
	if in.Current <= 0 {
		// 从未标定过：直接采用估计值，没有"缓慢移动"的起点可言。
		return int64(math.Round(est)), true
	}
	cur := float64(in.Current)
	next := cur + (est-cur)*maxStep
	out := int64(math.Round(next))
	if out == in.Current {
		return in.Current, false
	}
	return out, true
}
