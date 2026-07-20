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
