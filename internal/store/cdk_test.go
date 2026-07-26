package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ccw/internal/project"
)

// CDK轮换写入端的集成测试（console-fleet-design §11.1.1）。
// 读路径（ResolveCDK校验disabled_at/expires_at、用数据库now()）在001时代就已完备，
// 这里验证的是新补的写入端与两者的配合。需要真实PG（无库自动skip，见testStore）。

func TestProjectBySlug(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()

	p, err := st.GetProjectByID(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ProjectBySlug(ctx, p.Slug)
	if err != nil {
		t.Fatalf("ProjectBySlug(%q): %v", p.Slug, err)
	}
	if got.ID != projectID {
		t.Errorf("ProjectBySlug返回了别的项目：%s != %s", got.ID, projectID)
	}

	_, err = st.ProjectBySlug(ctx, "no-such-slug-ever")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的slug应返回ErrNotFound哨兵（调用方靠它做统一错误），got: %v", err)
	}
}

func TestCountProjects(t *testing.T) {
	st, _ := testStore(t) // testStore自己建了1个项目
	ctx := context.Background()
	before, err := st.CountProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := st.EnsureAccount(ctx, "cnt-"+uuid.NewString()[:8], "test-pool")
	slug := "cnt-" + uuid.NewString()[:8]
	pid, err := st.CreateProject(ctx, accountID, slug, "ccw-"+slug, 1<<30, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Pool.Exec(ctx, `DELETE FROM projects WHERE id=$1`, pid)
		st.Pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, accountID)
	})
	after, err := st.CountProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Errorf("CountProjects: before=%d after=%d，want +1", before, after)
	}
}

// CreateCDK现在返回public_id：Console靠它建立cdk_issues记录（设计§11.1），
// 轮换靠它圈定"除新CDK之外的全部旧CDK"。
func TestCreateCDKReturnsPublicIDAndResolves(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	plain, pub, err := st.CreateCDK(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Pool.Exec(ctx, `DELETE FROM cdks WHERE project_id=$1`, projectID) })
	if pub == "" || len(plain) == 0 {
		t.Fatal("plain与publicID都不得为空")
	}
	p, err := st.ResolveCDK(ctx, plain)
	if err != nil {
		t.Fatalf("新签发的CDK应能通过认证: %v", err)
	}
	if p.ID != projectID {
		t.Errorf("CDK解析到了别的项目")
	}
	infos, err := st.ListCDKs(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].PublicID != pub {
		t.Fatalf("ListCDKs应看到刚签发的CDK，got %+v", infos)
	}
	if infos[0].CreatedAt.IsZero() {
		t.Error("created_at应由003迁移补上并在插入时写入")
	}
	if infos[0].Disabled || infos[0].Expired {
		t.Error("新CDK不应是disabled/expired状态")
	}
}

// 宽限轮换：旧CDK的expires_at设为now()+grace，但**只缩短不延长**（LEAST语义）——
// 已经更早过期的CDK不得因轮换被续命。
func TestExpireOtherProjectCDKsUsesLeast(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	t.Cleanup(func() { st.Pool.Exec(ctx, `DELETE FROM cdks WHERE project_id=$1`, projectID) })

	_, oldPub, err := st.CreateCDK(ctx, projectID) // 无过期时间的旧CDK
	if err != nil {
		t.Fatal(err)
	}
	_, soonPub, err := st.CreateCDK(ctx, projectID) // 已设了更早过期时间的旧CDK
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE cdks SET expires_at = now() + interval '1 hour' WHERE public_id=$1`, soonPub); err != nil {
		t.Fatal(err)
	}
	_, newPub, err := st.CreateCDK(ctx, projectID) // 轮换签发的新CDK
	if err != nil {
		t.Fatal(err)
	}

	affected, err := st.ExpireOtherProjectCDKs(ctx, projectID, newPub, 24*3600)
	if err != nil {
		t.Fatal(err)
	}
	if affected != 2 {
		t.Errorf("应影响2张旧CDK，实际%d", affected)
	}

	// 用数据库now()比对（CLAUDE.md：所有时间窗口用数据库now()与UTC）。
	var oldIn, soonIn float64
	if err := st.Pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM expires_at - now()) FROM cdks WHERE public_id=$1`, oldPub).Scan(&oldIn); err != nil {
		t.Fatal(err)
	}
	if oldIn < 23*3600 || oldIn > 24*3600+60 {
		t.Errorf("无过期时间的旧CDK应变为约24h后过期，实际%.0fs", oldIn)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM expires_at - now()) FROM cdks WHERE public_id=$1`, soonPub).Scan(&soonIn); err != nil {
		t.Fatal(err)
	}
	if soonIn > 3600+60 {
		t.Errorf("已经更早过期的CDK不得被延长（LEAST），实际%.0fs后过期", soonIn)
	}

	// 新CDK不受影响：仍无过期时间。
	var newExpires *time.Time
	if err := st.Pool.QueryRow(ctx,
		`SELECT expires_at FROM cdks WHERE public_id=$1`, newPub).Scan(&newExpires); err != nil {
		t.Fatal(err)
	}
	if newExpires != nil {
		t.Errorf("except指定的新CDK不应被设置过期时间，got %v", newExpires)
	}
}

// 立即撤销：除新CDK外全部disabled_at=now()；重复执行第二次影响0行（幂等）。
func TestDisableOtherProjectCDKs(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	t.Cleanup(func() { st.Pool.Exec(ctx, `DELETE FROM cdks WHERE project_id=$1`, projectID) })

	oldPlain, _, err := st.CreateCDK(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	newPlain, newPub, err := st.CreateCDK(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}

	affected, err := st.DisableOtherProjectCDKs(ctx, projectID, newPub)
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Errorf("应禁用1张旧CDK，实际%d", affected)
	}
	if _, err := st.ResolveCDK(ctx, oldPlain); !errors.Is(err, project.ErrInvalidCDK) {
		t.Errorf("被禁用的旧CDK应立即无法认证，got: %v", err)
	}
	if _, err := st.ResolveCDK(ctx, newPlain); err != nil {
		t.Errorf("新CDK应仍可认证: %v", err)
	}

	again, err := st.DisableOtherProjectCDKs(ctx, projectID, newPub)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("重复执行应影响0行（不重写disabled_at），实际%d", again)
	}
}

func TestDisableCDKByPublicID(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	t.Cleanup(func() { st.Pool.Exec(ctx, `DELETE FROM cdks WHERE project_id=$1`, projectID) })

	plain, pub, err := st.CreateCDK(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DisableCDKByPublicID(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveCDK(ctx, plain); !errors.Is(err, project.ErrInvalidCDK) {
		t.Errorf("禁用后应无法认证，got: %v", err)
	}
	// 已禁用的再禁用/不存在的public_id：都返回ErrNotFound，调用方据此给统一错误。
	if err := st.DisableCDKByPublicID(ctx, pub); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复禁用应返回ErrNotFound，got: %v", err)
	}
	if err := st.DisableCDKByPublicID(ctx, "ffffffffffffffff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知public_id应返回ErrNotFound，got: %v", err)
	}
}

// list-cdks的状态字段由数据库判定（expired按数据库now()），输出永不含明文或哈希。
func TestListCDKsStates(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	t.Cleanup(func() { st.Pool.Exec(ctx, `DELETE FROM cdks WHERE project_id=$1`, projectID) })

	_, activePub, _ := st.CreateCDK(ctx, projectID)
	_, expiredPub, _ := st.CreateCDK(ctx, projectID)
	_, disabledPub, _ := st.CreateCDK(ctx, projectID)
	st.Pool.Exec(ctx, `UPDATE cdks SET expires_at = now() - interval '1 minute' WHERE public_id=$1`, expiredPub)
	st.Pool.Exec(ctx, `UPDATE cdks SET disabled_at = now() WHERE public_id=$1`, disabledPub)

	infos, err := st.ListCDKs(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("应有3张CDK，got %d", len(infos))
	}
	byPub := map[string]CDKInfo{}
	for _, i := range infos {
		byPub[i.PublicID] = i
	}
	if i := byPub[activePub]; i.Disabled || i.Expired {
		t.Errorf("active CDK状态错误: %+v", i)
	}
	if i := byPub[expiredPub]; !i.Expired || i.Disabled {
		t.Errorf("expired CDK状态错误: %+v", i)
	}
	if i := byPub[disabledPub]; !i.Disabled {
		t.Errorf("disabled CDK状态错误: %+v", i)
	}
}

// status --json的数据面：项目基本面 + 逻辑磁盘用量 + 活跃CDK数 + 最近用量事件时间
// （最后一项是发现"采集停摆"的关键信号，Console巡检§11.5用它）。
func TestStatusProjects(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	t.Cleanup(func() { st.Pool.Exec(ctx, `DELETE FROM cdks WHERE project_id=$1`, projectID) })

	if _, _, err := st.CreateCDK(ctx, projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO usage_events (project_id, occurred_at, model, input_tokens, output_tokens,
		  cache_read_tokens, cache_write_tokens, weighted_units, source_event_id)
		VALUES ($1, now(), 'claude-x', 1,1,0,0, 6, 'status-ev-1')`, projectID); err != nil {
		t.Fatal(err)
	}

	rows, err := st.StatusProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine *StatusProject
	for i := range rows {
		if rows[i].ID == projectID {
			mine = &rows[i]
		}
	}
	if mine == nil {
		t.Fatal("StatusProjects应包含testStore建的项目")
	}
	if mine.ActiveCDKs != 1 {
		t.Errorf("ActiveCDKs=%d, want 1", mine.ActiveCDKs)
	}
	if mine.LastUsageEventAt == nil {
		t.Error("LastUsageEventAt不应为nil（刚插入过事件）")
	}
	if mine.Slug == "" || mine.ContainerName == "" {
		t.Errorf("项目基本面字段缺失: %+v", mine)
	}
}

func TestSchemaMigrationsList(t *testing.T) {
	st, _ := testStore(t)
	names, err := st.SchemaMigrations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"001_initial.sql": false, "002_account_pool_limits.sql": false, "003_cdk_created_at.sql": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("SchemaMigrations缺%s；got %v", n, names)
		}
	}
}
