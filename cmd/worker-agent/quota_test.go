package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"ccw/internal/project"
	"ccw/internal/quota"
)

// 假查库：项目与账号池上限。
type fakeQuotaLookup struct {
	p            project.Project
	pool5, pool7 int64
	projErr      error
	poolErr      error
	tierBP       int
	hasTier      bool
	win          quota.Windows
}

func (f fakeQuotaLookup) AccountWindows(_ context.Context, _ string, _ time.Time) (quota.Windows, error) {
	return f.win, nil
}

func (f fakeQuotaLookup) ProjectTierShare(context.Context, string) (int, bool, error) {
	return f.tierBP, f.hasTier, nil
}

func (f fakeQuotaLookup) GetProjectByID(context.Context, string) (project.Project, error) {
	return f.p, f.projErr
}

func (f fakeQuotaLookup) AccountPoolLimits(context.Context, string) (int64, int64, error) {
	return f.pool5, f.pool7, f.poolErr
}

// 假用量读取：项目窗口用量与账号池用量。
type fakeUsage struct{ window, pool int64 }

func (f fakeUsage) WindowUsed(context.Context, string, time.Time) (int64, error) {
	return f.window, nil
}
func (f fakeUsage) PoolUsed(context.Context, string, time.Time) (int64, error) { return f.pool, nil }

func testProject() project.Project {
	return project.Project{ID: "pid", AccountID: "aid", Slug: "alpha",
		FiveHourLimit: 1000, SevenDayLimit: 10000}
}

func TestSyncModeRWWhenUnderQuota(t *testing.T) {
	q := fakeQuotaLookup{p: testProject(), pool5: 100000, pool7: 1000000}
	svc := quota.Service{Reader: fakeUsage{window: 10, pool: 10}}
	if got := syncModeFor(context.Background(), q, svc, "pid", quota.Margins{}, time.Now(), func(string, ...any) {}); got != "rw" {
		t.Errorf("未超额应为rw，得到%q", got)
	}
}

// 验收21/27：项目超过自己的5小时限额后降级为cleanup。
func TestSyncModeCleanupWhenProjectOverQuota(t *testing.T) {
	q := fakeQuotaLookup{p: testProject(), pool5: 100000, pool7: 1000000}
	svc := quota.Service{Reader: fakeUsage{window: 1000, pool: 1000}} // 恰好达到5h限额
	if got := syncModeFor(context.Background(), q, svc, "pid", quota.Margins{}, time.Now(), func(string, ...any) {}); got != "cleanup" {
		t.Errorf("超额应为cleanup，得到%q", got)
	}
}

// 设计A30：项目自己没超，但账号池被打爆时同样降级。
// 002迁移之前池上限写死1<<62，这条永远不可能成立。
func TestSyncModeCleanupWhenAccountPoolExhausted(t *testing.T) {
	q := fakeQuotaLookup{p: testProject(), pool5: 5000, pool7: 50000}
	// 项目用量10远低于自己的1000限额，但池用量5000已经等于池上限。
	svc := quota.Service{Reader: fakeUsage{window: 10, pool: 5000}}
	d, _, err := checkProject(context.Background(), q, svc, "pid", quota.Margins{}, time.Now())
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !d.Over || d.Reason != "pool_exhausted" {
		t.Fatalf("池耗尽应判超额且原因为pool_exhausted，得到 Over=%v Reason=%q", d.Over, d.Reason)
	}
	if got := syncModeFor(context.Background(), q, svc, "pid", quota.Margins{}, time.Now(), func(string, ...any) {}); got != "cleanup" {
		t.Errorf("池耗尽应为cleanup，得到%q", got)
	}
}

// 额度状态未知时按"可能已超额"处理：查询失败必须降级，不能默认放行。
func TestSyncModeFailsClosed(t *testing.T) {
	for name, q := range map[string]fakeQuotaLookup{
		"项目查询失败":  {p: testProject(), projErr: errors.New("db down")},
		"池上限查询失败": {p: testProject(), poolErr: errors.New("db down")},
	} {
		svc := quota.Service{Reader: fakeUsage{}}
		logged := false
		got := syncModeFor(context.Background(), q, svc, "pid", quota.Margins{}, time.Now(),
			func(string, ...any) { logged = true })
		if got != "cleanup" {
			t.Errorf("%s：应降级为cleanup，得到%q", name, got)
		}
		if !logged {
			t.Errorf("%s：降级必须留下日志，否则闸门失灵时无人知晓", name)
		}
	}
}

// 池上限必须真的取自accounts表——写死极大值等于池闸门不存在。
// 组装走quota.Assemble（与control-api共用），这条同时锁住"两个二进制口径一致"。
func TestAssembleUsesAccountPoolLimits(t *testing.T) {
	q := fakeQuotaLookup{p: testProject(), pool5: 4242, pool7: 424242}
	p := testProject()
	lim, err := quota.Assemble(context.Background(), q, p.AccountID,
		p.FiveHourLimit, p.SevenDayLimit, quota.Margins{Reserve: 7, SafetyMargin: 9})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if lim.PoolFiveHour != 4242 || lim.PoolSevenDay != 424242 {
		t.Errorf("池上限未取自accounts表：%+v", lim)
	}
	if lim.FiveHour != 1000 || lim.SevenDay != 10000 {
		t.Errorf("项目限额不对：%+v", lim)
	}
	if lim.Reserve != 7 || lim.SafetyMargin != 9 {
		t.Errorf("余量未透传：%+v", lim)
	}
}

// 挂了档位的项目，限额由「档位比例 × 账号池上限」推导，绝对限额被覆盖。
//
// 这是档位真正生效的那一处：改了后台的百分比，闸门下一轮（30秒内）就按新值判。
func TestCheckProjectAppliesTier(t *testing.T) {
	q := fakeQuotaLookup{
		p:       project.Project{ID: "pid", AccountID: "acct", FiveHourLimit: 1_000_000, SevenDayLimit: 10_000_000},
		pool5:   9_600_000,
		pool7:   60_000_000,
		tierBP:  3300, // 7x
		hasTier: true,
	}
	svc := quota.Service{Reader: fakeUsage{}}
	_, lim, err := checkProject(context.Background(), q, svc, "pid", quota.Margins{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if lim.FiveHour != 3_168_000 {
		t.Errorf("7x(33%%) × 9.6M 应为 3,168,000，got %d", lim.FiveHour)
	}
	// 没挂档位的项目行为不变——这次迁移之前建的项目不该突然换一套限额
	q.hasTier = false
	_, lim, err = checkProject(context.Background(), q, svc, "pid", quota.Margins{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if lim.FiveHour != 1_000_000 {
		t.Errorf("无档位应沿用绝对限额，got %d", lim.FiveHour)
	}
}
