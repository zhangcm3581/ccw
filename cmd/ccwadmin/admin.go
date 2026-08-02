package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"ccw/internal/deploy"
	"ccw/internal/project"
	"ccw/internal/store"
)

// adminStore是全部管理子命令依赖的store能力面；单测注入假实现，
// 真实SQL行为由internal/store的PG集成测试覆盖。
type adminStore interface {
	EnsureAccount(ctx context.Context, name, pool string) (string, error)
	CountProjects(ctx context.Context) (int, error)
	ProjectBySlug(ctx context.Context, slug string) (project.Project, error)
	CreateProject(ctx context.Context, accountID, slug, containerName string, diskLimit, fiveHour, sevenDay int64) (string, error)
	CreateCDK(ctx context.Context, projectID string) (plain, publicID string, err error)
	ListProjects(ctx context.Context) ([]project.Project, error)
	ListCDKs(ctx context.Context, projectID string) ([]store.CDKInfo, error)
	ExpireOtherProjectCDKs(ctx context.Context, projectID, exceptPublicID string, graceSeconds int64) (int64, error)
	DisableOtherProjectCDKs(ctx context.Context, projectID, exceptPublicID string) (int64, error)
	DisableCDKByPublicID(ctx context.Context, publicID string) error
	StatusProjects(ctx context.Context) ([]store.StatusProject, error)
	SchemaMigrations(ctx context.Context) ([]string, error)
}

// 编译期断言：*store.Store满足全部子命令的需要——签名漂移在编译期暴露。
var _ adminStore = (*store.Store)(nil)

const (
	defaultDiskGiB = 15 // 设计§7.6：单项目磁盘配额默认值与上限均为15 GiB
	maxDiskGiB     = 15
	// **2026-08-02 按真机数据放大。**旧值（100万/1000万）在真实使用下，
	// Claude 账号 5 小时窗口才到 11% 就把项目判超了——尺子和刻度都不对。
	// 系数已按定价比例重标（见 deploy/.env.example），单位量级随之变化，
	// 默认限额一并放大到"一个项目大致可以独占账号一个窗口"的量级。
	//
	// 这仍是**起点不是结论**：真正的限额要按 /admin/usage 上的实际分布来定，
	// 尤其是多人共用一台机器时，每人应当只拿到其中一份。
	defaultFiveHour = 200_000_000
	defaultSevenDay = 1_000_000_000
)

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// ---- init-project ----

// runInitProject建项目并签发首张CDK（幂等：已存在则返回现有信息，设计§11.1）。
//
// 上限强制点：单节点最多3个项目、磁盘配额1–15 GiB（设计§7.6）。校验放在这里
// 而非只放Console——把约束只放在调用方等于没有约束（SSH直连节点仍可绕过）。
// slug校验与render-compose共用同一条规则（deploy.ValidateSlug，渲染计划§6），
// 否则会出现「数据库里有、compose渲染不出来」的死CDK。
func runInitProject(args []string, stdout, stderr io.Writer, st adminStore) int {
	var slug string
	diskGiB := int64(defaultDiskGiB)
	fiveHour := int64(defaultFiveHour)
	sevenDay := int64(defaultSevenDay)
	jsonOut := false

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		// 兼容旧的位置参数形式：init-project <slug> [disk_gib] [five_hour] [seven_day]
		slug = args[0]
		var perr error
		if diskGiB, perr = posInt(args, 1, diskGiB); perr != nil {
			fmt.Fprintln(stderr, "init-project:", perr)
			return 2
		}
		if fiveHour, perr = posInt(args, 2, fiveHour); perr != nil {
			fmt.Fprintln(stderr, "init-project:", perr)
			return 2
		}
		if sevenDay, perr = posInt(args, 3, sevenDay); perr != nil {
			fmt.Fprintln(stderr, "init-project:", perr)
			return 2
		}
	} else {
		fs := flag.NewFlagSet("init-project", flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.StringVar(&slug, "slug", "", "项目slug（必填）")
		fs.Int64Var(&diskGiB, "disk-gib", defaultDiskGiB, "磁盘配额GiB（1–15，设计§7.6）")
		fs.Int64Var(&fiveHour, "five-hour", defaultFiveHour, "5小时窗口限额（内部额度单位）")
		fs.Int64Var(&sevenDay, "seven-day", defaultSevenDay, "7天窗口限额（内部额度单位）")
		fs.BoolVar(&jsonOut, "json", false, "机器可读输出")
		if err := fs.Parse(args); err != nil {
			return 2
		}
	}

	if err := deploy.ValidateSlug(slug); err != nil {
		fmt.Fprintln(stderr, "init-project:", err)
		return 2
	}
	if diskGiB < 1 || diskGiB > maxDiskGiB {
		fmt.Fprintf(stderr, "init-project: disk_gib %d 超出允许范围1–%d（单项目磁盘配额上限，产品规则，console-fleet-design §7.6）\n", diskGiB, maxDiskGiB)
		return 2
	}
	if fiveHour <= 0 || sevenDay <= 0 {
		fmt.Fprintln(stderr, "init-project: five_hour与seven_day必须为正数")
		return 2
	}

	ctx := context.Background()

	// 幂等：已存在则返回现有信息而非报错（流水线重跑必须能安全再执行）。
	if p, err := st.ProjectBySlug(ctx, slug); err == nil {
		if p.DiskLimit != diskGiB<<30 || p.FiveHourLimit != fiveHour || p.SevenDayLimit != sevenDay {
			fmt.Fprintf(stderr, "init-project: 项目%s已存在，请求的配额与现有值不同——未做任何修改（改配额请直接UPDATE数据库或等Console档位功能）\n", slug)
		}
		out := initProjectOut{
			Slug: p.Slug, ProjectID: p.ID, Container: p.ContainerName, Created: false,
			DiskGiB: p.DiskLimit >> 30, FiveHour: p.FiveHourLimit, SevenDay: p.SevenDayLimit,
		}
		if jsonOut {
			writeJSON(stdout, out)
		} else {
			fmt.Fprintf(stdout, "project exists: slug=%s id=%s container=%s\n", out.Slug, out.ProjectID, out.Container)
			fmt.Fprintf(stdout, "disk=%dGiB five_hour=%d seven_day=%d\n", out.DiskGiB, out.FiveHour, out.SevenDay)
			fmt.Fprintln(stdout, "（未签发新CDK；需要新CDK请用 issue-cdk --slug "+out.Slug+"）")
		}
		return 0
	} else if !errors.Is(err, store.ErrNotFound) {
		fmt.Fprintln(stderr, "init-project:", err)
		return 1
	}

	// 产品硬上限：单节点最多3个项目容器（设计§7.6，验收A34）。
	n, err := st.CountProjects(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "init-project:", err)
		return 1
	}
	if n >= deploy.MaxProjectsPerNode {
		fmt.Fprintf(stderr, "init-project: 节点已有%d个项目，达到单节点上限%d（产品规则，console-fleet-design §7.6，不可绕过）\n", n, deploy.MaxProjectsPerNode)
		return 2
	}

	accountID, err := st.EnsureAccount(ctx, "default", "default-pool")
	if err != nil {
		fmt.Fprintln(stderr, "init-project: account:", err)
		return 1
	}
	projectID, err := st.CreateProject(ctx, accountID, slug, "ccw-"+slug, diskGiB<<30, fiveHour, sevenDay)
	if err != nil {
		fmt.Fprintln(stderr, "init-project: project:", err)
		return 1
	}
	cdk, publicID, err := st.CreateCDK(ctx, projectID)
	if err != nil {
		fmt.Fprintln(stderr, "init-project: cdk:", err)
		return 1
	}

	out := initProjectOut{
		Slug: slug, ProjectID: projectID, Container: "ccw-" + slug, Created: true,
		DiskGiB: diskGiB, FiveHour: fiveHour, SevenDay: sevenDay,
		PublicID: publicID, CDK: cdk, // 明文只出现在stdout这一次（一次性交付面）
	}
	if jsonOut {
		writeJSON(stdout, out)
	} else {
		fmt.Fprintf(stdout, "project created: slug=%s id=%s container=ccw-%s\n", slug, projectID, slug)
		fmt.Fprintf(stdout, "disk=%dGiB five_hour=%d seven_day=%d\n", diskGiB, fiveHour, sevenDay)
		fmt.Fprintln(stdout, "CDK (显示一次，请立即保存):")
		fmt.Fprintln(stdout, cdk)
	}
	return 0
}

type initProjectOut struct {
	Slug      string `json:"slug"`
	ProjectID string `json:"project_id"`
	Container string `json:"container"`
	Created   bool   `json:"created"`
	DiskGiB   int64  `json:"disk_gib"`
	FiveHour  int64  `json:"five_hour"`
	SevenDay  int64  `json:"seven_day"`
	PublicID  string `json:"public_id,omitempty"`
	CDK       string `json:"cdk,omitempty"`
}

func posInt(args []string, i int, def int64) (int64, error) {
	if len(args) <= i {
		return def, nil
	}
	n, err := strconv.ParseInt(args[i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("参数%d不是整数: %q", i+1, args[i])
	}
	return n, nil
}

// ---- issue-cdk ----

// runIssueCDK为已有项目签发新CDK。输出含public_id（Console靠它入库cdk_issues，
// 设计§11.1）；明文只在stdout出现一次。
func runIssueCDK(args []string, stdout, stderr io.Writer, st adminStore) int {
	fs := flag.NewFlagSet("issue-cdk", flag.ContinueOnError)
	fs.SetOutput(stderr)
	slug := fs.String("slug", "", "项目slug（必填）")
	jsonOut := fs.Bool("json", false, "机器可读输出")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *slug == "" {
		fmt.Fprintln(stderr, "issue-cdk: --slug 必填")
		return 2
	}
	ctx := context.Background()
	p, err := st.ProjectBySlug(ctx, *slug)
	if err != nil {
		fmt.Fprintln(stderr, "issue-cdk:", err)
		return 1
	}
	cdk, publicID, err := st.CreateCDK(ctx, p.ID)
	if err != nil {
		fmt.Fprintln(stderr, "issue-cdk:", err)
		return 1
	}
	if *jsonOut {
		writeJSON(stdout, map[string]string{"slug": p.Slug, "project_id": p.ID, "public_id": publicID, "cdk": cdk})
	} else {
		fmt.Fprintf(stdout, "public_id: %s\n", publicID)
		fmt.Fprintln(stdout, "CDK (显示一次，请立即保存):")
		fmt.Fprintln(stdout, cdk)
	}
	return 0
}

// ---- rotate-cdk ----

// unifiedFail输出统一错误（console-fleet-design §11.1.1）：不泄露
// 「项目不存在/CDK不存在/已禁用」的区别，也不回显目标。基础设施错误
// （连接失败等）不属于此列，如实报告——否则数据库故障会被误读成"目标不存在"。
func unifiedFail(stderr io.Writer, cmd string) int {
	fmt.Fprintf(stderr, "%s: 操作失败\n", cmd)
	return 1
}

// runRotateCDK轮换项目CDK（设计§11.1.1）：签发新CDK，旧CDK按模式失效。
//
//	--grace 24h（默认）：旧CDK设expires_at=now()+宽限，期内新旧都能用；
//	                     到期自动失效，无需定时任务（ResolveCDK每次比对expires_at）。
//	--revoke-now       ：凭据泄露应急，旧CDK当场disabled_at=now()。
//
// 先签新、后失效旧：中途失败时项目仍有可用CDK，重跑安全（多出的新CDK可事后禁用）。
func runRotateCDK(args []string, stdout, stderr io.Writer, st adminStore) int {
	fs := flag.NewFlagSet("rotate-cdk", flag.ContinueOnError)
	fs.SetOutput(stderr)
	slug := fs.String("slug", "", "项目slug（必填）")
	grace := fs.Duration("grace", 24*time.Hour, "旧CDK的宽限时长")
	revokeNow := fs.Bool("revoke-now", false, "立即撤销旧CDK（凭据泄露应急）")
	jsonOut := fs.Bool("json", false, "机器可读输出")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *slug == "" {
		fmt.Fprintln(stderr, "rotate-cdk: --slug 必填")
		return 2
	}
	graceSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "grace" {
			graceSet = true
		}
	})
	if *revokeNow && graceSet {
		fmt.Fprintln(stderr, "rotate-cdk: --grace 与 --revoke-now 互斥")
		return 2
	}
	if !*revokeNow && *grace <= 0 {
		fmt.Fprintln(stderr, "rotate-cdk: --grace 必须为正时长")
		return 2
	}

	ctx := context.Background()
	p, err := st.ProjectBySlug(ctx, *slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return unifiedFail(stderr, "rotate-cdk")
		}
		fmt.Fprintln(stderr, "rotate-cdk:", err)
		return 1
	}
	cdk, publicID, err := st.CreateCDK(ctx, p.ID)
	if err != nil {
		fmt.Fprintln(stderr, "rotate-cdk:", err)
		return 1
	}
	mode := "grace"
	var affected int64
	if *revokeNow {
		mode = "revoke-now"
		affected, err = st.DisableOtherProjectCDKs(ctx, p.ID, publicID)
	} else {
		affected, err = st.ExpireOtherProjectCDKs(ctx, p.ID, publicID, int64(grace.Seconds()))
	}
	if err != nil {
		fmt.Fprintln(stderr, "rotate-cdk:", err)
		return 1
	}

	if *jsonOut {
		writeJSON(stdout, map[string]any{
			"slug": p.Slug, "public_id": publicID, "cdk": cdk,
			"mode": mode, "grace_seconds": int64(grace.Seconds()), "previous_affected": affected,
		})
	} else {
		if *revokeNow {
			fmt.Fprintf(stdout, "已轮换：旧CDK（%d张）已立即失效\n", affected)
		} else {
			fmt.Fprintf(stdout, "已轮换：旧CDK（%d张）将在%s后失效，期间新旧都可用\n", affected, grace)
		}
		fmt.Fprintf(stdout, "public_id: %s\n", publicID)
		fmt.Fprintln(stdout, "新CDK (显示一次，请立即保存):")
		fmt.Fprintln(stdout, cdk)
	}
	return 0
}

// ---- disable-cdk ----

func runDisableCDK(args []string, stdout, stderr io.Writer, st adminStore) int {
	fs := flag.NewFlagSet("disable-cdk", flag.ContinueOnError)
	fs.SetOutput(stderr)
	publicID := fs.String("public-id", "", "要禁用的CDK的public-id（必填）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *publicID == "" {
		fmt.Fprintln(stderr, "disable-cdk: --public-id 必填")
		return 2
	}
	if err := st.DisableCDKByPublicID(context.Background(), *publicID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 统一错误：不区分"不存在/已禁用"，也不回显public-id（§11.1.1同款规则）。
			return unifiedFail(stderr, "disable-cdk")
		}
		fmt.Fprintln(stderr, "disable-cdk:", err)
		return 1
	}
	fmt.Fprintln(stdout, "disabled")
	return 0
}

// ---- list-cdks ----

type cdkRow struct {
	PublicID   string     `json:"public_id"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	DisabledAt *time.Time `json:"disabled_at"`
	State      string     `json:"state"` // active|expired|disabled
}

func runListCDKs(args []string, stdout, stderr io.Writer, st adminStore) int {
	fs := flag.NewFlagSet("list-cdks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	slug := fs.String("slug", "", "项目slug（必填）")
	jsonOut := fs.Bool("json", false, "机器可读输出")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *slug == "" {
		fmt.Fprintln(stderr, "list-cdks: --slug 必填")
		return 2
	}
	ctx := context.Background()
	p, err := st.ProjectBySlug(ctx, *slug)
	if err != nil {
		fmt.Fprintln(stderr, "list-cdks:", err)
		return 1
	}
	infos, err := st.ListCDKs(ctx, p.ID)
	if err != nil {
		fmt.Fprintln(stderr, "list-cdks:", err)
		return 1
	}
	rows := make([]cdkRow, 0, len(infos))
	for _, i := range infos {
		state := "active"
		switch {
		case i.Disabled:
			state = "disabled"
		case i.Expired:
			state = "expired"
		}
		rows = append(rows, cdkRow{PublicID: i.PublicID, CreatedAt: i.CreatedAt,
			ExpiresAt: i.ExpiresAt, DisabledAt: i.DisabledAt, State: state})
	}
	if *jsonOut {
		writeJSON(stdout, rows)
	} else {
		for _, r := range rows {
			fmt.Fprintf(stdout, "%s  %-8s  created=%s", r.PublicID, r.State, r.CreatedAt.UTC().Format(time.RFC3339))
			if r.ExpiresAt != nil {
				fmt.Fprintf(stdout, "  expires=%s", r.ExpiresAt.UTC().Format(time.RFC3339))
			}
			fmt.Fprintln(stdout)
		}
	}
	return 0
}

// usageStore是 usage 子命令需要的能力（只读）。
type usageStore interface {
	ProjectUsageReport(ctx context.Context) ([]store.ProjectUsage, error)
}

// ---- list-projects ----

type projectRow struct {
	Slug      string `json:"slug"`
	ProjectID string `json:"project_id"`
	Container string `json:"container"`
	DiskGiB   int64  `json:"disk_gib"`
	FiveHour  int64  `json:"five_hour"`
	SevenDay  int64  `json:"seven_day"`
}

func runListProjects(args []string, stdout, stderr io.Writer, st adminStore) int {
	fs := flag.NewFlagSet("list-projects", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "机器可读输出")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ps, err := st.ListProjects(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, "list-projects:", err)
		return 1
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Slug < ps[j].Slug })
	rows := make([]projectRow, 0, len(ps))
	for _, p := range ps {
		rows = append(rows, projectRow{Slug: p.Slug, ProjectID: p.ID, Container: p.ContainerName,
			DiskGiB: p.DiskLimit >> 30, FiveHour: p.FiveHourLimit, SevenDay: p.SevenDayLimit})
	}
	if *jsonOut {
		writeJSON(stdout, rows)
	} else {
		for _, r := range rows {
			fmt.Fprintf(stdout, "%s  id=%s  disk=%dGiB  5h=%d  7d=%d\n", r.Slug, r.ProjectID, r.DiskGiB, r.FiveHour, r.SevenDay)
		}
	}
	return 0
}

// ---- status ----

type statusOut struct {
	OK               bool            `json:"ok"`
	Version          string          `json:"version"`
	SchemaMigrations []string        `json:"schema_migrations"`
	Disk             *diskOut        `json:"disk,omitempty"`
	Projects         []statusProjRow `json:"projects"`
}

type diskOut struct {
	Path       string `json:"path"`
	Note       string `json:"note"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

type statusProjRow struct {
	Slug          string     `json:"slug"`
	Container     string     `json:"container"`
	DiskUsed      int64      `json:"disk_used_bytes"`
	DiskLimit     int64      `json:"disk_limit_bytes"`
	FiveHourLimit int64      `json:"five_hour_limit"`
	SevenDayLimit int64      `json:"seven_day_limit"`
	ActiveCDKs    int        `json:"active_cdks"`
	LastUsageAt   *time.Time `json:"last_usage_event_at"` // null＝从未有事件；项目在用却停在很久前＝采集停摆
}

// runStatus输出节点状态（Console巡检§11.5每5分钟调用，取version与项目面）。
// 容器状态/证书到期不在此列——Console经SSH直接跑docker compose ps更可靠（设计§4.2）；
// 这里报告的是只有数据库知道的事实，外加所在文件系统的磁盘水位（N4告警的数据源）。
func runStatus(args []string, stdout, stderr io.Writer, st adminStore, version string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "机器可读输出")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	migs, err := st.SchemaMigrations(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "status:", err)
		return 1
	}
	ps, err := st.StatusProjects(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "status:", err)
		return 1
	}
	out := statusOut{OK: true, Version: version, SchemaMigrations: migs}
	// 磁盘水位best-effort：statfs失败（如不支持的平台）不影响其余输出。
	if total, free, derr := diskStat("/"); derr == nil {
		out.Disk = &diskOut{Path: "/", Note: "容器文件系统（宿主机侧≈Docker data-root所在盘）",
			TotalBytes: total, FreeBytes: free}
	}
	for _, p := range ps {
		out.Projects = append(out.Projects, statusProjRow{
			Slug: p.Slug, Container: p.ContainerName,
			DiskUsed: p.DiskUsed, DiskLimit: p.DiskLimit,
			FiveHourLimit: p.FiveHourLimit, SevenDayLimit: p.SevenDayLimit,
			ActiveCDKs: p.ActiveCDKs, LastUsageAt: p.LastUsageEventAt,
		})
	}
	if *jsonOut {
		writeJSON(stdout, out)
		return 0
	}
	fmt.Fprintf(stdout, "version=%s migrations=%d\n", version, len(migs))
	if out.Disk != nil {
		fmt.Fprintf(stdout, "disk %s: free %.1f/%.1f GiB\n", out.Disk.Path,
			float64(out.Disk.FreeBytes)/(1<<30), float64(out.Disk.TotalBytes)/(1<<30))
	}
	for _, p := range out.Projects {
		last := "从未有用量事件"
		if p.LastUsageAt != nil {
			last = p.LastUsageAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(stdout, "%s  disk=%d/%d  active_cdks=%d  last_usage=%s\n",
			p.Slug, p.DiskUsed, p.DiskLimit, p.ActiveCDKs, last)
	}
	return 0
}

// ---- usage ----

// runUsage输出各项目的真实用量。
//
// **token 数是真实的**，直接来自 Claude 写的会话 JSONL；weighted_units 是本仓库
// 自己算的内部额度单位（估算口径，闸门用它）。两者必须分开呈现，
// 否则很容易把估算当成账号的实际消耗。
func runUsage(args []string, stdout, stderr io.Writer, st usageStore) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "机器可读输出")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rows, err := st.ProjectUsageReport(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, "usage:", err)
		return 1
	}
	if *jsonOut {
		writeJSON(stdout, rows)
		return 0
	}
	for _, r := range rows {
		last := "从未采集到"
		if r.LastEventAt != nil {
			last = r.LastEventAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		fmt.Fprintf(stdout, "%s  最近采集=%s\n", r.Slug, last)
		fmt.Fprintf(stdout, "  5h  in=%d out=%d cache_r=%d cache_w=%d  units=%d/%d\n",
			r.FiveHour.Input, r.FiveHour.Output, r.FiveHour.CacheRead, r.FiveHour.CacheWrite,
			r.FiveHour.Weighted, r.FiveHourLim)
		fmt.Fprintf(stdout, "  7d  in=%d out=%d cache_r=%d cache_w=%d  units=%d/%d\n",
			r.SevenDay.Input, r.SevenDay.Output, r.SevenDay.CacheRead, r.SevenDay.CacheWrite,
			r.SevenDay.Weighted, r.SevenDayLim)
		for _, m := range r.ByModel {
			fmt.Fprintf(stdout, "    %-28s in=%d out=%d\n", m.Model, m.Input, m.Output)
		}
	}
	return 0
}
