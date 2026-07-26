package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"ccw/internal/config"
	"ccw/internal/deploy"
	"ccw/internal/store"
)

// runRenderCompose执行render-compose子命令（渲染计划§3）。
//
// 渲染是纯函数、不需要数据库；只有--check需要查库比对，因此dbSlugs是惰性回调，
// 非--check路径绝不触碰它——这也是本子命令必须在config.Load之前分支的原因（§3.3）。
func runRenderCompose(args []string, stdout, stderr io.Writer, dbSlugs func() ([]string, error)) int {
	fs := flag.NewFlagSet("render-compose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projects := fs.String("projects", "", "逗号分隔的项目slug列表（必填）")
	out := fs.String("out", "", "输出路径；缺省写stdout")
	claudeImage := fs.String("claude-image", "", "项目容器镜像（默认ccw-claude:latest）")
	diskGiB := fs.Int("disk-gib", 15, "单项目磁盘配额GiB；仅校验上限（配额是逻辑配额、由init-project入库），不影响渲染输出")
	check := fs.Bool("check", false, "不输出，校验入参并与数据库projects表比对；退出码非0表示不一致")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *projects == "" {
		fmt.Fprintln(stderr, "render-compose: --projects 必填（逗号分隔的slug列表）")
		return 2
	}
	// 上限强制点之一（设计§7.6）；init-project独立强制同一条规则。
	if *diskGiB < 1 || *diskGiB > 15 {
		fmt.Fprintf(stderr, "render-compose: --disk-gib %d 超出允许范围1–15（单项目磁盘配额上限15 GiB，产品规则，console-fleet-design §7.6）\n", *diskGiB)
		return 2
	}

	slugs := strings.Split(*projects, ",")
	rendered, err := deploy.RenderCompose(deploy.ComposeInput{Projects: slugs, ClaudeImage: *claudeImage})
	if err != nil {
		fmt.Fprintln(stderr, "render-compose:", err)
		return 2
	}

	if *check {
		db, err := dbSlugs()
		if err != nil {
			// 数据库不可达≠无漂移：必须失败，不能沉默通过。
			fmt.Fprintln(stderr, "render-compose --check: 读数据库失败:", err)
			return 2
		}
		missing, extra := deploy.CheckDrift(slugs, db)
		if len(missing) == 0 && len(extra) == 0 {
			fmt.Fprintf(stdout, "render-compose --check: 一致（%d个项目）\n", len(slugs))
			return 0
		}
		for _, s := range missing {
			fmt.Fprintf(stdout, "漂移：数据库有项目 %s，但不在本次渲染列表中——其CDK能认证但连不上容器\n", s)
		}
		for _, s := range extra {
			fmt.Fprintf(stdout, "漂移：渲染列表含 %s，但数据库无此项目——容器会启动但无人能认证\n", s)
		}
		return 1
	}

	if *out != "" {
		if err := os.WriteFile(*out, []byte(rendered), 0o644); err != nil {
			fmt.Fprintln(stderr, "render-compose:", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stdout, rendered)
	return 0
}

// dbSlugsFromEnv是--check的生产实现：连库并列出projects表的全部slug。
// 不跑迁移——--check是只读巡检，遇到未初始化的库应当报错而不是改库。
func dbSlugsFromEnv() ([]string, error) {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	defer st.Pool.Close()
	ps, err := st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	slugs := make([]string, 0, len(ps))
	for _, p := range ps {
		slugs = append(slugs, p.Slug)
	}
	return slugs, nil
}
