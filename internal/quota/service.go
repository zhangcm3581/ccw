package quota

import (
	"context"
	"math"
	"time"
)

type UsageReader interface {
	WindowUsed(ctx context.Context, projectID string, since time.Time) (int64, error)
	PoolUsed(ctx context.Context, accountID string, since time.Time) (int64, error)
}

type Limits struct {
	FiveHour, SevenDay         int64
	PoolFiveHour, PoolSevenDay int64
	Reserve, SafetyMargin      int64
}

type Decision struct {
	Over                       bool
	Reason                     string
	FiveHourUsed, SevenDayUsed int64
}

// PoolLimitReader提供账号级池上限；生产实现是store查accounts表。
type PoolLimitReader interface {
	AccountPoolLimits(ctx context.Context, accountID string) (fiveHour, sevenDay int64, err error)
}

// Margins是池保护的安全余量，来自部署配置（CCW_POOL_RESERVE / CCW_POOL_SAFETY_MARGIN）。
type Margins struct{ Reserve, SafetyMargin int64 }

// ApplyTier把档位比例折算成项目限额。
//
// **档位优先于绝对限额**：挂了档位的项目，限额 = 比例 × 账号池上限，
// 而池上限由真实账号用量自动校准（calibrate.go）——这样"给你 33%"
// 才真的对应账号的三分之一，而不是一个拍脑袋的绝对数。
//
// shareBP 是万分之一（1000 = 10%）。用整数而不是浮点：限额要参与
// ">=" 比较，浮点会带来"到底算不算超"的边界抖动。
func ApplyTier(l Limits, shareBP int, hasTier bool) Limits {
	if !hasTier || shareBP <= 0 {
		return l
	}
	five, ok5 := tierLimit(l.PoolFiveHour, shareBP)
	seven, ok7 := tierLimit(l.PoolSevenDay, shareBP)
	// **任一窗口算不出来就整个不套用**：只换一半会得到一组自相矛盾的限额
	// （5h 按档位、7d 按绝对值），排查时极难看懂。
	if !ok5 || !ok7 {
		return l
	}
	l.FiveHour, l.SevenDay = five, seven
	return l
}

// tierLimit算 pool × shareBP / 10000，算不出来时返回 ok=false。
//
// **两个必须挡住的输入**，它们都会让项目限额变成 0，也就是立刻永久受限：
//
//  1. pool <= 0：没有可分的池。
//  2. pool 大到乘法会溢出——**这正是全新安装的默认状态**：迁移 002 把
//     pool_*_limit 的默认值设成 MaxInt64 当"无限制"哨兵，而
//     MaxInt64 × 3300 回绕之后 /10000 得 0。挂个档位就把人锁死，
//     而且发生在校准还没跑起来的时候。
//
// 这两种情况一律沿用绝对限额：宁可暂时不按档位分，也不能把人锁在门外。
//
// 溢出那条是**纵深防御**：实测采样下来，回绕的结果都落在 <=0，会被下面
// 那条 `v <= 0` 兜住。留着它是为了让正确性不依赖"回绕恰好落在哪一侧"
// ——那不是一条能指望的性质。
func tierLimit(pool int64, shareBP int) (int64, bool) {
	if pool <= 0 || pool > math.MaxInt64/int64(shareBP) {
		return 0, false
	}
	v := pool * int64(shareBP) / 10000
	if v <= 0 {
		return 0, false
	}
	return v, true
}

// Assemble组装项目级 + 账号级的双层限额。
//
// **control-api与worker-agent必须共用本函数。**此前两者各自组装：worker读accounts表、
// control-api读环境变量，结果是同一个项目在门户里显示"未超额"、在worker那里却已被
// 降级为cleanup。限额只应有一个真相源（accounts表 + 同一份余量配置），
// 两处各写一份迟早漂移。
func Assemble(ctx context.Context, r PoolLimitReader, accountID string,
	fiveHour, sevenDay int64, m Margins) (Limits, error) {
	pool5h, pool7d, err := r.AccountPoolLimits(ctx, accountID)
	if err != nil {
		return Limits{}, err
	}
	return Limits{
		FiveHour: fiveHour, SevenDay: sevenDay,
		PoolFiveHour: pool5h, PoolSevenDay: pool7d,
		Reserve: m.Reserve, SafetyMargin: m.SafetyMargin,
	}, nil
}

type Service struct{ Reader UsageReader }

func (s Service) Check(ctx context.Context, projectID, accountID string, l Limits, now time.Time) (Decision, error) {
	var d Decision
	var err error
	if d.FiveHourUsed, err = s.Reader.WindowUsed(ctx, projectID, now.Add(-5*time.Hour)); err != nil {
		return d, err
	}
	if d.SevenDayUsed, err = s.Reader.WindowUsed(ctx, projectID, now.Add(-7*24*time.Hour)); err != nil {
		return d, err
	}
	switch {
	case d.FiveHourUsed >= l.FiveHour:
		d.Over, d.Reason = true, "five_hour_limit"
	case d.SevenDayUsed >= l.SevenDay:
		d.Over, d.Reason = true, "seven_day_limit"
	default:
		// 池保护同时看5小时与7天窗口（审查§9.3：只看5小时不足以保护周额度）
		pool5h, err := s.Reader.PoolUsed(ctx, accountID, now.Add(-5*time.Hour))
		if err != nil {
			return d, err
		}
		pool7d, err := s.Reader.PoolUsed(ctx, accountID, now.Add(-7*24*time.Hour))
		if err != nil {
			return d, err
		}
		if l.PoolFiveHour-pool5h <= l.Reserve+l.SafetyMargin ||
			l.PoolSevenDay-pool7d <= l.Reserve+l.SafetyMargin {
			d.Over, d.Reason = true, "pool_exhausted"
		}
	}
	return d, nil
}

// AccountUsageProvider：spec预留接口，未来接获授权的上游用量来源做校准；第一版无实现。
type AccountUsageProvider interface {
	Snapshot(ctx context.Context, pool string) (UsageSnapshot, error)
}

type UsageSnapshot struct {
	FiveHourUsedPct float64
	SevenDayUsedPct float64
	FiveHourResetAt time.Time
	SevenDayResetAt time.Time
}
