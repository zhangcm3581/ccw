// Command ccwadmin是管理员CLI：建项目并签发一次性CDK。
//
// 用法：
//
//	ccwadmin init-project <slug> [disk_gib] [five_hour_units] [seven_day_units]
//
// 建立（或复用）default账号，创建项目，容器名固定为 ccw-<slug>，
// 签发一张CDK并打印其明文（仅此一次显示）。数据库连接取自CCW_DATABASE_URL。
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"ccw/internal/config"
	"ccw/internal/store"
)

func main() {
	if len(os.Args) < 3 || os.Args[1] != "init-project" {
		fmt.Fprintln(os.Stderr, "usage: ccwadmin init-project <slug> [disk_gib] [five_hour_units] [seven_day_units]")
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
