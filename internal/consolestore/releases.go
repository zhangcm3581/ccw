package consolestore

import (
	"context"
	"time"
)

// Release与Artifact对应releases/release_artifacts表（设计§10）。
// 下载页从表渲染、不扫目录——`published_at IS NULL`的版本对下载面完全不可见，
// 这是"半成品文件不可被下载"的保证（设计§3.2）。
type Release struct {
	Version     string
	Notes       string
	PublishedAt *time.Time
	CreatedAt   time.Time
}

type Artifact struct {
	Version   string
	OS        string
	Arch      string
	Filename  string
	SizeBytes int64
	SHA256    string
}

// RegisterRelease登记（或重建）一个版本的产物集合：同版本重复注册＝
// 删旧产物行、插入新集合（重新构建后sha必然变化，upsert局部字段会留下幽灵行）。
// published_at不受注册动作影响——发布状态只由Publish推进。
func (s *Store) RegisterRelease(ctx context.Context, version, notes string, arts []Artifact) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO releases (version, notes) VALUES ($1, $2)
		ON CONFLICT (version) DO UPDATE SET notes = EXCLUDED.notes`, version, notes); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM release_artifacts WHERE version = $1`, version); err != nil {
		return err
	}
	for _, a := range arts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO release_artifacts (version, os, arch, filename, size_bytes, sha256)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			version, a.OS, a.Arch, a.Filename, a.SizeBytes, a.SHA256); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Publish把版本置为已发布（幂等：已发布的不改时间戳）；未知版本返回ErrNotFound。
func (s *Store) Publish(ctx context.Context, version string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE releases SET published_at = COALESCE(published_at, now()) WHERE version = $1`, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LatestPublished返回最近发布的版本及其产物（按os/arch排序，输出稳定）。
// 没有任何已发布版本时返回ErrNotFound——下载页据此显示「暂无发布」。
func (s *Store) LatestPublished(ctx context.Context) (Release, []Artifact, error) {
	var r Release
	err := s.Pool.QueryRow(ctx, `
		SELECT version, notes, published_at, created_at FROM releases
		WHERE published_at IS NOT NULL
		ORDER BY published_at DESC, version DESC LIMIT 1`).
		Scan(&r.Version, &r.Notes, &r.PublishedAt, &r.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return Release{}, nil, ErrNotFound
		}
		return Release{}, nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT version, os, arch, filename, size_bytes, sha256 FROM release_artifacts
		WHERE version = $1 ORDER BY os, arch`, r.Version)
	if err != nil {
		return Release{}, nil, err
	}
	defer rows.Close()
	var arts []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.Version, &a.OS, &a.Arch, &a.Filename, &a.SizeBytes, &a.SHA256); err != nil {
			return Release{}, nil, err
		}
		arts = append(arts, a)
	}
	return r, arts, rows.Err()
}

// ArtifactByFilename按文件名查产物，**只查已发布版本**——/dist的服务边界：
// 未发布或未知文件一律ErrNotFound，磁盘上有文件也不发。
func (s *Store) ArtifactByFilename(ctx context.Context, filename string) (Artifact, error) {
	var a Artifact
	err := s.Pool.QueryRow(ctx, `
		SELECT ra.version, ra.os, ra.arch, ra.filename, ra.size_bytes, ra.sha256
		FROM release_artifacts ra JOIN releases r ON r.version = ra.version
		WHERE ra.filename = $1 AND r.published_at IS NOT NULL`, filename).
		Scan(&a.Version, &a.OS, &a.Arch, &a.Filename, &a.SizeBytes, &a.SHA256)
	if err != nil {
		if isNoRows(err) {
			return Artifact{}, ErrNotFound
		}
		return Artifact{}, err
	}
	return a, nil
}
