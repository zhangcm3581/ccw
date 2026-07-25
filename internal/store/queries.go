package store

import (
	"context"
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

func (s *Store) PoolUsed(ctx context.Context, accountID string, since time.Time) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(u.weighted_units),0) FROM usage_events u
		JOIN projects p ON p.id = u.project_id
		WHERE p.account_id=$1 AND u.occurred_at > $2`, accountID, since).Scan(&n)
	return n, err
}
