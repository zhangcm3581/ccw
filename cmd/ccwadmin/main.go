// Command ccwadmin是管理员CLI（节点侧，Console经SSH调用，设计§11.1）。
//
// 子命令：
//
//	ccwadmin init-project <slug> [disk_gib] [five_hour_units] [seven_day_units]
//	ccwadmin render-compose --projects a,b[,c] [--out path] [--check]
//
// init-project：建立（或复用）default账号，创建项目，容器名固定为 ccw-<slug>，
// 签发一张CDK并打印其明文（仅此一次显示）。数据库连接取自CCW_DATABASE_URL。
// render-compose：渲染compose.yaml，不需要数据库（渲染计划§3.3），--check除外。
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"ccw/internal/config"
	"ccw/internal/store"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ccwadmin init-project <slug> [disk_gib] [five_hour_units] [seven_day_units]
  ccwadmin render-compose --projects a,b[,c] [--out path] [--claude-image ref] [--disk-gib n] [--check]`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "render-compose":
		// 渲染不读数据库，必须在config.Load之前分支（渲染计划§3.3）；
		// --check需要的数据库连接由回调惰性建立。
		os.Exit(runRenderCompose(os.Args[2:], os.Stdout, os.Stderr, dbSlugsFromEnv))
	case "init-project":
		initProject()
	default:
		usage()
		os.Exit(2)
	}
}

func initProject() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(2)
	}
	slug := os.Args[2]
	diskGiB := argInt(3, 20)
	fiveHour := argInt(4, 1_000_000)
	sevenDay := argInt(5, 10_000_000)

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
	if err := st.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ccwadmin: migrate:", err)
		os.Exit(1)
	}

	accountID, err := st.EnsureAccount(ctx, "default", "default-pool")
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccwadmin: account:", err)
		os.Exit(1)
	}
	projectID, err := st.CreateProject(ctx, accountID, slug, "ccw-"+slug,
		diskGiB<<30, fiveHour, sevenDay)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccwadmin: project:", err)
		os.Exit(1)
	}
	cdk, err := st.CreateCDK(ctx, projectID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccwadmin: cdk:", err)
		os.Exit(1)
	}

	fmt.Printf("project created: slug=%s id=%s container=ccw-%s\n", slug, projectID, slug)
	fmt.Printf("disk=%dGiB five_hour=%d seven_day=%d\n", diskGiB, fiveHour, sevenDay)
	fmt.Println("CDK (显示一次，请立即保存):")
	fmt.Println(cdk)
}

func argInt(i int, def int64) int64 {
	if len(os.Args) > i {
		if n, err := strconv.ParseInt(os.Args[i], 10, 64); err == nil {
			return n
		}
	}
	return def
}
