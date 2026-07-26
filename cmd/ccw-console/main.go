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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"ccw/internal/config"
	"ccw/internal/console"
	"ccw/internal/consolestore"
	"ccw/internal/deploy"
	"ccw/internal/dns"
	"ccw/internal/provision"
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
		// 机队管理：SSH执行层 + 流水线 + 日志广播。与Auth同批启用——
		// 它的每个入口都在requireAdmin之后。
		// 节点源码包必须存在才启用机队管理：节点靠它构建镜像，
		// 缺了它纳管会一路跑到compose-up才失败，白白改了目标机器的状态。
		srcTar, srcErr := loadNodeSource(cfg.NodeSrcPath)
		if srcErr != nil {
			logf("机队管理未启用：%v", srcErr)
		} else {
			hub := console.NewLogHub(cfg.LogDir)
			srv.Fleet = &console.Fleet{
				Store: st, Logs: hub,
				Orchestrator: &provision.Orchestrator{
					Store: st, Box: box, DNS: &dns.Manual{},
					Dial:               provision.DefaultDialer(20 * time.Second),
					Log:                hub.Append,
					Finish:             hub.Finish,
					Artifacts:          nodeArtifacts,
					SourceTar:          func() ([]byte, error) { return srcTar, nil },
					RepoRoot:           "/srv/ccw",
					ComposeProjectName: "ccw",
				},
			}
			logf("机队管理已启用（源码包%d KiB，日志目录%s）", len(srcTar)/1024, cfg.LogDir)
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

// nodeArtifacts渲染要覆盖写入节点的编排文件（路径相对RepoRoot）。
//
// 只有compose.yaml：它要按本节点的项目列表渲染。Caddyfile与三个Dockerfile
// 来自源码包，不在这里重复推送——两处各存一份迟早漂移。
//
// 渲染器与`ccwadmin render-compose`是同一个，因此Console部署出来的编排与
// 管理员手动渲染的字节级一致（模板契约I1–I8同样成立）。
func nodeArtifacts(slugs []string) (map[string]string, error) {
	composeYAML, err := deploy.RenderCompose(deploy.ComposeInput{Projects: slugs})
	if err != nil {
		return nil, err
	}
	return map[string]string{"deploy/compose.yaml": composeYAML}, nil
}

// loadNodeSource读取节点源码包并做最小完整性校验——
// 缺关键文件时立刻说清楚，而不是等纳管跑到构建那一步才炸在别人的机器上。
func loadNodeSource(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读不到节点源码包%s（生成：scripts/build-node-src.sh，或用deploy/Dockerfile.console构建的镜像）: %w", path, err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("节点源码包不是合法gzip: %w", err)
	}
	defer gz.Close()
	need := map[string]bool{
		"./go.mod": false, "./deploy/Dockerfile.control-api": false,
		"./deploy/Dockerfile.worker-agent": false, "./deploy/Dockerfile.claude": false,
		"./deploy/Caddyfile": false,
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("节点源码包无法读取: %w", err)
		}
		if _, ok := need[h.Name]; ok {
			need[h.Name] = true
		}
	}
	for name, found := range need {
		if !found {
			return nil, fmt.Errorf("节点源码包缺%s——节点将无法构建镜像", name)
		}
	}
	return b, nil
}
