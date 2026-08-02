package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"ccw/internal/config"
	"ccw/internal/control"
	"ccw/internal/project"
	"ccw/internal/quota"
	"ccw/internal/store"
)

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()

	st, err := store.New(ctx, cfg.DatabaseURL) // 内部Ping，失败即非零退出
	if err != nil {
		fmt.Fprintln(os.Stderr, "control-api:", err)
		os.Exit(1)
	}
	if err := st.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "control-api: migrate:", err)
		os.Exit(1)
	}

	// 池上限从accounts表读，与worker-agent同一个真相源（quota.Assemble）。
	// 此前这里读CCW_POOL_5H/7D环境变量，而worker读数据库，结果是门户显示"未超额"、
	// worker那边却已经降级为cleanup。那两个环境变量已废弃。
	// **与 worker-agent 用同一个实现**（quota.AssembleProject）：两处各算各的
	// 一定会漂移——上面那段注释记的是池上限那次，2026-08-03 又在档位上重演了
	// 一遍（这里用绝对限额判超额、worker 用档位折算后的限额）。
	limitsFor := func(ctx context.Context, p project.Project) (quota.Limits, error) {
		return quota.AssembleProject(ctx, st, p.ID, p.AccountID, p.FiveHourLimit, p.SevenDayLimit, cfg.PoolMargins)
	}

	agentBase := os.Getenv("CCW_AGENT_WS_BASE")
	if agentBase == "" {
		agentBase = "wss://localhost/ws"
	}

	srv := control.New(st, st.GetProjectByID, cfg.TokenSigningKey,
		quota.Service{Reader: st}, store.PGIndex{Pool: st.Pool}, limitsFor, agentBase)
	// 窗口也要与 worker-agent 对齐，否则一边按 Claude 的重置点算、
	// 一边按滚动窗口算，同样会给出互相矛盾的判定。
	srv.WindowsFor = func(ctx context.Context, accountID string) quota.Windows {
		w, err := st.AccountWindows(ctx, accountID, time.Now())
		if err != nil {
			return quota.Windows{}
		}
		return w
	}

	// 只监听回环/内网（审查§3.1）；公网由反向代理的443统一入口。四类超时齐全。
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Println("control-api listening on", cfg.ListenAddr)
	if err := httpSrv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "control-api:", err)
		os.Exit(1)
	}
}
