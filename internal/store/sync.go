package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	syncpkg "ccw/internal/sync"
)

// *Store 实现 sync.RevisionStore：基于 file_index 表的权威同步状态。
// 真实数据库行为由 tests/e2e 验证（本机无 PostgreSQL 时仅编译）；
// 该处断言目前仍是 skip 状态，见 docs/STATUS.md 的 P1-3。

func (s *Store) Current(ctx context.Context, projectID, path string) (int64, int64, bool, error) {
	var rev, size int64
	err := s.Pool.QueryRow(ctx,
		`SELECT server_revision, size_bytes FROM file_index WHERE project_id=$1 AND path=$2`,
		projectID, path).Scan(&rev, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return rev, size, true, nil
}

func (s *Store) Manifest(ctx context.Context, projectID string) ([]syncpkg.FileEntry, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT path, size_bytes, sha256, server_revision, deleted FROM file_index WHERE project_id=$1`,
		projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []syncpkg.FileEntry
	for rows.Next() {
		var e syncpkg.FileEntry
		if err := rows.Scan(&e.Path, &e.Size, &e.SHA256, &e.Revision, &e.Deleted); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Commit：写入/更新 file_index 行到给定 revision。SyncSession 已在应用层做 CAS，
// worker 侧再以项目级锁串行化同一项目的写，避免并发 TOCTOU。
func (s *Store) Commit(ctx context.Context, projectID string, e syncpkg.FileEntry, device string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO file_index (project_id, path, size_bytes, sha256, server_revision, deleted, updated_by_device, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now())
		ON CONFLICT (project_id, path) DO UPDATE SET
		  size_bytes = EXCLUDED.size_bytes, sha256 = EXCLUDED.sha256,
		  server_revision = EXCLUDED.server_revision, deleted = EXCLUDED.deleted,
		  updated_by_device = EXCLUDED.updated_by_device, updated_at = now()`,
		projectID, e.Path, e.Size, e.SHA256, e.Revision, e.Deleted, device)
	return err
}

func (s *Store) TotalSize(ctx context.Context, projectID string) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(size_bytes),0) FROM file_index WHERE project_id=$1 AND NOT deleted`,
		projectID).Scan(&n)
	return n, err
}

// PurgeWorkspace硬删除一个工作区的全部索引行，返回释放的字节数。
//
// **硬删除而不是写墓碑**（sync/workspaces.go 有完整理由）：墓碑会把"这个文件
// 被删了"同步给用户的其他设备，把他们本地的文件也删掉；而删云端副本要的恰恰
// 相反——本地原样保留，下次连上来重新上传。
//
// ws 由调用方保证已过 ValidWorkspace（只有小写字母、数字与连字符），
// 因此不含 LIKE 的通配符；这里仍然用参数化查询，不拼字符串。
func (s *Store) PurgeWorkspace(ctx context.Context, projectID, ws string) (int64, error) {
	var freed int64
	err := s.Pool.QueryRow(ctx, `
		WITH gone AS (
			DELETE FROM file_index
			WHERE project_id = $1 AND path LIKE $2 || '/%'
			RETURNING size_bytes, deleted
		)
		SELECT COALESCE(SUM(size_bytes) FILTER (WHERE NOT deleted), 0) FROM gone`,
		projectID, ws).Scan(&freed)
	return freed, err
}
