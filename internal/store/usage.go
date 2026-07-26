package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"ccw/internal/usage"
)

// isNoRows：把"查无此行"与真实故障区分开。
// 混淆两者会让读取失败被当成"游标为0"，导致整个文件被重扫、用量重复计算——
// 靠Insert的幂等性虽不会算错总量，但会掩盖数据库故障。
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// 用量写入端：让*Store同时实现usage.Sink与usage.OffsetStore。
// 读取端（WindowUsed/PoolUsed，即quota.UsageReader）在queries.go。

// Insert实现usage.Sink。
//
// 幂等键是usage_events的UNIQUE (project_id, source_event_id)：同一requestId重复采集
// 不会新增行。冲突时按GREATEST逐字段取最大值而不是DO NOTHING——同一requestId的后续
// 记录可能带更大的token数（流式响应期间多次落盘），取最终值等价于取最大值。
// 依据：internal/usage/collector.go的Sink注释与docs/phase1-evidence/jsonl-semantics.md。
//
// weighted由调用方用usage.Weighted算好传入，这里不重算——权重是部署期配置，
// 不属于存储层的职责。
func (s *Store) Insert(ctx context.Context, projectID string, e usage.Event, weighted int64) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO usage_events (project_id, occurred_at, model,
		    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		    weighted_units, source_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (project_id, source_event_id) DO UPDATE SET
		  input_tokens       = GREATEST(usage_events.input_tokens,       EXCLUDED.input_tokens),
		  output_tokens      = GREATEST(usage_events.output_tokens,      EXCLUDED.output_tokens),
		  cache_read_tokens  = GREATEST(usage_events.cache_read_tokens,  EXCLUDED.cache_read_tokens),
		  cache_write_tokens = GREATEST(usage_events.cache_write_tokens, EXCLUDED.cache_write_tokens),
		  weighted_units     = GREATEST(usage_events.weighted_units,     EXCLUDED.weighted_units),
		  occurred_at        = EXCLUDED.occurred_at,
		  model              = EXCLUDED.model`,
		projectID, e.OccurredAt, e.Model,
		e.Input, e.Output, e.CacheRead, e.CacheWrite,
		weighted, e.SourceEventID)
	return err
}

// Load实现usage.OffsetStore：无记录时返回零游标（从文件头开始）。
func (s *Store) Load(ctx context.Context, projectID, fileIdentity string) (int64, string, error) {
	var offset int64
	var partial string
	err := s.Pool.QueryRow(ctx, `
		SELECT committed_offset, partial_line FROM usage_offsets
		WHERE project_id=$1 AND file_identity=$2`, projectID, fileIdentity).
		Scan(&offset, &partial)
	if err != nil {
		if isNoRows(err) {
			return 0, "", nil // 首次见到该文件：从头读
		}
		return 0, "", err
	}
	return offset, partial, nil
}

// Save实现usage.OffsetStore。
//
// 采集器只在成功写入Sink之后才调用它（collector.go:188），因此"游标已保存"
// 蕴含"该位置之前的事件已入库"。反过来Sink失败时游标不前进，下轮从同一位置
// 重扫，靠Insert的幂等性兜底。
func (s *Store) Save(ctx context.Context, projectID, fileIdentity, path string, offset int64, partial string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO usage_offsets (project_id, file_identity, path, committed_offset, partial_line)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (project_id, file_identity) DO UPDATE SET
		  path = EXCLUDED.path,
		  committed_offset = EXCLUDED.committed_offset,
		  partial_line = EXCLUDED.partial_line`,
		projectID, fileIdentity, path, offset, partial)
	return err
}
