package store

import (
	"context"

	"github.com/google/uuid"

	"ccw/internal/auth"
)

// EnsureAccount返回同名account的id，不存在则创建。
func (s *Store) EnsureAccount(ctx context.Context, name, upstreamPool string) (string, error) {
	var id string
	if err := s.Pool.QueryRow(ctx, `SELECT id FROM accounts WHERE name=$1`, name).Scan(&id); err == nil {
		return id, nil
	}
	id = uuid.NewString()
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO accounts (id, name, upstream_pool) VALUES ($1,$2,$3)`, id, name, upstreamPool)
	return id, err
}

// CreateProject建立项目行并返回其id。containerName是worker-agent附着终端时使用的容器名。
func (s *Store) CreateProject(ctx context.Context, accountID, slug, containerName string,
	diskLimit, fiveHour, sevenDay int64) (string, error) {
	id := uuid.NewString()
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO projects (id, account_id, slug, container_name, disk_limit_bytes, five_hour_limit, seven_day_limit)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, accountID, slug, containerName, diskLimit, fiveHour, sevenDay)
	return id, err
}

// CreateCDK为项目签发一张新CDK，返回一次性显示的明文；库中只存Argon2id哈希。
func (s *Store) CreateCDK(ctx context.Context, projectID string) (string, error) {
	plain, publicID, err := auth.NewCDK()
	if err != nil {
		return "", err
	}
	_, secret, err := auth.SplitCDK(plain)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		return "", err
	}
	_, err = s.Pool.Exec(ctx,
		`INSERT INTO cdks (id, project_id, public_id, secret_hash) VALUES ($1,$2,$3,$4)`,
		uuid.NewString(), projectID, publicID, hash)
	if err != nil {
		return "", err
	}
	return plain, nil
}
