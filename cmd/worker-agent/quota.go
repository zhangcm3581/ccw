package main

import (
	"context"
	"time"

	"ccw/internal/project"
	"ccw/internal/quota"
)

// quotaLookup抽象worker需要的两次查库，便于单测注入假实现。
type quotaLookup interface {
	// AccountWindows返回对齐 Claude 的窗口起点；拿不到时零值＝退回滚动窗口。
	AccountWindows(ctx context.Context, accountID string, now time.Time) (quota.Windows, error)
	// ProjectTierShare返回项目所属档位的比例（万分之一）；未挂档位时 ok=false。
	ProjectTierShare(ctx context.Context, projectID string) (int, bool, error)
	GetProjectByID(ctx context.Context, id string) (project.Project, error)
	AccountPoolLimits(ctx context.Context, accountID string) (int64, int64, error)
}

// checkProject查一个项目当前是否超额。
//
// 限额组装走quota.Assemble——与control-api同一个函数、同一个数据源（accounts表）。
// 两处各写一份的后果是门户显示"未超额"而worker已经降级为cleanup，见Assemble的注释。
func checkProject(ctx context.Context, q quotaLookup, svc quota.Service, projectID string,
	margins quota.Margins, now time.Time) (quota.Decision, quota.Limits, error) {
	p, err := q.GetProjectByID(ctx, projectID)
	if err != nil {
		return quota.Decision{}, quota.Limits{}, err
	}
	// 与 control-api 用同一个实现，见 quota.AssembleProject 的说明。
	lim, err := quota.AssembleProject(ctx, q, projectID, p.AccountID, p.FiveHourLimit, p.SevenDayLimit, margins)
	if err != nil {
		return quota.Decision{}, quota.Limits{}, err
	}
	// **把限额一并返回**：调用方（状态栏）需要它才能算百分比，
	// 而它已经在这里组装好了——再查一次库既多余，也可能与本次判定用的值不一致。
	// **窗口对齐 Claude**：项目额度跟着账号一起在 resets_at 归零，
	// 而不是靠滚动窗口慢慢滑出去。拿不到边界时 Windows 是零值，
	// Check 会退回滚动窗口——那是安全的降级。
	win, werr := q.AccountWindows(ctx, p.AccountID, now)
	if werr != nil {
		win = quota.Windows{}
	}
	d, cerr := svc.Check(ctx, projectID, p.AccountID, lim, now, win)
	return d, lim, cerr
}

// syncModeFor决定同步会话的模式：超额降级为cleanup（只许下载、删除、缩小）。
//
// 两个刻意的选择：
//
//  1. 每次接受连接都实时查一遍，不信任令牌里的模式。连接令牌有2分钟有效期且允许
//     重连，只看令牌就会让刚刚超额的项目在窗口内继续上传（审查：worker每次接受
//     连接时必须实时复查额度）。
//
//  2. 查询失败时返回cleanup而不是rw。额度状态未知时按"可能已超额"处理——
//     宁可让客户暂时传不上去，也不要在闸门失灵时敞开写入。
func syncModeFor(ctx context.Context, q quotaLookup, svc quota.Service, projectID string,
	margins quota.Margins, now time.Time, logf func(string, ...any)) string {
	d, _, err := checkProject(ctx, q, svc, projectID, margins, now)
	if err != nil {
		logf("额度查询失败，同步降级为cleanup（项目%s）：%v", projectID, err)
		return "cleanup"
	}
	if d.Over {
		return "cleanup"
	}
	return "rw"
}
