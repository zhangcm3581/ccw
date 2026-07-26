// Command ccw-console是Console控制平面进程（console-fleet-design §2.4）：
// 公开站点（落地页/下载分发/CDK查询）+ 未来的管理后台与SSH执行引擎。
//
// 子命令：
//
//	ccw-console [serve]            默认：跑迁移并启动HTTP服务（只监听回环，公网入口只有Caddy）
//	ccw-console register-release   把构建产物登记进releases表（下载页从表渲染，不扫目录）
//
// 配置见internal/config的LoadConsole：CCW_CONSOLE_DATABASE_URL、CCW_DIST_DIR必填，
// CCW_CONSOLE_LISTEN_ADDR默认127.0.0.1:8090。Console有独立数据库，与节点库无关。
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"ccw/internal/config"
	"ccw/internal/console"
	"ccw/internal/consolestore"
)

// buildVersion经-ldflags "-X main.buildVersion=..."注入。
var buildVersion = "dev"

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "register-release" {
		cfg, st := mustInit()
		defer st.Pool.Close()
		os.Exit(runRegisterRelease(args[1:], os.Stdout, os.Stderr, st, cfg.DistDir))
	}
	if len(args) > 0 && args[0] != "serve" {
		fmt.Fprintln(os.Stderr, `usage:
  ccw-console [serve]
  ccw-console register-release --version <v> [--notes s] [--publish] [--dir path]`)
		os.Exit(2)
	}

	cfg, st := mustInit()
	defer st.Pool.Close()

	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ccw-console: "+format+"\n", a...)
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           console.New(st, cfg.DistDir, logf).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Printf("ccw-console %s listening on %s\n", buildVersion, cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "ccw-console:", err)
		os.Exit(1)
	}
}

// mustInit加载配置、连接Console库并跑迁移；任一步失败即非零退出。
// Console是自己数据库的唯一属主，serve与register都可安全执行迁移
// （schema_migrations保证每个文件只执行一次）。
func mustInit() (config.ConsoleConfig, *consolestore.Store) {
	cfg, err := config.LoadConsole(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	st, err := consolestore.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccw-console:", err)
		os.Exit(1)
	}
	if err := st.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ccw-console: migrate:", err)
		os.Exit(1)
	}
	return cfg, st
}
