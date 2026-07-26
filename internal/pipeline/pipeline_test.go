package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 用假步骤单测幂等与恢复（设计C7指定的方式）。

type record struct {
	seq      int
	name     string
	status   Status
	exitCode int
	errMsg   string
}

type fakeRecorder struct {
	mu        sync.Mutex
	started   []string
	finished  []record
	runStatus Status
	completed map[string]bool
	failOn    string // 在该步骤的StepFinished上返回错误
}

func newRecorder() *fakeRecorder {
	return &fakeRecorder{completed: map[string]bool{}}
}

func (f *fakeRecorder) StepStarted(_ context.Context, _ string, _ int, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, name)
	return nil
}
func (f *fakeRecorder) StepFinished(_ context.Context, _ string, seq int, name string, st Status, code int, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn == name {
		return errors.New("recorder down")
	}
	f.finished = append(f.finished, record{seq, name, st, code, msg})
	if st == StatusSucceeded || st == StatusSkipped {
		f.completed[name] = true
	}
	return nil
}
func (f *fakeRecorder) RunFinished(_ context.Context, _ string, st Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runStatus = st
	return nil
}
func (f *fakeRecorder) CompletedSteps(context.Context, string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for k, v := range f.completed {
		out[k] = v
	}
	return out, nil
}

func okStep(name string, ran *[]string, mu *sync.Mutex) Step {
	return Step{Name: name, Run: func(context.Context, Logf) error {
		mu.Lock()
		defer mu.Unlock()
		*ran = append(*ran, name)
		return nil
	}}
}

func TestRunAllSucceed(t *testing.T) {
	rec := newRecorder()
	var mu sync.Mutex
	var ran []string
	r := &Runner{Recorder: rec}
	err := r.Run(context.Background(), "run-1", []Step{
		okStep("connect", &ran, &mu), okStep("probe", &ran, &mu), okStep("harden", &ran, &mu),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ran, ",") != "connect,probe,harden" {
		t.Errorf("执行顺序错误: %v", ran)
	}
	if rec.runStatus != StatusSucceeded {
		t.Errorf("run状态=%s", rec.runStatus)
	}
	for i, f := range rec.finished {
		if f.seq != i+1 || f.status != StatusSucceeded {
			t.Errorf("记账错误: %+v", f)
		}
	}
}

// precheck命中＝跳过并标记skipped，而不是重做一遍（§5.3、A9）。
func TestPrecheckSkips(t *testing.T) {
	rec := newRecorder()
	ranRun := false
	r := &Runner{Recorder: rec}
	err := r.Run(context.Background(), "run-1", []Step{{
		Name:     "install-docker",
		Precheck: func(context.Context, Logf) (bool, error) { return true, nil },
		Run:      func(context.Context, Logf) error { ranRun = true; return nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ranRun {
		t.Error("precheck满足时不应执行Run")
	}
	if len(rec.finished) != 1 || rec.finished[0].status != StatusSkipped {
		t.Errorf("应记为skipped: %+v", rec.finished)
	}
}

// precheck自己出错时按"未满足"处理并继续——precheck只是优化，
// 它坏了不该阻断部署。
func TestPrecheckErrorFallsThroughToRun(t *testing.T) {
	rec := newRecorder()
	ranRun := false
	var logs []string
	r := &Runner{Recorder: rec, Log: func(f string, a ...any) { logs = append(logs, f) }}
	err := r.Run(context.Background(), "run-1", []Step{{
		Name:     "probe",
		Precheck: func(context.Context, Logf) (bool, error) { return false, errors.New("ssh抖动") },
		Run:      func(context.Context, Logf) error { ranRun = true; return nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !ranRun {
		t.Error("precheck出错应继续执行Run")
	}
	if rec.finished[0].status != StatusSucceeded {
		t.Errorf("应成功: %+v", rec.finished[0])
	}
}

// 失败即停：后续步骤保持pending（不执行、不记账）。
func TestFailureStopsAndLeavesRestPending(t *testing.T) {
	rec := newRecorder()
	var mu sync.Mutex
	var ran []string
	r := &Runner{Recorder: rec}
	err := r.Run(context.Background(), "run-1", []Step{
		okStep("connect", &ran, &mu),
		{Name: "install-docker", Run: func(context.Context, Logf) error { return errors.New("apt挂了") }},
		okStep("compose-up", &ran, &mu),
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	var se *ErrStepFailed
	if !errors.As(err, &se) || se.Step != "install-docker" {
		t.Errorf("错误应指明失败步骤，got %v", err)
	}
	for _, n := range ran {
		if n == "compose-up" {
			t.Error("失败后不得继续执行后续步骤")
		}
	}
	if rec.runStatus != StatusFailed {
		t.Errorf("run应记为failed，got %s", rec.runStatus)
	}
	// 失败步骤有记账，后续步骤完全没有记录（保持pending）
	names := map[string]Status{}
	for _, f := range rec.finished {
		names[f.name] = f.status
	}
	if names["install-docker"] != StatusFailed {
		t.Errorf("失败步骤应记为failed: %v", names)
	}
	if _, has := names["compose-up"]; has {
		t.Error("未执行的步骤不得被记账（否则恢复时分不清真失败与没轮到）")
	}
}

// A9：从失败步重跑，已完成步骤标记skipped且不重复执行。
func TestResumeSkipsCompleted(t *testing.T) {
	rec := newRecorder()
	var mu sync.Mutex
	var ran []string
	steps := func(failInstall bool) []Step {
		return []Step{
			okStep("connect", &ran, &mu),
			{Name: "install-docker", Run: func(context.Context, Logf) error {
				mu.Lock()
				defer mu.Unlock()
				ran = append(ran, "install-docker")
				if failInstall {
					return errors.New("apt挂了")
				}
				return nil
			}},
			okStep("compose-up", &ran, &mu),
		}
	}
	r := &Runner{Recorder: rec}
	if err := r.Run(context.Background(), "run-1", steps(true)); err == nil {
		t.Fatal("首次应失败")
	}
	ranFirst := append([]string(nil), ran...)
	if strings.Join(ranFirst, ",") != "connect,install-docker" {
		t.Fatalf("首次执行: %v", ranFirst)
	}

	// 同一个runID重跑
	ran = nil
	if err := r.Run(context.Background(), "run-1", steps(false)); err != nil {
		t.Fatalf("续跑应成功: %v", err)
	}
	if strings.Join(ran, ",") != "install-docker,compose-up" {
		t.Errorf("续跑应跳过connect、从失败步重开，实际: %v", ran)
	}
	if rec.runStatus != StatusSucceeded {
		t.Errorf("续跑后run应成功，got %s", rec.runStatus)
	}
}

// 记账失败必须中止：没有记账的执行等于没有可恢复性。
func TestRecorderFailureAborts(t *testing.T) {
	rec := newRecorder()
	rec.failOn = "probe"
	var mu sync.Mutex
	var ran []string
	r := &Runner{Recorder: rec}
	err := r.Run(context.Background(), "run-1", []Step{
		okStep("connect", &ran, &mu),
		okStep("probe", &ran, &mu),
		okStep("harden", &ran, &mu),
	})
	if err == nil || !strings.Contains(err.Error(), "记账") {
		t.Fatalf("记账失败应中止流水线，got %v", err)
	}
	for _, n := range ran {
		if n == "harden" {
			t.Error("记账失败后不得继续")
		}
	}
}

// 步骤panic不得带走整个进程（Console里未recover的panic会终止全部服务）。
func TestStepPanicIsContained(t *testing.T) {
	rec := newRecorder()
	r := &Runner{Recorder: rec}
	err := r.Run(context.Background(), "run-1", []Step{
		{Name: "boom", Run: func(context.Context, Logf) error { panic("unexpected nil") }},
	})
	if err == nil {
		t.Fatal("panic应转为步骤失败")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("错误应指明panic: %v", err)
	}
	if rec.finished[0].status != StatusFailed {
		t.Errorf("应记为failed: %+v", rec.finished[0])
	}
}

func TestStepTimeout(t *testing.T) {
	rec := newRecorder()
	r := &Runner{Recorder: rec, DefaultTimeout: 100 * time.Millisecond}
	start := time.Now()
	err := r.Run(context.Background(), "run-1", []Step{{
		Name: "slow",
		Run: func(ctx context.Context, _ Logf) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}})
	if err == nil {
		t.Fatal("超时应失败")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("超时未生效")
	}
	// 步骤自己的Timeout优先于DefaultTimeout
	rec2 := newRecorder()
	r2 := &Runner{Recorder: rec2, DefaultTimeout: time.Hour}
	err = r2.Run(context.Background(), "run-2", []Step{{
		Name: "slow", Timeout: 100 * time.Millisecond,
		Run: func(ctx context.Context, _ Logf) error { <-ctx.Done(); return ctx.Err() },
	}})
	if err == nil {
		t.Fatal("步骤级Timeout应生效")
	}
}

// 整体取消：run记为cancelled，未执行的步骤保持pending。
func TestCancelMarksRunCancelled(t *testing.T) {
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var ran []string
	r := &Runner{Recorder: rec}
	err := r.Run(ctx, "run-1", []Step{
		{Name: "first", Run: func(context.Context, Logf) error { cancel(); return nil }},
		okStep("second", &ran, &mu),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回Canceled，got %v", err)
	}
	if len(ran) != 0 {
		t.Error("取消后不得执行后续步骤")
	}
	if rec.runStatus != StatusCancelled {
		t.Errorf("run应记为cancelled，got %s", rec.runStatus)
	}
}

// 远端退出码经ExitError带回记账层（provision_steps.exit_code）。
func TestExitCodeRecorded(t *testing.T) {
	rec := newRecorder()
	r := &Runner{Recorder: rec}
	err := r.Run(context.Background(), "run-1", []Step{{
		Name: "healthcheck",
		Run: func(context.Context, Logf) error {
			return &ExitError{Code: 42, Err: errors.New("curl失败")}
		},
	}})
	if err == nil {
		t.Fatal("应失败")
	}
	if rec.finished[0].exitCode != 42 {
		t.Errorf("退出码应被记账，got %d", rec.finished[0].exitCode)
	}
	if !strings.Contains(rec.finished[0].errMsg, "curl失败") {
		t.Errorf("错误信息应入库: %q", rec.finished[0].errMsg)
	}
}

// 步骤日志带步骤名前缀，便于在混合流里定位。
func TestLogPrefixing(t *testing.T) {
	rec := newRecorder()
	var mu sync.Mutex
	var logs []string
	r := &Runner{Recorder: rec, Log: func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, f)
	}}
	r.Run(context.Background(), "run-1", []Step{{
		Name: "probe",
		Run: func(_ context.Context, log Logf) error {
			log("发行版=Ubuntu 24.04")
			return nil
		},
	}})
	found := false
	for _, l := range logs {
		if strings.HasPrefix(l, "[probe] ") {
			found = true
		}
	}
	if !found {
		t.Errorf("步骤日志应带步骤名前缀: %v", logs)
	}
}
