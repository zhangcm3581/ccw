package store

import (
	"context"
	"time"

	"ccw/internal/project"
)

// StatusProject是ccwadmin status --json的每项目行（console-fleet-design §11.1）。
// LastUsageEventAt是发现"采集停摆"的关键信号：项目明明在用、这个时间却停在几小时前，
// 说明采集链路断了——这正是接线前那种"日志正常、usage_events为空"的静默失败形态。
type StatusProject struct {
	project.Project
	DiskUsed         int64
	ActiveCDKs       int
	LastUsageEventAt *time.Time
}

// StatusProjects一次查询带出全部项目的状态面。
func (s *Store) StatusProjects(ctx context.Context) ([]StatusProject, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id, p.account_id, p.slug, p.container_name,
		       p.disk_limit_bytes, p.five_hour_limit, p.seven_day_limit,
		       COALESCE((SELECT SUM(f.size_bytes) FROM file_index f
		                 WHERE f.project_id = p.id AND NOT f.deleted), 0),
		       (SELECT count(*) FROM cdks c WHERE c.project_id = p.id
		          AND c.disabled_at IS NULL
		          AND (c.expires_at IS NULL OR c.expires_at > now())),
		       (SELECT max(u.occurred_at) FROM usage_events u WHERE u.project_id = p.id)
		FROM projects p ORDER BY p.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatusProject
	for rows.Next() {
		var r StatusProject
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Slug, &r.ContainerName,
			&r.DiskLimit, &r.FiveHourLimit, &r.SevenDayLimit,
			&r.DiskUsed, &r.ActiveCDKs, &r.LastUsageEventAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SchemaMigrations返回已执行迁移的文件名（升序）：status --json用它报告schema版本，
// Console巡检据此判断节点代码/库结构是否落后。
func (s *Store) SchemaMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
