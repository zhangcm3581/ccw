package store

import (
	"context"
	"errors"
	"time"

	"ccw/internal/project"
)

// ErrNotFound是"查无此行"的哨兵：调用方（ccwadmin）靠它把「项目不存在/CDK不存在/
// 已禁用」折叠成统一错误（console-fleet-design §11.1.1），同时保留基础设施错误
// （连接失败等）的真实信息——两类错误的处置完全不同，不能混为一谈。
var ErrNotFound = errors.New("store: not found")

// ProjectBySlug按slug查项目；查无返回ErrNotFound。
func (s *Store) ProjectBySlug(ctx context.Context, slug string) (project.Project, error) {
	var p project.Project
	err := s.Pool.QueryRow(ctx, `
		SELECT id, account_id, slug, container_name, disk_limit_bytes, five_hour_limit, seven_day_limit
		FROM projects WHERE slug = $1`, slug).
		Scan(&p.ID, &p.AccountID, &p.Slug, &p.ContainerName, &p.DiskLimit, &p.FiveHourLimit, &p.SevenDayLimit)
	if err != nil {
		if isNoRows(err) {
			return project.Project{}, ErrNotFound
		}
		return project.Project{}, err
	}
	return p, nil
}

// CountProjects返回项目总数：init-project用它强制单节点3个项目的产品上限（设计§7.6）。
// 注意check-then-create不是原子的——两个并发的init-project理论上可双双通过检查；
// 单管理员场景下接受该窗口，Console串行执行流水线时也不会并发建项目。
func (s *Store) CountProjects(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM projects`).Scan(&n)
	return n, err
}

// CDKInfo是list-cdks的行：只含public_id与状态，**绝不含明文或哈希**——
// 明文签发后不可再取（设计§11.1），哈希对外没有任何用途、只会扩大泄露面。
// Disabled/Expired由数据库判定（expired按数据库now()，CLAUDE.md时间窗口规则）。
type CDKInfo struct {
	PublicID   string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	DisabledAt *time.Time
	Disabled   bool
	Expired    bool
}

// ListCDKs列出项目全部CDK（含已失效的——对账与审计需要看到全貌）。
func (s *Store) ListCDKs(ctx context.Context, projectID string) ([]CDKInfo, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT public_id, created_at, expires_at, disabled_at,
		       disabled_at IS NOT NULL,
		       (expires_at IS NOT NULL AND expires_at <= now())
		FROM cdks WHERE project_id = $1 ORDER BY created_at, public_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CDKInfo
	for rows.Next() {
		var i CDKInfo
		if err := rows.Scan(&i.PublicID, &i.CreatedAt, &i.ExpiresAt, &i.DisabledAt, &i.Disabled, &i.Expired); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ExpireOtherProjectCDKs是宽限轮换（rotate-cdk --grace）的写入端：
// 项目内除exceptPublicID之外、当前仍有效的CDK，一律设expires_at=now()+grace。
//
// 两个刻意的语义：
//  1. LEAST——只缩短、不延长。已经更早过期的CDK不得因轮换被续命。
//  2. now()来自数据库（CLAUDE.md：所有时间窗口用数据库now()与UTC），
//     不接受调用方传入的绝对时间——客户端时钟不可信。
//
// 宽限到期后自动失效不需要任何定时任务：ResolveCDK每次查询都比对expires_at（§11.1.1）。
func (s *Store) ExpireOtherProjectCDKs(ctx context.Context, projectID, exceptPublicID string, graceSeconds int64) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE cdks
		SET expires_at = LEAST(COALESCE(expires_at, 'infinity'::timestamptz),
		                       now() + make_interval(secs => $3::double precision))
		WHERE project_id = $1 AND public_id <> $2
		  AND disabled_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())`,
		projectID, exceptPublicID, graceSeconds)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DisableOtherProjectCDKs是立即撤销（rotate-cdk --revoke-now）的写入端：
// 项目内除exceptPublicID之外、尚未禁用的CDK立即disabled_at=now()。
// 已禁用的行不重写（重复执行影响0行，幂等）。
func (s *Store) DisableOtherProjectCDKs(ctx context.Context, projectID, exceptPublicID string) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE cdks SET disabled_at = now()
		WHERE project_id = $1 AND public_id <> $2 AND disabled_at IS NULL`,
		projectID, exceptPublicID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DisableCDKByPublicID禁用单张CDK（disable-cdk子命令）。
// 未命中（不存在或已禁用）返回ErrNotFound，由调用方折叠成统一错误。
func (s *Store) DisableCDKByPublicID(ctx context.Context, publicID string) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE cdks SET disabled_at = now() WHERE public_id = $1 AND disabled_at IS NULL`, publicID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
