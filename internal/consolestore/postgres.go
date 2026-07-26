// Package consolestore是Console独立库的存储层（console-fleet-design §2.1、§10）。
//
// Console有自己的PostgreSQL，只存机队元数据；节点保留各自的完整栈与本地库。
// 本包的migrations/是Console库迁移的唯一一份源——与internal/store/migrations/
// （节点库）是两个数据库的两套schema，无交集，不是同一套迁移的拷贝。
package consolestore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations
var migrationsFS embed.FS

// ErrNotFound是"查无此行"的哨兵；/connect查询用它统一返回「未找到」。
var ErrNotFound = errors.New("consolestore: not found")

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

type Store struct{ Pool *pgxpool.Pool }

// New连接Console库并立即Ping；失败由调用方以非零码退出。
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("consolestore: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("consolestore: ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Migrate与节点库同一套机制：schema_migrations表记录已执行迁移，每个文件只执行一次。
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
			return fmt.Errorf("consolestore: migrate %s: %w", n, err)
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
