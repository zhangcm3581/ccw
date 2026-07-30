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

// 重置节点会毁掉节点自己的 Postgres 卷，那台机器上的 CDK 哈希随之消失。
// RevokeNodeCDKs 要把它们全标成已撤销——否则 /connect（只过滤 revoked_at
// IS NULL）仍会把 public-id 解析成接入域名，使用者拿到域名却在 exchange
// 时吃 invalid_cdk。
func TestRevokeNodeCDKsOnlyTouchesThatNode(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	nodeA, nodeB := newNode(t, st), newNode(t, st)

	mk := func(nodeID, slug, pub string) {
		pid, err := st.UpsertNodeProject(ctx, nodeID, slug, "remote-"+slug, 15<<30, 100, 500)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.RecordCDKIssue(ctx, pid, pub, ""); err != nil {
			t.Fatal(err)
		}
	}
	mk(nodeA, "alpha", "pub-a1")
	mk(nodeA, "beta", "pub-a2")
	mk(nodeB, "gamma", "pub-b1")
	// nodeA 上已经撤销过的一张：不该被重复计数，也不该改撤销时间。
	if err := st.RevokeCDKIssue(ctx, "pub-a2"); err != nil {
		t.Fatal(err)
	}

	n, err := st.RevokeNodeCDKs(ctx, nodeA)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应只撤销 nodeA 上仍有效的那 1 张，got %d", n)
	}

	issues, err := st.ListCDKIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range issues {
		revoked := is.RevokedAt != nil
		want := is.PublicID != "pub-b1" // 另一台节点的必须完好
		if revoked != want {
			t.Errorf("%s: revoked=%v, want %v（撤销不能越过节点边界）", is.PublicID, revoked, want)
		}
	}

	// 幂等：再跑一次没有可撤的了。
	if n2, err := st.RevokeNodeCDKs(ctx, nodeA); err != nil || n2 != 0 {
		t.Errorf("重复撤销应为0条，got %d, %v", n2, err)
	}
}
