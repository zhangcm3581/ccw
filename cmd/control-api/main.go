package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
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

	// 池额度目前只从环境变量读；默认极大＝暂不启用池保护，
	// 由运维注入真实上游额度后才生效。改为从accounts表读取是待办，见docs/STATUS.md。
	limitsFor := func(p project.Project) quota.Limits {
		return quota.Limits{
			FiveHour: p.FiveHourLimit, SevenDay: p.SevenDayLimit,
			PoolFiveHour: envInt("CCW_POOL_5H", 1<<62), PoolSevenDay: envInt("CCW_POOL_7D", 1<<62),
			Reserve: envInt("CCW_POOL_RESERVE", 0), SafetyMargin: envInt("CCW_POOL_SAFETY_MARGIN", 0),
		}
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

func envInt(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
