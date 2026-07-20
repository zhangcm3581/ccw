package store

import (
	"context"
	"embed"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"ccw/internal/auth"
	"ccw/internal/project"
)

//go:embed migrations
var migrationsFS embed.FS // migration唯一源就在本包的migrations/目录，禁止在仓库其他位置复制第二份

type Store struct{ Pool *pgxpool.Pool }

// New连接数据库并立即Ping；失败返回错误，调用方（main）必须以非零码退出。
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Migrate：schema_migrations记录已执行迁移，每个迁移文件只执行一次。
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		var done bool
		if err := s.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, n).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + n)
		if err != nil {
			return err
		}
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("store: migrate %s: %w", n, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, n); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ResolveCDK实现project.Resolver：按public-id做O(1)查询，再验证secret哈希。
func (s *Store) ResolveCDK(ctx context.Context, plain string) (project.Project, error) {
	pub, secret, err := auth.SplitCDK(plain)
	if err != nil {
		return project.Project{}, project.ErrInvalidCDK
	}
	var hash string
	var p project.Project
	err = s.Pool.QueryRow(ctx, `
		SELECT c.secret_hash, p.id, p.account_id, p.slug, p.container_name,
		       p.disk_limit_bytes, p.five_hour_limit, p.seven_day_limit
		FROM cdks c JOIN projects p ON p.id = c.project_id
		WHERE c.public_id = $1
		  AND c.disabled_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > now())`, pub).
		Scan(&hash, &p.ID, &p.AccountID, &p.Slug, &p.ContainerName,
			&p.DiskLimit, &p.FiveHourLimit, &p.SevenDayLimit)
	if err != nil || !auth.VerifySecret(secret, hash) {
		return project.Project{}, project.ErrInvalidCDK
	}
	return p, nil
}

// GetProjectByID：session claims里的project ID一律经此查库（control-api不保存进程内会话状态）。
func (s *Store) GetProjectByID(ctx context.Context, id string) (project.Project, error) {
	var p project.Project
	err := s.Pool.QueryRow(ctx, `
		SELECT id, account_id, slug, container_name, disk_limit_bytes, five_hour_limit, seven_day_limit
		FROM projects WHERE id = $1`, id).
		Scan(&p.ID, &p.AccountID, &p.Slug, &p.ContainerName, &p.DiskLimit, &p.FiveHourLimit, &p.SevenDayLimit)
	if err != nil {
		return project.Project{}, fmt.Errorf("store: project %s not found", id)
	}
	return p, nil
}
