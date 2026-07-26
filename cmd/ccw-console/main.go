// Command ccw-console是Console控制平面进程（console-fleet-design §2.4）：
// 公开站点（落地页/下载分发/CDK查询）+ 管理后台（认证已就绪，纳管流水线待实施）。
//
// 子命令：
//
//	ccw-console [serve]            默认：跑迁移并启动HTTP服务（只监听回环，公网入口只有Caddy）
//	ccw-console register-release   把构建产物登记进releases表（下载页从表渲染，不扫目录）
//	ccw-console create-admin       创建管理员账号并生成两步验证密钥
//
// 配置见internal/config的LoadConsole。管理后台需要CCW_SECRET_KEY与
// CCW_ADMIN_ALLOWLIST；二者缺失时**不注册任何/admin路由**——没有认证与网络层
// 限制就不上管理页面。
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
	"ccw/internal/secretbox"
)

// buildVersion经-ldflags "-X main.buildVersion=..."注入。
var buildVersion = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ccw-console [serve]
  ccw-console register-release --version <v> [--notes s] [--publish] [--dir path]
  ccw-console create-admin --username <name>`)
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "register-release":
		cfg, st := mustInit()
		defer st.Pool.Close()
		os.Exit(runRegisterRelease(args, os.Stdout, os.Stderr, st, cfg.DistDir))
	case "create-admin":
		cfg, st := mustInit()
		defer st.Pool.Close()
		box, err := mustBox(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ccw-console:", err)
			os.Exit(1)
		}
		os.Exit(runCreateAdmin(args, os.Stdout, os.Stderr, st, box, readPasswordTTY))
	case "serve":
		serve()
	default:
		usage()
		os.Exit(2)
	}
}

func serve() {
	cfg, st := mustInit()
	defer st.Pool.Close()

	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ccw-console: "+format+"\n", a...)
	}
	srv := console.New(st, cfg.DistDir, logf)

	if cfg.AdminEnabled() {
		box, err := mustBox(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ccw-console:", err)
			os.Exit(1)
		}
		nets, err := console.ParseAllowlist(cfg.AdminAllowlist)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ccw-console:", err)
			os.Exit(1)
		}
		srv.Auth = &console.Auth{
			Store: st, Box: box, Allowlist: nets,
			Secure: !cfg.AdminInsecureCookie,
		}
		logf("管理后台已启用（白名单%d条）", len(nets))
	} else {
		// 说清楚为什么没开，否则运维会以为后台坏了。
		logf("管理后台未启用：需要同时设置CCW_SECRET_KEY与CCW_ADMIN_ALLOWLIST；/admin路由未注册")
	}

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Printf("ccw-console %s listening on %s\n", buildVersion, cfg.ListenAddr)
	if err := httpSrv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "ccw-console:", err)
		os.Exit(1)
	}
}

func mustBox(cfg config.ConsoleConfig) (*secretbox.Box, error) {
	if len(cfg.SecretKey) == 0 {
		return nil, fmt.Errorf("CCW_SECRET_KEY未设置（生成：openssl rand -hex 32）")
	}
	return secretbox.New(cfg.SecretKey)
}

// mustInit加载配置、连接Console库并跑迁移；任一步失败即非零退出。
// Console是自己数据库的唯一属主，各子命令都可安全执行迁移
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
