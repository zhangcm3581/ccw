package store

import (
	"ccw/internal/quota"
	"context"
	"errors"
	"time"

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

// UsageTotals是一个时间窗口内的真实用量汇总。
//
// **token 数是真实的**——直接来自 Claude Code 写的会话 JSONL，逐条累加，
// 没有任何估算。WeightedUnits 才是本仓库自己算的"内部额度单位"
// （token × CCW_USAGE_WEIGHTS），闸门用它，但它是估算口径，
// 不等于 Claude 账号的真实消耗（spec §10）。
type UsageTotals struct {
	Events     int64 `json:"events"`
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Weighted   int64 `json:"weighted_units"`
}

// ModelUsage是按模型拆分的用量。贵的模型和便宜的模型混在一起看不出问题，
// 而"这周 Opus 用了多少"恰恰是最该知道的一条。
type ModelUsage struct {
	Model string `json:"model"`
	UsageTotals
}

// ProjectUsage是一个项目的用量概览。
type ProjectUsage struct {
	Slug        string       `json:"slug"`
	ProjectID   string       `json:"project_id"`
	FiveHour    UsageTotals  `json:"five_hour"`
	SevenDay    UsageTotals  `json:"seven_day"`
	Total       UsageTotals  `json:"total"`
	ByModel     []ModelUsage `json:"by_model"` // 7天窗口内
	FiveHourLim int64        `json:"five_hour_limit"`
	SevenDayLim int64        `json:"seven_day_limit"`
	// LastEventAt是最后一条用量的时间。**它是判断采集有没有在工作的唯一依据**：
	// 空值或很久以前，说明 JSONL 没被扫到（最常见是只读挂载漏了），
	// 那时限额设多少都没有意义。
	LastEventAt *time.Time `json:"last_event_at"`
	// PoolFiveHour/PoolSevenDay是这个账号整个窗口的容量（内部单位）。
	// 档位限额 = 比例 × 它，界面上要能把"2x 等于多少单位"直接摆出来——
	// 否则管理员只能指派完看结果，而指派会立刻杀掉活跃会话。
	PoolFiveHour int64  `json:"pool_five_hour"`
	PoolSevenDay int64  `json:"pool_seven_day"`
	Tier         string `json:"tier"`
	// TierEffective为true表示限额确实是按档位算出来的。
	// 挂了档位但账号容量未校准时它是 false——那时闸门沿用绝对限额，
	// 页面要说清楚，否则"挂了却没变化"看不出原因。
	TierEffective bool `json:"tier_effective"`
}

// ProjectUsageReport汇总全部项目的用量。
//
// 窗口边界用**数据库的 now()**（CLAUDE.md：所有时间窗口用数据库now()与UTC），
// 不接受调用方传时间——巡检机器的时钟不可信。
func (s *Store) ProjectUsageReport(ctx context.Context) ([]ProjectUsage, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.slug, p.id::text, p.five_hour_limit, p.seven_day_limit,
		       a.pool_five_hour_limit, a.pool_seven_day_limit, COALESCE(p.tier, ''),
		       COALESCE(t.share_bp, 0),
		       COALESCE(SUM(u.input_tokens)       FILTER (WHERE u.occurred_at >= now() - interval '5 hours'), 0),
		       COALESCE(SUM(u.output_tokens)      FILTER (WHERE u.occurred_at >= now() - interval '5 hours'), 0),
		       COALESCE(SUM(u.cache_read_tokens)  FILTER (WHERE u.occurred_at >= now() - interval '5 hours'), 0),
		       COALESCE(SUM(u.cache_write_tokens) FILTER (WHERE u.occurred_at >= now() - interval '5 hours'), 0),
		       COALESCE(SUM(u.weighted_units)     FILTER (WHERE u.occurred_at >= now() - interval '5 hours'), 0),
		       COUNT(u.id)                        FILTER (WHERE u.occurred_at >= now() - interval '5 hours'),
		       COALESCE(SUM(u.input_tokens)       FILTER (WHERE u.occurred_at >= now() - interval '7 days'), 0),
		       COALESCE(SUM(u.output_tokens)      FILTER (WHERE u.occurred_at >= now() - interval '7 days'), 0),
		       COALESCE(SUM(u.cache_read_tokens)  FILTER (WHERE u.occurred_at >= now() - interval '7 days'), 0),
		       COALESCE(SUM(u.cache_write_tokens) FILTER (WHERE u.occurred_at >= now() - interval '7 days'), 0),
		       COALESCE(SUM(u.weighted_units)     FILTER (WHERE u.occurred_at >= now() - interval '7 days'), 0),
		       COUNT(u.id)                        FILTER (WHERE u.occurred_at >= now() - interval '7 days'),
		       COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0),
		       COALESCE(SUM(u.cache_read_tokens), 0), COALESCE(SUM(u.cache_write_tokens), 0),
		       COALESCE(SUM(u.weighted_units), 0), COUNT(u.id),
		       MAX(u.occurred_at)
		FROM projects p
		JOIN accounts a ON a.id = p.account_id
		LEFT JOIN quota_tiers t ON t.name = p.tier
		LEFT JOIN usage_events u ON u.project_id = p.id
		GROUP BY p.id, p.slug, p.five_hour_limit, p.seven_day_limit,
		         a.pool_five_hour_limit, a.pool_seven_day_limit, p.tier, t.share_bp
		ORDER BY p.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProjectUsage
	for rows.Next() {
		var u ProjectUsage
		var shareBP int
		if err := rows.Scan(&u.Slug, &u.ProjectID, &u.FiveHourLim, &u.SevenDayLim,
			&u.PoolFiveHour, &u.PoolSevenDay, &u.Tier, &shareBP,
			&u.FiveHour.Input, &u.FiveHour.Output, &u.FiveHour.CacheRead, &u.FiveHour.CacheWrite,
			&u.FiveHour.Weighted, &u.FiveHour.Events,
			&u.SevenDay.Input, &u.SevenDay.Output, &u.SevenDay.CacheRead, &u.SevenDay.CacheWrite,
			&u.SevenDay.Weighted, &u.SevenDay.Events,
			&u.Total.Input, &u.Total.Output, &u.Total.CacheRead, &u.Total.CacheWrite,
			&u.Total.Weighted, &u.Total.Events, &u.LastEventAt); err != nil {
			return nil, err
		}
		// **报的是实际生效的限额，不是 projects 表里那一列。**
		// 档位是闸门在运行时算的（quota.ApplyTier），从不写回该列——
		// 直接报那一列会让页面显示一个根本没被用到的数字，
		// 而用户据此判断"档位没生效"（2026-08-02 真机上就是这样）。
		abs := quota.Limits{
			FiveHour: u.FiveHourLim, SevenDay: u.SevenDayLim,
			PoolFiveHour: u.PoolFiveHour, PoolSevenDay: u.PoolSevenDay,
		}
		lim := quota.ApplyTier(abs, shareBP, u.Tier != "")
		// 档位真正被套用时 ApplyTier 会改掉限额；容量未校准（或会溢出）时它
		// 原样返回，那就说明档位挂着但没生效。用"变没变"判断，而不是把
		// ApplyTier 的判定条件在这里再抄一遍——抄一遍就会有两套规则。
		u.TierEffective = lim.FiveHour != abs.FiveHour || lim.SevenDay != abs.SevenDay
		u.FiveHourLim, u.SevenDayLim = lim.FiveHour, lim.SevenDay
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		m, err := s.usageByModel(ctx, out[i].ProjectID)
		if err != nil {
			return nil, err
		}
		out[i].ByModel = m
	}
	return out, nil
}

// usageByModel取7天窗口内按模型拆分的用量，用量大的在前。
func (s *Store) usageByModel(ctx context.Context, projectID string) ([]ModelUsage, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT model, COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		       COALESCE(SUM(weighted_units),0), COUNT(*)
		FROM usage_events
		WHERE project_id = $1 AND occurred_at >= now() - interval '7 days'
		GROUP BY model
		ORDER BY SUM(output_tokens) DESC, model`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelUsage
	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Input, &m.Output, &m.CacheRead, &m.CacheWrite,
			&m.Weighted, &m.Events); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
