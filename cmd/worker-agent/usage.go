package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ccw/internal/project"
	"ccw/internal/usage"
)

// projectLister抽象store.ListProjects，便于单测注入假实现。
type projectLister interface {
	ListProjects(ctx context.Context) ([]project.Project, error)
}

// usageCollectors持有每个项目的Collector并逐轮复用。
//
// 复用而不是每轮新建：Collector的BadLines是累计指标，新建会让它归零，
// 坏行就永远不会攒到能被察觉的量。游标本身在PG里，不依赖这个缓存。
type usageCollectors struct {
	root    string
	weights usage.Weights
	sink    usage.Sink
	offsets usage.OffsetStore

	byProject map[string]*usage.Collector
	// missingDirLogged记录已经报过"目录不存在"的项目，避免每30秒刷一次同样的日志。
	missingDirLogged map[string]bool
}

func newUsageCollectors(root string, w usage.Weights, sink usage.Sink, offsets usage.OffsetStore) *usageCollectors {
	return &usageCollectors{
		root: root, weights: w, sink: sink, offsets: offsets,
		byProject:        map[string]*usage.Collector{},
		missingDirLogged: map[string]bool{},
	}
}

// dirFor：项目的JSONL目录＝<root>/<slug>，与compose里worker-agent的只读挂载对应。
func (u *usageCollectors) dirFor(p project.Project) string {
	return filepath.Join(u.root, p.Slug)
}

func (u *usageCollectors) collectorFor(p project.Project) *usage.Collector {
	if c, ok := u.byProject[p.ID]; ok {
		return c
	}
	c := &usage.Collector{
		Dir: u.dirFor(p), ProjectID: p.ID,
		Sink: u.sink, Offsets: u.offsets, Weights: u.weights,
	}
	u.byProject[p.ID] = c
	return c
}

// runOnce对全部项目跑一轮采集，返回本轮的错误列表（不中断其余项目）。
//
// 两个刻意的设计：
//
//  1. 遍历的是传入的全部项目，不是"有活跃终端连接的项目"。tmux会话在客户端断开后
//     继续运行，Claude照常消耗额度；只采活跃项目会丢掉无人值守期间的用量，
//     而闸门恰恰要在那时生效。
//
//  2. 目录不存在时显式报错。Collector.Scan对不存在的目录直接返回nil（单文件失败
//     不中断整体采集），若不在这里检查，漏挂卷的后果就是"采集器在跑、日志正常、
//     usage_events永远为空"——与接线前的现象完全一样，极难排查。
func (u *usageCollectors) runOnce(ctx context.Context, projects []project.Project) []error {
	var errs []error
	for _, p := range projects {
		dir := u.dirFor(p)
		if _, err := os.Stat(dir); err != nil {
			if !u.missingDirLogged[p.ID] {
				u.missingDirLogged[p.ID] = true
				errs = append(errs, fmt.Errorf(
					"用量采集：项目%s的JSONL目录不存在（%s）——检查compose是否把%s-claude-projects挂进了worker-agent；"+
						"在此之前该项目的用量不会被记录", p.Slug, dir, p.Slug))
			}
			continue
		}
		u.missingDirLogged[p.ID] = false
		if err := u.scanOne(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("用量采集：项目%s扫描失败：%w", p.Slug, err))
		}
	}
	return errs
}

// scanOne隔离单个项目的panic：一个项目的异常不得中断其余项目的采集。
func (u *usageCollectors) scanOne(ctx context.Context, p project.Project) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return u.collectorFor(p).Scan(ctx)
}

// guard捕获一轮循环里的panic，让长驻goroutine活下去。
//
// **Go的panic不分goroutine：任何一个未recover的panic都会终止整个进程。**
// 因此"采集goroutine的panic不影响执行goroutine，反之亦然"这条不变量，
// 必须两个循环各自recover才成立——只在其中一侧加recover是做了一半。
// 少了它，一次数据库驱动的意外panic会把采集、闸门、HTTP服务一起带走。
func guard(name string, logf func(string, ...any), fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logf("%s：本轮panic已捕获，循环继续：%v", name, r)
		}
	}()
	fn()
}

// BadLines汇总各项目的坏行计数，供日志暴露；不静默丢弃解析失败。
func (u *usageCollectors) BadLines() int64 {
	var n int64
	for _, c := range u.byProject {
		n += c.BadLines
	}
	return n
}

// runUsageLoop每interval跑一轮采集，直到ctx结束。
//
// 与额度执行循环分开成两个goroutine：采集遍历全部项目、执行只遍历有连接的项目，
// 失败域也不同——采集失败（例如数据库抖动）不应该影响"超额就关终端"这条链路。
func runUsageLoop(ctx context.Context, lister projectLister, u *usageCollectors,
	interval time.Duration, logf func(string, ...any)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastBad int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			guard("用量采集", logf, func() {
				projects, err := lister.ListProjects(ctx)
				if err != nil {
					logf("用量采集：列项目失败：%v", err)
					return
				}
				for _, e := range u.runOnce(ctx, projects) {
					logf("%v", e)
				}
				// 坏行只在增量出现时报一次，避免每轮刷屏；
				// 注意绝不打印行内容——JSONL里是完整的会话正文。
				if bad := u.BadLines(); bad > lastBad {
					logf("用量采集：累计跳过%d个无法解析的行（不含内容）", bad)
					lastBad = bad
				}
			})
		}
	}
}
