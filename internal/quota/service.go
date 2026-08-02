package quota

import (
	"context"
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
	l.FiveHour = l.PoolFiveHour * int64(shareBP) / 10000
	l.SevenDay = l.PoolSevenDay * int64(shareBP) / 10000
	return l
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
