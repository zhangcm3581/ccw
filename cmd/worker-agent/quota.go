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

// limitsFor组装项目级与账号级的双层限额。
//
// 池上限来自accounts表（002迁移新增）。此前这里写死1<<62，池闸门从未生效——
// 多个项目共用一个上游账号时，那等于没有任何机制阻止"各自都没超限、加起来把账号打爆"。
//
// Reserve与SafetyMargin暂留0：安全余量只有在限额本身是真实值之后才有意义，
// 而当前处于"先记账、后校准"的第一阶段（限额刻意设得很大、不真正拦人）。
// 校准阶段定下真实限额时，应一并给出余量取值。
func limitsFor(ctx context.Context, q quotaLookup, p project.Project) (quota.Limits, error) {
	pool5h, pool7d, err := q.AccountPoolLimits(ctx, p.AccountID)
	if err != nil {
		return quota.Limits{}, err
	}
	return quota.Limits{
		FiveHour: p.FiveHourLimit, SevenDay: p.SevenDayLimit,
		PoolFiveHour: pool5h, PoolSevenDay: pool7d,
	}, nil
}

// checkProject查一个项目当前是否超额。
func checkProject(ctx context.Context, q quotaLookup, svc quota.Service, projectID string, now time.Time) (quota.Decision, error) {
	p, err := q.GetProjectByID(ctx, projectID)
	if err != nil {
		return quota.Decision{}, err
	}
	lim, err := limitsFor(ctx, q, p)
	if err != nil {
		return quota.Decision{}, err
	}
	return svc.Check(ctx, projectID, p.AccountID, lim, now)
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
	now time.Time, logf func(string, ...any)) string {
	d, err := checkProject(ctx, q, svc, projectID, now)
	if err != nil {
		logf("额度查询失败，同步降级为cleanup（项目%s）：%v", projectID, err)
		return "cleanup"
	}
	if d.Over {
		return "cleanup"
	}
	return "rw"
}
