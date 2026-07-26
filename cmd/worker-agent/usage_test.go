package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccw/internal/project"
	"ccw/internal/usage"
)

// 假Sink：记录每条事件的归属项目，用于验证不串味。
type fakeSink struct {
	mu     sync.Mutex
	byProj map[string]int64
	fail   error
	panics bool
}

func newFakeSink() *fakeSink { return &fakeSink{byProj: map[string]int64{}} }

func (f *fakeSink) Insert(_ context.Context, projectID string, _ usage.Event, weighted int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panics {
		panic("boom")
	}
	if f.fail != nil {
		return f.fail
	}
	f.byProj[projectID] += weighted
	return nil
}

func (f *fakeSink) total(projectID string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byProj[projectID]
}

// 假OffsetStore：进程内游标，语义与PG实现一致（无记录返回零值）。
type fakeOffsets struct {
	mu sync.Mutex
	m  map[string][2]any
}

func newFakeOffsets() *fakeOffsets { return &fakeOffsets{m: map[string][2]any{}} }

func (f *fakeOffsets) Load(_ context.Context, projectID, id string) (int64, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[projectID+"|"+id]
	if !ok {
		return 0, "", nil
	}
	return v[0].(int64), v[1].(string), nil
}

func (f *fakeOffsets) Save(_ context.Context, projectID, id, _ string, off int64, partial string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[projectID+"|"+id] = [2]any{off, partial}
	return nil
}

type fakeLister struct{ projects []project.Project }

func (f fakeLister) ListProjects(context.Context) ([]project.Project, error) { return f.projects, nil }

func jsonlLine(requestID string, input, output int64) string {
	return `{"type":"assistant","requestId":"` + requestID + `","timestamp":"2026-07-26T10:00:00Z",` +
		`"message":{"model":"claude-x","usage":{"input_tokens":` + itoa(input) +
		`,"output_tokens":` + itoa(output) + `,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func writeJSONL(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func proj(id, slug string) project.Project {
	return project.Project{ID: id, Slug: slug, AccountID: "acct", ContainerName: "ccw-" + slug}
}

var testWeights = usage.Weights{Input: 1, Output: 5, CacheRead: 1, CacheWrite: 1}

// U6：两个项目的事件各归各的project_id，归属由目录（挂载）决定。
func TestRunOnceKeepsProjectsSeparate(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "alpha"), "s1.jsonl", jsonlLine("req-a", 100, 20)) // 200
	writeJSONL(t, filepath.Join(root, "beta"), "s1.jsonl", jsonlLine("req-b", 10, 2))    // 20

	sink, offs := newFakeSink(), newFakeOffsets()
	u := newUsageCollectors(root, testWeights, sink, offs)
	projects := []project.Project{proj("id-a", "alpha"), proj("id-b", "beta")}

	if errs := u.runOnce(context.Background(), projects); len(errs) != 0 {
		t.Fatalf("不应有错误: %v", errs)
	}
	if got := sink.total("id-a"); got != 200 {
		t.Errorf("项目alpha=%d，期望200", got)
	}
	if got := sink.total("id-b"); got != 20 {
		t.Errorf("项目beta=%d，期望20", got)
	}
}

// U3：采集范围是传入的全部项目——没有任何"活跃连接"的概念参与筛选。
// tmux在客户端断开后继续跑，只采活跃项目会丢掉无人值守期间的用量。
func TestRunOnceCollectsAllProjectsNotJustActive(t *testing.T) {
	root := t.TempDir()
	for _, slug := range []string{"p1", "p2", "p3"} {
		writeJSONL(t, filepath.Join(root, slug), "s.jsonl", jsonlLine("req-"+slug, 10, 0))
	}
	sink, offs := newFakeSink(), newFakeOffsets()
	u := newUsageCollectors(root, testWeights, sink, offs)
	projects := []project.Project{proj("i1", "p1"), proj("i2", "p2"), proj("i3", "p3")}

	u.runOnce(context.Background(), projects)
	for _, id := range []string{"i1", "i2", "i3"} {
		if sink.total(id) != 10 {
			t.Errorf("项目%s没有被采集（=%d）", id, sink.total(id))
		}
	}
}

// 漏挂卷是本次接线最危险的失败模式：目录不存在时Collector.Scan返回nil，
// 表现为"采集器在跑、日志正常、usage_events永远为空"。必须显式报错。
func TestRunOnceReportsMissingDirectory(t *testing.T) {
	root := t.TempDir()
	sink, offs := newFakeSink(), newFakeOffsets()
	u := newUsageCollectors(root, testWeights, sink, offs)

	errs := u.runOnce(context.Background(), []project.Project{proj("id-x", "ghost")})
	if len(errs) != 1 {
		t.Fatalf("目录不存在应报1个错误，实际%d个", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "ghost-claude-projects") {
		t.Errorf("错误信息应指出该挂哪个卷，实际：%v", errs[0])
	}

	// 同一项目不应每轮重复刷同样的错误。
	if errs := u.runOnce(context.Background(), []project.Project{proj("id-x", "ghost")}); len(errs) != 0 {
		t.Errorf("重复的目录缺失不应反复报告，实际报了%d次", len(errs))
	}
}

// U5：Sink失败时游标不前进，修好之后不丢事件、也不重复计量。
func TestSinkFailureDoesNotAdvanceCursor(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "alpha"), "s.jsonl", jsonlLine("req-a", 100, 20))

	sink, offs := newFakeSink(), newFakeOffsets()
	sink.fail = errors.New("db down")
	u := newUsageCollectors(root, testWeights, sink, offs)
	projects := []project.Project{proj("id-a", "alpha")}

	if errs := u.runOnce(context.Background(), projects); len(errs) != 1 {
		t.Fatalf("Sink失败应被报告，实际%d个错误", len(errs))
	}
	if got := sink.total("id-a"); got != 0 {
		t.Fatalf("失败时不应记账，实际%d", got)
	}

	sink.fail = nil
	u.runOnce(context.Background(), projects)
	if got := sink.total("id-a"); got != 200 {
		t.Errorf("恢复后应补齐，得到%d，期望200", got)
	}
	// 再跑一轮：游标已前进，不应重复计量。
	u.runOnce(context.Background(), projects)
	if got := sink.total("id-a"); got != 200 {
		t.Errorf("不应重复计量，得到%d，期望200", got)
	}
}

// U4：半行（无换行结尾）不入账；补全后恰好入账一次。
func TestPartialLineIsNotCounted(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "alpha")
	full := jsonlLine("req-1", 10, 0)
	half := strings.TrimSuffix(jsonlLine("req-2", 999, 0), "\n")
	writeJSONL(t, dir, "s.jsonl", full+half)

	sink, offs := newFakeSink(), newFakeOffsets()
	u := newUsageCollectors(root, testWeights, sink, offs)
	projects := []project.Project{proj("id-a", "alpha")}

	u.runOnce(context.Background(), projects)
	if got := sink.total("id-a"); got != 10 {
		t.Fatalf("半行不应入账：得到%d，期望10", got)
	}
	// 补上换行，半行成为完整行。
	writeJSONL(t, dir, "s.jsonl", full+half+"\n")
	u.runOnce(context.Background(), projects)
	if got := sink.total("id-a"); got != 10+999 {
		t.Errorf("补全后应恰好入账一次：得到%d，期望%d", got, 10+999)
	}
}

// U7：单个项目的panic不得中断其余项目的采集。
func TestPanicInOneProjectDoesNotStopOthers(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "alpha"), "s.jsonl", jsonlLine("req-a", 10, 0))
	writeJSONL(t, filepath.Join(root, "beta"), "s.jsonl", jsonlLine("req-b", 10, 0))

	sink, offs := newFakeSink(), newFakeOffsets()
	sink.panics = true
	u := newUsageCollectors(root, testWeights, sink, offs)
	projects := []project.Project{proj("id-a", "alpha"), proj("id-b", "beta")}

	errs := u.runOnce(context.Background(), projects) // 不应把进程带走
	if len(errs) != 2 {
		t.Fatalf("两个项目都应报panic错误，实际%d个", len(errs))
	}
	for _, e := range errs {
		if !strings.Contains(e.Error(), "panic") {
			t.Errorf("错误应标明panic：%v", e)
		}
	}
}

// 循环本身：ctx取消后必须退出，不能泄漏goroutine。
func TestRunUsageLoopStopsOnContextCancel(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "alpha"), "s.jsonl", jsonlLine("req-a", 10, 0))
	sink, offs := newFakeSink(), newFakeOffsets()
	u := newUsageCollectors(root, testWeights, sink, offs)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runUsageLoop(ctx, fakeLister{[]project.Project{proj("id-a", "alpha")}}, u,
			time.Millisecond, func(string, ...any) {})
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx取消后循环未退出")
	}
	if sink.total("id-a") != 10 {
		t.Errorf("循环期间应完成至少一轮采集，得到%d", sink.total("id-a"))
	}
}
