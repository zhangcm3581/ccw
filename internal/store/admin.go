package store

import (
	"context"

	"github.com/google/uuid"

	"ccw/internal/auth"
	"ccw/internal/project"
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

// ListProjects返回全部项目，按slug排序。
//
// 用量采集必须遍历全部项目，而不是只遍历有活跃终端连接的项目：tmux会话在客户端
// 断开后继续运行，Claude照常消耗额度。只采活跃项目会丢掉无人值守期间的用量，
// 而闸门恰恰要在那时生效。见用量接线计划§2.3。
func (s *Store) ListProjects(ctx context.Context) ([]project.Project, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, account_id, slug, container_name, disk_limit_bytes, five_hour_limit, seven_day_limit
		FROM projects ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []project.Project
	for rows.Next() {
		var p project.Project
		if err := rows.Scan(&p.ID, &p.AccountID, &p.Slug, &p.ContainerName,
			&p.DiskLimit, &p.FiveHourLimit, &p.SevenDayLimit); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
