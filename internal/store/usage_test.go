package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"ccw/internal/quota"
	"ccw/internal/usage"
)

// 接口断言：编译期保证*Store仍然满足用量链路的三个接口。
// 这条不需要数据库，在任何机器上都会跑——签名漂移会直接编译失败，
// 而不是等到部署后表现为"采集器在跑但usage_events为空"。
var (
	_ usage.Sink        = (*Store)(nil)
	_ usage.OffsetStore = (*Store)(nil)
	_ quota.UsageReader = (*Store)(nil)
)

// testStore：需要真实PostgreSQL的集成测试入口。
// 未设置CCW_TEST_DATABASE_URL时skip——按CLAUDE.md，"没验证"要如实表现为skip，
// 不用空断言伪装成通过。本地跑法见函数内的skip信息。
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := os.Getenv("CCW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("需要PostgreSQL：设置 CCW_TEST_DATABASE_URL=postgres://... 后重跑")
	}
	ctx := context.Background()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	// 每个测试用独立的account/project，避免相互污染；结束时删掉自己的数据。
	suffix := uuid.NewString()[:8]
	accountID, err := st.EnsureAccount(ctx, "test-"+suffix, "test-pool")
	if err != nil {
		t.Fatalf("建account失败: %v", err)
	}
	projectID, err := st.CreateProject(ctx, accountID, "test-"+suffix, "ccw-test-"+suffix,
		1<<30, 1_000_000, 10_000_000)
	if err != nil {
		t.Fatalf("建project失败: %v", err)
	}
	t.Cleanup(func() {
		st.Pool.Exec(ctx, `DELETE FROM usage_events WHERE project_id=$1`, projectID)
		st.Pool.Exec(ctx, `DELETE FROM usage_offsets WHERE project_id=$1`, projectID)
		st.Pool.Exec(ctx, `DELETE FROM projects WHERE id=$1`, projectID)
		st.Pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, accountID)
		st.Pool.Close()
	})
	return st, projectID
}

func sumWeighted(t *testing.T, st *Store, projectID string) int64 {
	t.Helper()
	n, err := st.WindowUsed(context.Background(), projectID, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("WindowUsed失败: %v", err)
	}
	return n
}

func countEvents(t *testing.T, st *Store, projectID string) int64 {
	t.Helper()
	var n int64
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM usage_events WHERE project_id=$1`, projectID).Scan(&n); err != nil {
		t.Fatalf("count失败: %v", err)
	}
	return n
}

// U1：同一(project_id, source_event_id)重复写入不增加行数、不增加总量。
// 这是"Sink失败后游标不前进、下轮重扫"能安全兜底的前提。
func TestUsageInsertIsIdempotent(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	e := usage.Event{
		SourceEventID: "req-idempotent", OccurredAt: time.Now().UTC(),
		Model: "claude-x", Input: 100, Output: 20,
	}
	w := usage.Weights{Input: 1, Output: 5, CacheRead: 1, CacheWrite: 1}
	want := usage.Weighted(e, w) // 100*1 + 20*5 = 200

	for i := 0; i < 3; i++ {
		if err := st.Insert(ctx, projectID, e, want); err != nil {
			t.Fatalf("第%d次Insert失败: %v", i+1, err)
		}
	}
	if got := countEvents(t, st, projectID); got != 1 {
		t.Errorf("重复写入应只有1行，实际%d行", got)
	}
	if got := sumWeighted(t, st, projectID); got != want {
		t.Errorf("weighted总量=%d，期望%d", got, want)
	}
}

// U2：同一requestId再次出现且token更大时，按GREATEST逐字段取最大值。
// 若实现写成DO NOTHING，流式响应过程中先落盘的小值会被永久保留，用量偏低。
func TestUsageInsertTakesGreatest(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	w := usage.Weights{Input: 1, Output: 5, CacheRead: 1, CacheWrite: 1}
	at := time.Now().UTC()

	small := usage.Event{SourceEventID: "req-grow", OccurredAt: at, Model: "claude-x", Input: 100, Output: 10}
	big := usage.Event{SourceEventID: "req-grow", OccurredAt: at, Model: "claude-x", Input: 100, Output: 80}

	if err := st.Insert(ctx, projectID, small, usage.Weighted(small, w)); err != nil {
		t.Fatalf("Insert small: %v", err)
	}
	if err := st.Insert(ctx, projectID, big, usage.Weighted(big, w)); err != nil {
		t.Fatalf("Insert big: %v", err)
	}
	if got, want := sumWeighted(t, st, projectID), usage.Weighted(big, w); got != want {
		t.Errorf("应更新为较大值：得到%d，期望%d", got, want)
	}

	// 反向：更小的值再次到达时不得把已记录的值改小。
	if err := st.Insert(ctx, projectID, small, usage.Weighted(small, w)); err != nil {
		t.Fatalf("Insert small again: %v", err)
	}
	if got, want := sumWeighted(t, st, projectID), usage.Weighted(big, w); got != want {
		t.Errorf("较小值不应覆盖较大值：得到%d，期望%d", got, want)
	}
}

// U6：两个项目各自的事件互不串味——归属由project_id保证。
func TestUsageInsertIsPerProject(t *testing.T) {
	st, projectA := testStore(t)
	ctx := context.Background()
	accountID, err := st.EnsureAccount(ctx, "test-b-"+uuid.NewString()[:8], "test-pool")
	if err != nil {
		t.Fatalf("建account失败: %v", err)
	}
	slug := "test-b-" + uuid.NewString()[:8]
	projectB, err := st.CreateProject(ctx, accountID, slug, "ccw-"+slug, 1<<30, 1_000_000, 10_000_000)
	if err != nil {
		t.Fatalf("建project失败: %v", err)
	}
	t.Cleanup(func() {
		st.Pool.Exec(ctx, `DELETE FROM usage_events WHERE project_id=$1`, projectB)
		st.Pool.Exec(ctx, `DELETE FROM projects WHERE id=$1`, projectB)
		st.Pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, accountID)
	})

	at := time.Now().UTC()
	// 故意用相同的source_event_id：唯一键是(project_id, source_event_id)，
	// 两个项目各自应有独立的一行。
	e := usage.Event{SourceEventID: "req-shared-id", OccurredAt: at, Model: "claude-x", Input: 10}
	if err := st.Insert(ctx, projectA, e, 10); err != nil {
		t.Fatalf("Insert A: %v", err)
	}
	if err := st.Insert(ctx, projectB, e, 70); err != nil {
		t.Fatalf("Insert B: %v", err)
	}
	if got := sumWeighted(t, st, projectA); got != 10 {
		t.Errorf("项目A用量=%d，期望10（不应受B影响）", got)
	}
	if got := sumWeighted(t, st, projectB); got != 70 {
		t.Errorf("项目B用量=%d，期望70", got)
	}
}

// 游标：首次读取返回零值，写入后可原样读回，重复写入是覆盖而非报错。
func TestUsageOffsetRoundTrip(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	const id = "dev:inode:1234"

	off, partial, err := st.Load(ctx, projectID, id)
	if err != nil {
		t.Fatalf("首次Load不应报错: %v", err)
	}
	if off != 0 || partial != "" {
		t.Errorf("首次Load应为零游标，得到offset=%d partial=%q", off, partial)
	}

	if err := st.Save(ctx, projectID, id, "/srv/ccw/usage/p/a.jsonl", 4096, `{"half":`); err != nil {
		t.Fatalf("Save失败: %v", err)
	}
	off, partial, err = st.Load(ctx, projectID, id)
	if err != nil {
		t.Fatalf("Load失败: %v", err)
	}
	if off != 4096 || partial != `{"half":` {
		t.Errorf("读回不一致：offset=%d partial=%q", off, partial)
	}

	// 同一identity再次Save应覆盖（文件继续增长时每轮都会发生）。
	if err := st.Save(ctx, projectID, id, "/srv/ccw/usage/p/a.jsonl", 8192, ""); err != nil {
		t.Fatalf("二次Save失败: %v", err)
	}
	off, partial, err = st.Load(ctx, projectID, id)
	if err != nil {
		t.Fatalf("二次Load失败: %v", err)
	}
	if off != 8192 || partial != "" {
		t.Errorf("覆盖后不一致：offset=%d partial=%q", off, partial)
	}
}

// 采集器与存储层合起来的语义：用真实PG做Sink/OffsetStore跑一遍Collector，
// 断言"重复Scan不重复计量"（U1在采集器层面的表现）。
func TestCollectorWithPGSinkIsIdempotent(t *testing.T) {
	st, projectID := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	line := `{"type":"assistant","requestId":"req-collect-1","timestamp":"2026-07-26T10:00:00Z",` +
		`"message":{"model":"claude-x","usage":{"input_tokens":100,"output_tokens":20,` +
		`"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(dir+"/session.jsonl", []byte(line), 0o600); err != nil {
		t.Fatalf("写测试JSONL失败: %v", err)
	}

	c := &usage.Collector{
		Dir: dir, ProjectID: projectID, Sink: st, Offsets: st,
		Weights: usage.Weights{Input: 1, Output: 5, CacheRead: 1, CacheWrite: 1},
	}
	for i := 0; i < 3; i++ {
		if err := c.Scan(ctx); err != nil {
			t.Fatalf("第%d次Scan失败: %v", i+1, err)
		}
	}
	if got := countEvents(t, st, projectID); got != 1 {
		t.Errorf("三次Scan应只入账1行，实际%d行", got)
	}
	if got := sumWeighted(t, st, projectID); got != 200 {
		t.Errorf("weighted=%d，期望200", got)
	}
}
