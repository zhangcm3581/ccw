package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"ccw/internal/project"
	"ccw/internal/store"
)

// fakeAdminStore：内存假实现，记录调用，供子命令层单测（真实SQL行为由internal/store
// 的PG集成测试覆盖，这里只测命令层的校验、编排与输出边界）。
type fakeAdminStore struct {
	projects map[string]project.Project // by slug
	cdks     map[string][]store.CDKInfo // by projectID

	createdProjects []string
	createdCDKs     []string // projectID
	expireCalls     []expireCall
	disableCalls    []disableCall
	disabledPublic  []string

	nextCDKPlain, nextCDKPublic string
	failWith                    error
}

type expireCall struct {
	projectID, except string
	graceSeconds      int64
}
type disableCall struct{ projectID, except string }

func newFake() *fakeAdminStore {
	return &fakeAdminStore{
		projects: map[string]project.Project{}, cdks: map[string][]store.CDKInfo{},
		nextCDKPlain: "ccw_pub1234.secretsecret", nextCDKPublic: "pub1234",
	}
}

func (f *fakeAdminStore) addProject(slug string) project.Project {
	p := project.Project{ID: "id-" + slug, AccountID: "acc", Slug: slug,
		ContainerName: "ccw-" + slug, DiskLimit: 15 << 30, FiveHourLimit: 1, SevenDayLimit: 2}
	f.projects[slug] = p
	return p
}

func (f *fakeAdminStore) EnsureAccount(ctx context.Context, name, pool string) (string, error) {
	return "acc", f.failWith
}
func (f *fakeAdminStore) CountProjects(ctx context.Context) (int, error) {
	return len(f.projects), f.failWith
}
func (f *fakeAdminStore) ProjectBySlug(ctx context.Context, slug string) (project.Project, error) {
	if f.failWith != nil {
		return project.Project{}, f.failWith
	}
	p, ok := f.projects[slug]
	if !ok {
		return project.Project{}, store.ErrNotFound
	}
	return p, nil
}
func (f *fakeAdminStore) CreateProject(ctx context.Context, accountID, slug, containerName string, disk, five, seven int64) (string, error) {
	f.createdProjects = append(f.createdProjects, slug)
	f.projects[slug] = project.Project{ID: "id-" + slug, AccountID: accountID, Slug: slug,
		ContainerName: containerName, DiskLimit: disk, FiveHourLimit: five, SevenDayLimit: seven}
	return "id-" + slug, f.failWith
}
func (f *fakeAdminStore) CreateCDK(ctx context.Context, projectID string) (string, string, error) {
	f.createdCDKs = append(f.createdCDKs, projectID)
	return f.nextCDKPlain, f.nextCDKPublic, f.failWith
}
func (f *fakeAdminStore) ListProjects(ctx context.Context) ([]project.Project, error) {
	var out []project.Project
	for _, p := range f.projects {
		out = append(out, p)
	}
	return out, f.failWith
}
func (f *fakeAdminStore) ListCDKs(ctx context.Context, projectID string) ([]store.CDKInfo, error) {
	return f.cdks[projectID], f.failWith
}
func (f *fakeAdminStore) ExpireOtherProjectCDKs(ctx context.Context, projectID, except string, graceSeconds int64) (int64, error) {
	f.expireCalls = append(f.expireCalls, expireCall{projectID, except, graceSeconds})
	return 1, f.failWith
}
func (f *fakeAdminStore) DisableOtherProjectCDKs(ctx context.Context, projectID, except string) (int64, error) {
	f.disableCalls = append(f.disableCalls, disableCall{projectID, except})
	return 1, f.failWith
}
func (f *fakeAdminStore) DisableCDKByPublicID(ctx context.Context, publicID string) error {
	if f.failWith != nil {
		return f.failWith
	}
	for _, infos := range f.cdks {
		for _, i := range infos {
			if i.PublicID == publicID && !i.Disabled {
				f.disabledPublic = append(f.disabledPublic, publicID)
				return nil
			}
		}
	}
	return store.ErrNotFound
}
func (f *fakeAdminStore) StatusProjects(ctx context.Context) ([]store.StatusProject, error) {
	var out []store.StatusProject
	for _, p := range f.projects {
		out = append(out, store.StatusProject{Project: p, ActiveCDKs: 1})
	}
	return out, f.failWith
}
func (f *fakeAdminStore) SchemaMigrations(ctx context.Context) ([]string, error) {
	return []string{"001_initial.sql", "002_account_pool_limits.sql", "003_cdk_created_at.sql"}, f.failWith
}

func run2(t *testing.T, fn func([]string, *bytes.Buffer, *bytes.Buffer) int, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := fn(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// ---- init-project ----

func initFn(f *fakeAdminStore) func([]string, *bytes.Buffer, *bytes.Buffer) int {
	return func(a []string, o, e *bytes.Buffer) int { return runInitProject(a, o, e, f) }
}

// A34：第4个项目必须被拒绝，且校验在ccwadmin而非只在Console——
// 绕过渲染器/Console直连节点同样拒绝（设计§7.6、§11.1）。
func TestInitProjectEnforcesThreeProjectCap(t *testing.T) {
	f := newFake()
	f.addProject("p1")
	f.addProject("p2")
	f.addProject("p3")
	code, _, stderr := run2(t, initFn(f), "--slug", "p4")
	if code == 0 {
		t.Fatal("第4个项目应被拒绝（A34）")
	}
	if !strings.Contains(stderr, "3") || !strings.Contains(stderr, "7.6") {
		t.Errorf("错误信息应说明上限与来源（设计§7.6），got: %s", stderr)
	}
	if len(f.createdProjects) != 0 {
		t.Error("拒绝时不得创建项目")
	}
}

// A35：--disk-gib 16被拒绝；不传时默认15。
func TestInitProjectDiskCapAndDefault(t *testing.T) {
	f := newFake()
	code, _, stderr := run2(t, initFn(f), "--slug", "alpha", "--disk-gib", "16")
	if code == 0 {
		t.Fatal("--disk-gib 16应被拒绝（A35）")
	}
	if !strings.Contains(stderr, "7.6") {
		t.Errorf("错误信息应说明来源，got: %s", stderr)
	}

	code, _, _ = run2(t, initFn(f), "--slug", "alpha")
	if code != 0 {
		t.Fatal("默认参数应成功")
	}
	if got := f.projects["alpha"].DiskLimit; got != 15<<30 {
		t.Errorf("默认磁盘配额应为15 GiB，got %d bytes", got)
	}
}

// 幂等（设计§11.1）：已存在的slug返回现有信息、不报错、不签发新CDK。
func TestInitProjectIdempotent(t *testing.T) {
	f := newFake()
	f.addProject("alpha")
	code, out, _ := run2(t, initFn(f), "--slug", "alpha", "--json")
	if code != 0 {
		t.Fatalf("已存在的slug应成功返回，exit=%d", code)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("--json输出应是合法JSON: %v\n%s", err, out)
	}
	if resp["created"] != false {
		t.Errorf("created应为false，got %v", resp["created"])
	}
	if _, has := resp["cdk"]; has {
		t.Error("已存在的项目不得重新签发CDK（要新CDK走issue-cdk）")
	}
	if len(f.createdCDKs) != 0 {
		t.Error("不得调用CreateCDK")
	}
}

func TestInitProjectCreatesWithCDK(t *testing.T) {
	f := newFake()
	code, out, stderr := run2(t, initFn(f), "--slug", "alpha", "--json")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["created"] != true || resp["cdk"] != f.nextCDKPlain || resp["public_id"] != f.nextCDKPublic {
		t.Errorf("新建项目应返回created/cdk/public_id，got %v", resp)
	}
	// CDK明文只允许出现在stdout（一次性交付面）；stderr绝不能有。
	if strings.Contains(stderr, "ccw_") {
		t.Error("CDK明文泄漏到stderr")
	}
}

// 兼容旧的位置参数形式（DEPLOY.md历史用法），但上限规则同样生效。
func TestInitProjectLegacyPositional(t *testing.T) {
	f := newFake()
	code, _, _ := run2(t, initFn(f), "beta", "10", "500", "900")
	if code != 0 {
		t.Fatal("位置参数形式应仍可用")
	}
	p := f.projects["beta"]
	if p.DiskLimit != 10<<30 || p.FiveHourLimit != 500 || p.SevenDayLimit != 900 {
		t.Errorf("位置参数解析错误: %+v", p)
	}
	// 旧文档里的20 GiB现在超上限，必须拒绝——DEPLOY.md已同步改为15。
	code, _, stderr := run2(t, initFn(f), "gamma", "20")
	if code == 0 {
		t.Fatal("位置参数的disk=20也应被拒绝（上限15，A35同款规则）")
	}
	if !strings.Contains(stderr, "7.6") {
		t.Errorf("错误应说明来源，got: %s", stderr)
	}
}

func TestInitProjectValidatesSlug(t *testing.T) {
	f := newFake()
	for _, bad := range []string{"BAD", "a", "postgres", "-x-"} {
		if code, _, _ := run2(t, initFn(f), "--slug", bad); code == 0 {
			t.Errorf("slug %q 应被拒绝（与渲染器同一条规则，渲染计划§6）", bad)
		}
	}
}

// ---- issue-cdk / rotate-cdk / disable-cdk / list-cdks ----

func TestIssueCDK(t *testing.T) {
	f := newFake()
	f.addProject("alpha")
	code, out, stderr := run2(t, func(a []string, o, e *bytes.Buffer) int {
		return runIssueCDK(a, o, e, f)
	}, "--slug", "alpha", "--json")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["public_id"] != f.nextCDKPublic || resp["cdk"] != f.nextCDKPlain {
		t.Errorf("issue-cdk应返回public_id与一次性明文，got %v", resp)
	}
	if strings.Contains(stderr, "ccw_") {
		t.Error("CDK明文泄漏到stderr")
	}
}

func TestRotateCDKGraceDefault(t *testing.T) {
	f := newFake()
	f.addProject("alpha")
	code, out, _ := run2(t, func(a []string, o, e *bytes.Buffer) int {
		return runRotateCDK(a, o, e, f)
	}, "--slug", "alpha", "--json")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if len(f.expireCalls) != 1 || len(f.disableCalls) != 0 {
		t.Fatalf("默认应走宽限路径（Expire），got expire=%d disable=%d", len(f.expireCalls), len(f.disableCalls))
	}
	c := f.expireCalls[0]
	if c.projectID != "id-alpha" || c.except != f.nextCDKPublic {
		t.Errorf("宽限应豁免新签发的CDK：%+v", c)
	}
	if c.graceSeconds != int64((24 * time.Hour).Seconds()) {
		t.Errorf("默认宽限应为24h，got %ds", c.graceSeconds)
	}
	if !strings.Contains(out, f.nextCDKPlain) {
		t.Error("新CDK明文应输出到stdout（仅此一次）")
	}
}

func TestRotateCDKRevokeNow(t *testing.T) {
	f := newFake()
	f.addProject("alpha")
	code, _, _ := run2(t, func(a []string, o, e *bytes.Buffer) int {
		return runRotateCDK(a, o, e, f)
	}, "--slug", "alpha", "--revoke-now")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if len(f.disableCalls) != 1 || len(f.expireCalls) != 0 {
		t.Fatalf("--revoke-now应走立即禁用路径，got expire=%d disable=%d", len(f.expireCalls), len(f.disableCalls))
	}
	if f.disableCalls[0].except != f.nextCDKPublic {
		t.Errorf("立即禁用应豁免新CDK：%+v", f.disableCalls[0])
	}
}

// §11.1.1：轮换失败一律统一错误，不泄露「项目不存在/CDK不存在/已禁用」的区别。
func TestRotateCDKUnifiedErrorOnUnknownSlug(t *testing.T) {
	f := newFake()
	code, out, stderr := run2(t, func(a []string, o, e *bytes.Buffer) int {
		return runRotateCDK(a, o, e, f)
	}, "--slug", "ghost")
	if code == 0 {
		t.Fatal("未知slug的轮换应失败")
	}
	combined := out + stderr
	for _, leak := range []string{"不存在", "not found", "ghost"} {
		if strings.Contains(combined, leak) {
			t.Errorf("统一错误不得泄露失败原因/目标（含%q），got: %s", leak, combined)
		}
	}
}

// 基础设施错误（连接失败等）不属于统一错误的范畴，应如实报告——
// 否则数据库挂了会被误读成"目标不存在"。
func TestRotateCDKInfraErrorIsReported(t *testing.T) {
	f := newFake()
	f.failWith = errors.New("connection refused")
	code, _, stderr := run2(t, func(a []string, o, e *bytes.Buffer) int {
		return runRotateCDK(a, o, e, f)
	}, "--slug", "alpha")
	if code == 0 {
		t.Fatal("基础设施错误应失败")
	}
	if !strings.Contains(stderr, "connection refused") {
		t.Errorf("基础设施错误应如实报告，got: %s", stderr)
	}
}

func TestDisableCDKUnifiedError(t *testing.T) {
	f := newFake()
	code, out, stderr := run2(t, func(a []string, o, e *bytes.Buffer) int {
		return runDisableCDK(a, o, e, f)
	}, "--public-id", "nope")
	if code == 0 {
		t.Fatal("未知public-id应失败")
	}
	if strings.Contains(out+stderr, "不存在") || strings.Contains(out+stderr, "nope") {
		t.Errorf("统一错误不得回显目标或原因，got: %s", out+stderr)
	}
}

func TestListCDKs(t *testing.T) {
	f := newFake()
	p := f.addProject("alpha")
	now := time.Now().UTC()
	f.cdks[p.ID] = []store.CDKInfo{
		{PublicID: "aaaa", CreatedAt: now},
		{PublicID: "bbbb", CreatedAt: now, Disabled: true},
		{PublicID: "cccc", CreatedAt: now, Expired: true},
	}
	code, out, _ := run2(t, func(a []string, o, e *bytes.Buffer) int {
		return runListCDKs(a, o, e, f)
	}, "--slug", "alpha", "--json")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("JSON解析失败: %v\n%s", err, out)
	}
	if len(rows) != 3 {
		t.Fatalf("应有3行，got %d", len(rows))
	}
	states := map[string]string{}
	for _, r := range rows {
		states[r["public_id"].(string)] = r["state"].(string)
	}
	if states["aaaa"] != "active" || states["bbbb"] != "disabled" || states["cccc"] != "expired" {
		t.Errorf("状态映射错误: %v", states)
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "ccw_") {
		t.Error("list-cdks输出不得含明文或哈希（明文不可再取，设计§11.1）")
	}
}

// ---- list-projects / status ----

func TestListProjectsJSON(t *testing.T) {
	f := newFake()
	f.addProject("alpha")
	f.addProject("beta")
	code, out, _ := run2(t, func(a []string, o, e *bytes.Buffer) int {
		return runListProjects(a, o, e, f)
	}, "--json")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("应有2个项目，got %d", len(rows))
	}
	// 输出必须按slug排序，保证Console对账时稳定可比对。
	if rows[0]["slug"] != "alpha" || rows[1]["slug"] != "beta" {
		t.Errorf("应按slug排序: %v", rows)
	}
}

func TestStatusJSON(t *testing.T) {
	f := newFake()
	f.addProject("alpha")
	code, out, _ := run2(t, func(a []string, o, e *bytes.Buffer) int {
		return runStatus(a, o, e, f, "v-test")
	}, "--json")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("JSON解析失败: %v\n%s", err, out)
	}
	if resp["ok"] != true || resp["version"] != "v-test" {
		t.Errorf("ok/version错误: %v", resp)
	}
	migs, _ := resp["schema_migrations"].([]any)
	if len(migs) != 3 {
		t.Errorf("schema_migrations应有3项，got %v", resp["schema_migrations"])
	}
	projs, _ := resp["projects"].([]any)
	if len(projs) != 1 {
		t.Fatalf("projects应有1项，got %v", resp["projects"])
	}
	p0 := projs[0].(map[string]any)
	if p0["slug"] != "alpha" || p0["active_cdks"] != float64(1) {
		t.Errorf("project字段错误: %v", p0)
	}
	// last_usage_event_at为null时必须显式出现（Console靠它判断采集是否停摆，
	// 字段缺失与null的语义不同：缺失＝旧版本ccwadmin，null＝从未有事件）。
	if _, has := p0["last_usage_event_at"]; !has {
		t.Error("last_usage_event_at字段必须存在（可为null）")
	}
}

// ---- tiers ----

type fakeTierStore struct {
	tiers    []store.QuotaTier
	setName  string
	setBP    int
	assigned string
	assignTo *string
}

func (f *fakeTierStore) ListQuotaTiers(context.Context) ([]store.QuotaTier, error) {
	return f.tiers, nil
}
func (f *fakeTierStore) SetTierShare(_ context.Context, name string, bp int) error {
	f.setName, f.setBP = name, bp
	return nil
}
func (f *fakeTierStore) SetProjectTier(_ context.Context, slug string, name *string) error {
	f.assigned, f.assignTo = slug, name
	return nil
}

// 命令行按**百分比**收，存的是万分之一。让人敲 3300 表示 33% 是不必要的心智负担，
// 但转换错了会让全部限额差 100 倍——所以单独测这一步。
func TestTiersSetConvertsPercentToBasisPoints(t *testing.T) {
	f := &fakeTierStore{tiers: []store.QuotaTier{{Name: "7x", ShareBP: 3300}}}
	var out, errb bytes.Buffer
	if code := runTiers([]string{"--set", "7x", "--percent", "33"}, &out, &errb, f); code != 0 {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	if f.setName != "7x" || f.setBP != 3300 {
		t.Errorf("33%% 应存成 3300 bp，got %s=%d", f.setName, f.setBP)
	}
	// 小数也要对
	runTiers([]string{"--set", "5x", "--percent", "12.5"}, &out, &errb, f)
	if f.setBP != 1250 {
		t.Errorf("12.5%% 应存成 1250 bp，got %d", f.setBP)
	}
}

// 越界的百分比要拒绝：0 或负数会让该档位的项目立刻全员受限，
// >100 则是超卖账号池。
func TestTiersRejectsOutOfRangePercent(t *testing.T) {
	f := &fakeTierStore{}
	var out, errb bytes.Buffer
	for _, p := range []string{"0", "-5", "101", "1000"} {
		if code := runTiers([]string{"--set", "7x", "--percent", p}, &out, &errb, f); code == 0 {
			t.Errorf("--percent %s 应被拒绝", p)
		}
		if f.setName != "" {
			t.Errorf("--percent %s 不该到达存储层", p)
		}
	}
}

// --assign 不带 --tier 表示改回绝对限额（传 NULL），而不是挂到一个空名字的档位。
func TestTiersAssignEmptyMeansNoTier(t *testing.T) {
	f := &fakeTierStore{}
	var out, errb bytes.Buffer
	runTiers([]string{"--assign", "alice"}, &out, &errb, f)
	if f.assigned != "alice" || f.assignTo != nil {
		t.Errorf("留空应传 NULL，got %v", f.assignTo)
	}
	runTiers([]string{"--assign", "bob", "--tier", "5x"}, &out, &errb, f)
	if f.assignTo == nil || *f.assignTo != "5x" {
		t.Errorf("应挂到 5x，got %v", f.assignTo)
	}
}
