// Package pipeline是Console的流水线引擎（console-fleet-design §5.3、C7）。
//
// 每个步骤必须满足三条性质，这是引擎的正确性定义：
//
//	幂等   —— 重复执行结果相同
//	可precheck —— 已满足则跳过并标记skipped（而不是重做一遍）
//	可续跑 —— 从第一个失败步重开，已完成的步骤不重复执行
//
// 引擎不认识SSH、Docker或DNS：步骤只是一个函数。这样bootstrap、重新部署、
// 解除纳管都能复用同一套记账与恢复逻辑，也让引擎本身能用假步骤做单测。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Status是步骤与运行的状态，取值与provision_steps/provision_runs的CHECK一致。
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusSkipped   Status = "skipped"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Logf是步骤向实时日志输出的出口。**实现方负责脱敏**——
// sshexec.Run/Stream已在源头脱敏，步骤自己拼的字符串要自己过redact。
type Logf func(format string, a ...any)

// Step是一个流水线步骤。
type Step struct {
	Name string
	// Precheck报告"已经满足"（true＝跳过Run）。为nil表示总是执行。
	// 返回error时视为precheck失败：**当作未满足继续执行Run**，而不是让整条流水线失败——
	// precheck只是优化，它自己坏了不该阻断部署。
	Precheck func(ctx context.Context, log Logf) (bool, error)
	// Run执行步骤。返回error即步骤失败，流水线在此中止（后续步骤保持pending）。
	Run func(ctx context.Context, log Logf) error
	// Timeout是本步骤的超时；0取Runner.DefaultTimeout。
	Timeout time.Duration
}

// Recorder是引擎的记账出口（生产实现写provision_runs/provision_steps）。
// 每个回调的error都会中止流水线：**没有记账的执行等于没有可恢复性**，
// 与审计写入失败即动作失败是同一类要求。
type Recorder interface {
	StepStarted(ctx context.Context, runID string, seq int, name string) error
	StepFinished(ctx context.Context, runID string, seq int, name string, st Status, exitCode int, errMsg string) error
	RunFinished(ctx context.Context, runID string, st Status) error
	// CompletedSteps返回该run中已成功/已跳过的步骤名（续跑时用于跳过）。
	CompletedSteps(ctx context.Context, runID string) (map[string]bool, error)
}

// Runner执行一组步骤。
type Runner struct {
	Recorder Recorder
	// Log是全局日志出口；引擎会把步骤名前缀加好再传给步骤。
	Log Logf
	// DefaultTimeout是未指定Timeout的步骤的上限（0＝不限）。
	DefaultTimeout time.Duration
}

// ErrStepFailed包装步骤失败，便于调用方拿到失败的步骤名。
type ErrStepFailed struct {
	Step string
	Err  error
}

func (e *ErrStepFailed) Error() string { return fmt.Sprintf("步骤%s失败: %v", e.Step, e.Err) }
func (e *ErrStepFailed) Unwrap() error { return e.Err }

// Run执行整条流水线。
//
// 续跑语义：先读CompletedSteps，已完成的步骤直接跳过（不重跑、不再记账）。
// 因此"从失败步重开"就是用**同一个runID**再调一次Run（§5.3、A9）。
//
// 失败即停：后续步骤保持pending而不是标记failed——它们没有执行过，
// 把没跑过的东西记成失败会让恢复时无法区分"真失败"与"没轮到"。
func (r *Runner) Run(ctx context.Context, runID string, steps []Step) error {
	log := r.Log
	if log == nil {
		log = func(string, ...any) {}
	}

	done, err := r.Recorder.CompletedSteps(ctx, runID)
	if err != nil {
		return fmt.Errorf("pipeline: 读取已完成步骤失败: %w", err)
	}

	for i, s := range steps {
		seq := i + 1
		if done[s.Name] {
			log("[%d/%d] %s：此前已完成，跳过", seq, len(steps), s.Name)
			continue
		}
		if err := ctx.Err(); err != nil {
			// 取消：把run标记为cancelled，未执行的步骤保持pending。
			if rerr := r.Recorder.RunFinished(ctx, runID, StatusCancelled); rerr != nil {
				return fmt.Errorf("pipeline: 记账失败: %w", rerr)
			}
			return err
		}

		stepLog := func(format string, a ...any) { log("["+s.Name+"] "+format, a...) }
		if err := r.Recorder.StepStarted(ctx, runID, seq, s.Name); err != nil {
			return fmt.Errorf("pipeline: 记账失败: %w", err)
		}
		log("[%d/%d] %s：开始", seq, len(steps), s.Name)

		status, exitCode, stepErr := r.runOne(ctx, s, stepLog)

		msg := ""
		if stepErr != nil {
			msg = stepErr.Error()
		}
		if err := r.Recorder.StepFinished(ctx, runID, seq, s.Name, status, exitCode, msg); err != nil {
			return fmt.Errorf("pipeline: 记账失败: %w", err)
		}

		switch status {
		case StatusSkipped:
			log("[%d/%d] %s：已满足，跳过", seq, len(steps), s.Name)
		case StatusSucceeded:
			log("[%d/%d] %s：完成", seq, len(steps), s.Name)
		default:
			log("[%d/%d] %s：失败——%v", seq, len(steps), s.Name, stepErr)
			if rerr := r.Recorder.RunFinished(ctx, runID, StatusFailed); rerr != nil {
				return fmt.Errorf("pipeline: 记账失败: %w", rerr)
			}
			return &ErrStepFailed{Step: s.Name, Err: stepErr}
		}
	}

	if err := r.Recorder.RunFinished(ctx, runID, StatusSucceeded); err != nil {
		return fmt.Errorf("pipeline: 记账失败: %w", err)
	}
	log("流水线完成（%d个步骤）", len(steps))
	return nil
}

// runOne执行单个步骤（含precheck与超时），返回状态、退出码与错误。
//
// panic隔离：一个步骤的意外panic不该带走整个Console进程——
// 它跑在HTTP请求或后台goroutine里，未recover的panic会终止整个进程。
func (r *Runner) runOne(ctx context.Context, s Step, log Logf) (status Status, exitCode int, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			status, exitCode, err = StatusFailed, -1, fmt.Errorf("panic: %v", rec)
		}
	}()

	timeout := s.Timeout
	if timeout == 0 {
		timeout = r.DefaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if s.Precheck != nil {
		ok, perr := s.Precheck(ctx, log)
		switch {
		case perr != nil:
			// precheck只是优化，它自己失败不该阻断部署——记一行日志后照常执行。
			log("precheck失败（按未满足处理，继续执行）：%v", perr)
		case ok:
			return StatusSkipped, 0, nil
		}
	}
	if s.Run == nil {
		return StatusSucceeded, 0, nil
	}
	if rerr := s.Run(ctx, log); rerr != nil {
		var ee *ExitError
		if errors.As(rerr, &ee) {
			return StatusFailed, ee.Code, rerr
		}
		return StatusFailed, -1, rerr
	}
	return StatusSucceeded, 0, nil
}

// ExitError让步骤把远端命令的退出码带回记账层（provision_steps.exit_code）。
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return fmt.Sprintf("退出码%d: %v", e.Code, e.Err) }
func (e *ExitError) Unwrap() error { return e.Err }
