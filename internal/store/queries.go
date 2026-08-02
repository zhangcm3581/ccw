package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	syncpkg "ccw/internal/sync"
)

// PGIndex实现storage.Index：file_index行写入与逻辑字节求和。
// 真实数据库行为由tests/e2e验证（本机无PostgreSQL时仅编译）；
// 该处断言目前仍是skip状态，见docs/STATUS.md的P1-3。
type PGIndex struct{ Pool *pgxpool.Pool }

func (x PGIndex) Upsert(ctx context.Context, projectID string, e syncpkg.FileEntry) error {
	_, err := x.Pool.Exec(ctx, `
		INSERT INTO file_index (project_id, path, size_bytes, sha256, server_revision, deleted, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6, now())
		ON CONFLICT (project_id, path) DO UPDATE SET
		  size_bytes = EXCLUDED.size_bytes, sha256 = EXCLUDED.sha256,
		  server_revision = EXCLUDED.server_revision, deleted = EXCLUDED.deleted, updated_at = now()`,
		projectID, e.Path, e.Size, e.SHA256, e.Revision, e.Deleted)
	return err
}

func (x PGIndex) DiskUsed(ctx context.Context, projectID string) (int64, error) {
	var n int64
	err := x.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(size_bytes),0) FROM file_index WHERE project_id=$1 AND NOT deleted`,
		projectID).Scan(&n)
	return n, err
}

// WindowUsed/PoolUsed让*Store实现quota.UsageReader。
func (s *Store) WindowUsed(ctx context.Context, projectID string, since time.Time) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(weighted_units),0) FROM usage_events WHERE project_id=$1 AND occurred_at > $2`,
		projectID, since).Scan(&n)
	return n, err
}

// AccountPoolLimits返回账号级池的双窗口上限（内部额度单位）。
//
// 这是"多个项目共用一个上游账号"时的第二道闸门：项目级限额防单个项目失控，
// 池级限额防各项目都没超限、加起来却把账号打爆。002迁移之前这两个值没有存储，
// worker只能写死极大值，池闸门从未生效。
func (s *Store) AccountPoolLimits(ctx context.Context, accountID string) (fiveHour, sevenDay int64, err error) {
	err = s.Pool.QueryRow(ctx,
		`SELECT pool_five_hour_limit, pool_seven_day_limit FROM accounts WHERE id=$1`, accountID).
		Scan(&fiveHour, &sevenDay)
	return fiveHour, sevenDay, err
}

func (s *Store) PoolUsed(ctx context.Context, accountID string, since time.Time) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(u.weighted_units),0) FROM usage_events u
		JOIN projects p ON p.id = u.project_id
		WHERE p.account_id=$1 AND u.occurred_at > $2`, accountID, since).Scan(&n)
	return n, err
}

// SetAccountPoolLimits写回账号池上限（自动校准的落点）。
//
// 这个值是全部档位的基准，所以**只由校准逻辑改**，而校准本身有防呆
// （见 internal/quota/calibrate.go）：百分比过低、累计量过小时一律不动，
// 有值时每次只朝估计值移动 20%。
func (s *Store) SetAccountPoolLimits(ctx context.Context, accountID string, fiveHour, sevenDay int64) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE accounts SET pool_five_hour_limit=$2, pool_seven_day_limit=$3 WHERE id=$1`,
		accountID, fiveHour, sevenDay)
	return err
}

// QuotaTier是一个额度档位。ShareBP是占账号池的比例，万分之一（1000=10%）。
type QuotaTier struct {
	Name    string `json:"name"`
	ShareBP int    `json:"share_bp"`
	Order   int    `json:"sort_order"`
}

// ListQuotaTiers列出全部档位。
func (s *Store) ListQuotaTiers(ctx context.Context) ([]QuotaTier, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT name, share_bp, sort_order FROM quota_tiers ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaTier
	for rows.Next() {
		var t QuotaTier
		if err := rows.Scan(&t.Name, &t.ShareBP, &t.Order); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTierShare改一个档位的百分比。
func (s *Store) SetTierShare(ctx context.Context, name string, shareBP int) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE quota_tiers SET share_bp=$2 WHERE name=$1`, name, shareBP)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: 档位%q不存在", name)
	}
	return nil
}

// SetProjectTier把项目挂到一个档位；name为空表示改回用绝对限额。
func (s *Store) SetProjectTier(ctx context.Context, slug string, name *string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE projects SET tier=$2 WHERE slug=$1`, slug, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: 项目%q不存在", slug)
	}
	return nil
}

// ProjectTierShare返回项目所属档位的比例（万分之一）。
// 没挂档位时返回 ok=false，调用方应沿用绝对限额。
func (s *Store) ProjectTierShare(ctx context.Context, projectID string) (int, bool, error) {
	var bp *int
	err := s.Pool.QueryRow(ctx, `
		SELECT t.share_bp FROM projects p
		LEFT JOIN quota_tiers t ON t.name = p.tier
		WHERE p.id = $1`, projectID).Scan(&bp)
	if err != nil {
		return 0, false, err
	}
	if bp == nil {
		return 0, false, nil
	}
	return *bp, true, nil
}
