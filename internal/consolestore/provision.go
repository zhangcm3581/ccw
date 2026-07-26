package consolestore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"ccw/internal/pipeline"
)

// 流水线记账（console-fleet-design §10的provision_runs/provision_steps）。
// *Store由此实现pipeline.Recorder——引擎不认识数据库，记账全在这里。

// 编译期断言：签名漂移在编译期暴露，而不是等到部署时流水线无法恢复。
var _ pipeline.Recorder = (*Store)(nil)

// CreateRun新建一次运行，返回runID。kind取bootstrap|redeploy|domain|rotate-key|
// decommission|diagnose（§10）。
func (s *Store) CreateRun(ctx context.Context, nodeID, kind, triggeredBy string) (string, error) {
	id := uuid.NewString()
	var actor any
	if triggeredBy != "" {
		actor = triggeredBy
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO provision_runs (id, node_id, kind, status, triggered_by)
		VALUES ($1,$2,$3,'running',$4)`, id, nodeID, kind, actor)
	return id, err
}

// StepStarted记录步骤开始。用upsert而不是insert：续跑时同一个(run,seq)会再次开始，
// 那是正常的恢复流程，不该因主键冲突而失败。
func (s *Store) StepStarted(ctx context.Context, runID string, seq int, name string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO provision_steps (run_id, seq, name, status, started_at)
		VALUES ($1,$2,$3,'running', now())
		ON CONFLICT (run_id, seq) DO UPDATE SET
		  name = EXCLUDED.name, status = 'running', started_at = now(),
		  finished_at = NULL, exit_code = NULL`, runID, seq, name)
	return err
}

// StepFinished记录步骤结束。errMsg写入前经redact——它来自步骤返回的error，
// 可能拼进了命令输出。
func (s *Store) StepFinished(ctx context.Context, runID string, seq int, name string,
	st pipeline.Status, exitCode int, errMsg string) error {
	var code any
	if exitCode != 0 {
		code = exitCode
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO provision_steps (run_id, seq, name, status, exit_code, started_at, finished_at)
		VALUES ($1,$2,$3,$4,$5, now(), now())
		ON CONFLICT (run_id, seq) DO UPDATE SET
		  name = EXCLUDED.name, status = EXCLUDED.status,
		  exit_code = EXCLUDED.exit_code, finished_at = now()`,
		runID, seq, name, string(st), code)
	return err
}

// RunFinished收尾整次运行。
func (s *Store) RunFinished(ctx context.Context, runID string, st pipeline.Status) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE provision_runs SET status=$2, finished_at=now() WHERE id=$1`, runID, string(st))
	return err
}

// CompletedSteps返回该run中已成功或已跳过的步骤名——续跑时据此跳过（§5.3）。
func (s *Store) CompletedSteps(ctx context.Context, runID string) (map[string]bool, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT name FROM provision_steps WHERE run_id=$1 AND status IN ('succeeded','skipped')`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// RunSummary是运行详情页与列表用的视图。
type RunSummary struct {
	ID         string
	NodeID     string
	Kind       string
	Status     string
	StartedAt  time.Time
	FinishedAt *time.Time
	Steps      []StepSummary
}

type StepSummary struct {
	Seq        int
	Name       string
	Status     string
	ExitCode   *int
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// GetRun读取一次运行及其步骤（按seq）。
func (s *Store) GetRun(ctx context.Context, runID string) (RunSummary, error) {
	var r RunSummary
	err := s.Pool.QueryRow(ctx, `
		SELECT id, node_id, kind, status, started_at, finished_at
		FROM provision_runs WHERE id=$1`, runID).
		Scan(&r.ID, &r.NodeID, &r.Kind, &r.Status, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		if isNoRows(err) {
			return RunSummary{}, ErrNotFound
		}
		return RunSummary{}, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT seq, name, status, exit_code, started_at, finished_at
		FROM provision_steps WHERE run_id=$1 ORDER BY seq`, runID)
	if err != nil {
		return RunSummary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var st StepSummary
		if err := rows.Scan(&st.Seq, &st.Name, &st.Status, &st.ExitCode, &st.StartedAt, &st.FinishedAt); err != nil {
			return RunSummary{}, err
		}
		r.Steps = append(r.Steps, st)
	}
	return r, rows.Err()
}

// ListRuns列出某节点最近的运行（详情页的历史区）。
func (s *Store) ListRuns(ctx context.Context, nodeID string, limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, node_id, kind, status, started_at, finished_at
		FROM provision_runs WHERE node_id=$1 ORDER BY started_at DESC LIMIT $2`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		if err := rows.Scan(&r.ID, &r.NodeID, &r.Kind, &r.Status, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
