package consolestore

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"ccw/internal/pipeline"
)

func newNode(t *testing.T, st *Store) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	suffix := uuid.NewString()[:8]
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO nodes (id, name, host, ssh_user, status) VALUES ($1,$2,'203.0.113.7','ccw','new')`,
		id, "node-"+suffix); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Pool.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, id) }) // runs/steps级联
	return id
}

func TestProvisionRecorderRoundTrip(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	nodeID := newNode(t, st)

	runID, err := st.CreateRun(ctx, nodeID, "bootstrap", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StepStarted(ctx, runID, 1, "connect"); err != nil {
		t.Fatal(err)
	}
	if err := st.StepFinished(ctx, runID, 1, "connect", pipeline.StatusSucceeded, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.StepStarted(ctx, runID, 2, "install-docker"); err != nil {
		t.Fatal(err)
	}
	if err := st.StepFinished(ctx, runID, 2, "install-docker", pipeline.StatusFailed, 100, "apt挂了"); err != nil {
		t.Fatal(err)
	}
	if err := st.RunFinished(ctx, runID, pipeline.StatusFailed); err != nil {
		t.Fatal(err)
	}

	done, err := st.CompletedSteps(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !done["connect"] || done["install-docker"] {
		t.Errorf("只有成功/跳过的步骤算完成: %v", done)
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "failed" || len(run.Steps) != 2 {
		t.Fatalf("运行详情错误: %+v", run)
	}
	if run.Steps[1].ExitCode == nil || *run.Steps[1].ExitCode != 100 {
		t.Errorf("退出码应入库: %+v", run.Steps[1])
	}
	if run.FinishedAt == nil {
		t.Error("finished_at应已写入")
	}
}

// 续跑：同一个(run,seq)再次StepStarted不得因主键冲突失败（A9的存储侧前提）。
func TestStepStartedIsUpsertForResume(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	nodeID := newNode(t, st)
	runID, _ := st.CreateRun(ctx, nodeID, "bootstrap", "")

	st.StepStarted(ctx, runID, 2, "install-docker")
	st.StepFinished(ctx, runID, 2, "install-docker", pipeline.StatusFailed, 100, "apt挂了")
	// 续跑：同一步骤重新开始
	if err := st.StepStarted(ctx, runID, 2, "install-docker"); err != nil {
		t.Fatalf("续跑时重复StepStarted不应失败: %v", err)
	}
	if err := st.StepFinished(ctx, runID, 2, "install-docker", pipeline.StatusSucceeded, 0, ""); err != nil {
		t.Fatal(err)
	}
	run, _ := st.GetRun(ctx, runID)
	if len(run.Steps) != 1 || run.Steps[0].Status != "succeeded" {
		t.Errorf("续跑后应覆盖为succeeded且不新增行: %+v", run.Steps)
	}
	if run.Steps[0].ExitCode != nil {
		t.Errorf("成功后应清掉上次的退出码: %+v", run.Steps[0])
	}
}

func TestGetRunNotFound(t *testing.T) {
	st := testConsoleStore(t)
	if _, err := st.GetRun(context.Background(), uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知runID应ErrNotFound，got %v", err)
	}
}

func TestListRuns(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	nodeID := newNode(t, st)
	for i := 0; i < 3; i++ {
		if _, err := st.CreateRun(ctx, nodeID, "redeploy", ""); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := st.ListRuns(ctx, nodeID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Errorf("应有3次运行，got %d", len(runs))
	}
	if runs[0].Kind != "redeploy" || runs[0].Status != "running" {
		t.Errorf("字段错误: %+v", runs[0])
	}
}

// 引擎与真实记账合起来跑一遍：失败→续跑→成功，验证A9在真实数据库上成立。
func TestEngineWithPGRecorderResume(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	nodeID := newNode(t, st)
	runID, _ := st.CreateRun(ctx, nodeID, "bootstrap", "")

	var ran []string
	steps := func(fail bool) []pipeline.Step {
		return []pipeline.Step{
			{Name: "connect", Run: func(context.Context, pipeline.Logf) error {
				ran = append(ran, "connect")
				return nil
			}},
			{Name: "install-docker", Run: func(context.Context, pipeline.Logf) error {
				ran = append(ran, "install-docker")
				if fail {
					return &pipeline.ExitError{Code: 100, Err: errors.New("apt挂了")}
				}
				return nil
			}},
			{Name: "compose-up", Run: func(context.Context, pipeline.Logf) error {
				ran = append(ran, "compose-up")
				return nil
			}},
		}
	}
	r := &pipeline.Runner{Recorder: st}
	if err := r.Run(ctx, runID, steps(true)); err == nil {
		t.Fatal("首次应失败")
	}
	run, _ := st.GetRun(ctx, runID)
	if run.Status != "failed" {
		t.Errorf("run应记为failed: %s", run.Status)
	}

	ran = nil
	if err := r.Run(ctx, runID, steps(false)); err != nil {
		t.Fatalf("续跑应成功: %v", err)
	}
	if len(ran) != 2 || ran[0] != "install-docker" {
		t.Errorf("续跑应跳过connect: %v", ran)
	}
	run, _ = st.GetRun(ctx, runID)
	if run.Status != "succeeded" {
		t.Errorf("续跑后run应succeeded: %s", run.Status)
	}
}
