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
	limitsFor := func(ctx context.Context, p project.Project) (quota.Limits, error) {
		return quota.Assemble(ctx, st, p.AccountID, p.FiveHourLimit, p.SevenDayLimit, cfg.PoolMargins)
	}

	agentBase := os.Getenv("CCW_AGENT_WS_BASE")
	if agentBase == "" {
		agentBase = "wss://localhost/ws"
	}

	srv := control.New(st, st.GetProjectByID, cfg.TokenSigningKey,
		quota.Service{Reader: st}, store.PGIndex{Pool: st.Pool}, limitsFor, agentBase)

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
