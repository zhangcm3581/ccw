package main

import (
	"context"
	"time"

	"ccw/internal/project"
	"ccw/internal/quota"
)

// quotaLookup抽象worker需要的两次查库，便于单测注入假实现。
type quotaLookup interface {
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
	lim, err := quota.Assemble(ctx, q, p.AccountID, p.FiveHourLimit, p.SevenDayLimit, margins)
	if err != nil {
		return quota.Decision{}, quota.Limits{}, err
	}
	// **把限额一并返回**：调用方（状态栏）需要它才能算百分比，
	// 而它已经在这里组装好了——再查一次库既多余，也可能与本次判定用的值不一致。
	d, cerr := svc.Check(ctx, projectID, p.AccountID, lim, now)
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
