// Command ccwadmin是管理员CLI（节点侧，Console经SSH调用，设计§11.1）。
//
// 子命令（全部支持--json，供Console机器可读）：
//
//	init-project   建项目并签发首张CDK（幂等；强制3项目/15GiB上限，设计§7.6）
//	list-projects  列项目及配额
//	usage          列各项目的真实用量（token）与内部额度单位
//	tiers          查看/修改额度档位，把项目挂到档位
//	issue-cdk      为已有项目签发新CDK（输出含public_id）
//	rotate-cdk     轮换CDK：--grace 24h（默认）或--revoke-now（设计§11.1.1）
//	disable-cdk    按public-id禁用单张CDK
//	list-cdks      列项目全部CDK的状态（不含明文，明文不可再取）
//	status         节点状态：schema版本、磁盘水位、每项目用量新鲜度
//	render-compose 渲染compose.yaml（不需要数据库，渲染计划§3.3）
//
// 数据库连接取自CCW_DATABASE_URL（config.Load，缺失即硬失败）。
package main

import (
	"context"
	"fmt"
	"os"

	"ccw/internal/config"
	"ccw/internal/store"
)

// buildVersion经-ldflags "-X main.buildVersion=..."注入；status --json输出它，
// Console巡检据此更新nodes.stack_version（设计§11.5）。
var buildVersion = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ccwadmin init-project --slug <slug> [--disk-gib 1..15] [--five-hour n] [--seven-day n] [--json]
  ccwadmin init-project <slug> [disk_gib] [five_hour_units] [seven_day_units]   （旧式位置参数，仍兼容）
  ccwadmin list-projects [--json]
  ccwadmin usage [--json]
  ccwadmin tiers [--json] [--set 7x --percent 33] [--assign <slug> --tier 7x]
  ccwadmin issue-cdk --slug <slug> [--json]
  ccwadmin rotate-cdk --slug <slug> [--grace 24h | --revoke-now] [--json]
  ccwadmin disable-cdk --public-id <id>
  ccwadmin list-cdks --slug <slug> [--json]
  ccwadmin status [--json]
  ccwadmin render-compose --projects a,b[,c] [--out path] [--claude-image ref] [--disk-gib n] [--check]`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	if cmd == "render-compose" {
		// 渲染不读数据库，必须在config.Load之前分支（渲染计划§3.3）；
		// --check需要的数据库连接由回调惰性建立。
		os.Exit(runRenderCompose(args, os.Stdout, os.Stderr, dbSlugsFromEnv))
	}

	run, ok := map[string]func([]string, *store.Store) int{
		// 只有init-project跑迁移：它是bootstrap路径；其余命令是只读巡检或
		// 精确写入，遇到未初始化的库应当报错而不是悄悄建表。
		"init-project":  func(a []string, st *store.Store) int { return runInitProject(a, os.Stdout, os.Stderr, st) },
		"list-projects": func(a []string, st *store.Store) int { return runListProjects(a, os.Stdout, os.Stderr, st) },
		"issue-cdk":     func(a []string, st *store.Store) int { return runIssueCDK(a, os.Stdout, os.Stderr, st) },
		"rotate-cdk":    func(a []string, st *store.Store) int { return runRotateCDK(a, os.Stdout, os.Stderr, st) },
		"disable-cdk":   func(a []string, st *store.Store) int { return runDisableCDK(a, os.Stdout, os.Stderr, st) },
		"list-cdks":     func(a []string, st *store.Store) int { return runListCDKs(a, os.Stdout, os.Stderr, st) },
		"status":        func(a []string, st *store.Store) int { return runStatus(a, os.Stdout, os.Stderr, st, buildVersion) },
		"usage":         func(a []string, st *store.Store) int { return runUsage(a, os.Stdout, os.Stderr, st) },
		"tiers":         func(a []string, st *store.Store) int { return runTiers(a, os.Stdout, os.Stderr, st) },
	}[cmd]
	if !ok {
		usage()
		os.Exit(2)
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccwadmin:", err)
		os.Exit(1)
	}
	defer st.Pool.Close()
	if cmd == "init-project" {
		if err := st.Migrate(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "ccwadmin: migrate:", err)
			os.Exit(1)
		}
	}
	os.Exit(run(os.Args[2:], st))
}
