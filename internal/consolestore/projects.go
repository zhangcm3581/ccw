package consolestore

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// 节点上项目与CDK签发事件的镜像（console-fleet-design §10、§11.1）。
//
// **这两张表是镜像，权威在节点自己的库。**Console存它们只为两件事：
// `/connect`能把public-id解析成接入域名（§6.6），以及后台能在不登机的情况下
// 看清「哪台机器上有哪些项目、发过哪些CDK」。
//
// 铁律：**CDK明文与secret部分绝不到达这一层**（§8.4）。这里只存public_id——
// 它是设计上可公开的部分，`/connect`本来就用它做查询。

type NodeProject struct {
	ID              string
	NodeID          string
	NodeName        string // JOIN出来的，方便页面直接渲染
	Slug            string
	RemoteProjectID string
	DiskLimitBytes  int64
	FiveHourLimit   int64
	SevenDayLimit   int64
	SyncedAt        time.Time
}

type CDKIssue struct {
	ID        string
	ProjectID string // node_projects.id
	Slug      string
	NodeID    string
	NodeName  string
	PublicID  string
	IssuedBy  *string
	IssuedAt  time.Time
	RevokedAt *time.Time
}

// UpsertNodeProject登记（或更新）一个节点上的项目镜像。
//
// 幂等按(node_id, slug)：bootstrap重跑、后来又改了配额，都只更新同一行。
// 返回node_projects.id供RecordCDKIssue外键使用。
func (s *Store) UpsertNodeProject(ctx context.Context, nodeID, slug, remoteID string,
	diskBytes, fiveHour, sevenDay int64) (string, error) {
	id := uuid.NewString()
	var out string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO node_projects
			(id, node_id, slug, remote_project_id, disk_limit_bytes, five_hour_limit, seven_day_limit)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (node_id, slug) DO UPDATE SET
			remote_project_id = EXCLUDED.remote_project_id,
			disk_limit_bytes  = EXCLUDED.disk_limit_bytes,
			five_hour_limit   = EXCLUDED.five_hour_limit,
			seven_day_limit   = EXCLUDED.seven_day_limit,
			synced_at         = now()
		RETURNING id`,
		id, nodeID, slug, remoteID, diskBytes, fiveHour, sevenDay).Scan(&out)
	return out, err
}

// RecordCDKIssue记一次签发事件。
//
// public_id唯一：同一个CDK被重复上报（流水线重跑、同步对账）不produce重复行。
// issuedBy为空字符串时记NULL——bootstrap里签发的CDK由流水线触发，
// 触发者已记在provision_runs上，这里不强行attribution。
func (s *Store) RecordCDKIssue(ctx context.Context, projectID, publicID, issuedBy string) error {
	var by any
	if issuedBy != "" {
		by = issuedBy
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO cdk_issues (id, node_project_id, public_id, issued_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (public_id) DO NOTHING`,
		uuid.NewString(), projectID, publicID, by)
	return err
}

// RevokeCDKIssue标记某个public_id已撤销。幂等；已撤销的不改时间。
func (s *Store) RevokeCDKIssue(ctx context.Context, publicID string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE cdk_issues SET revoked_at = now()
		WHERE public_id = $1 AND revoked_at IS NULL`, publicID)
	return err
}

// ListNodeProjects列出全部节点的项目镜像（机队上限3台×3项目，无需分页）。
func (s *Store) ListNodeProjects(ctx context.Context) ([]NodeProject, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT np.id, np.node_id, n.name, np.slug, np.remote_project_id,
		       np.disk_limit_bytes, np.five_hour_limit, np.seven_day_limit, np.synced_at
		FROM node_projects np
		JOIN nodes n ON n.id = np.node_id
		ORDER BY n.name, np.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeProject
	for rows.Next() {
		var p NodeProject
		if err := rows.Scan(&p.ID, &p.NodeID, &p.NodeName, &p.Slug, &p.RemoteProjectID,
			&p.DiskLimitBytes, &p.FiveHourLimit, &p.SevenDayLimit, &p.SyncedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListCDKIssues列出全部签发记录，最近的在前。
func (s *Store) ListCDKIssues(ctx context.Context) ([]CDKIssue, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT ci.id, np.id, np.slug, np.node_id, n.name,
		       ci.public_id, ci.issued_by::text, ci.issued_at, ci.revoked_at
		FROM cdk_issues ci
		JOIN node_projects np ON np.id = ci.node_project_id
		JOIN nodes n          ON n.id = np.node_id
		ORDER BY ci.issued_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CDKIssue
	for rows.Next() {
		var c CDKIssue
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Slug, &c.NodeID, &c.NodeName,
			&c.PublicID, &c.IssuedBy, &c.IssuedAt, &c.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetNodeProject按id取一个项目镜像（签发/轮换前要知道它属于哪台节点）。
func (s *Store) GetNodeProject(ctx context.Context, id string) (NodeProject, error) {
	var p NodeProject
	err := s.Pool.QueryRow(ctx, `
		SELECT np.id, np.node_id, n.name, np.slug, np.remote_project_id,
		       np.disk_limit_bytes, np.five_hour_limit, np.seven_day_limit, np.synced_at
		FROM node_projects np
		JOIN nodes n ON n.id = np.node_id
		WHERE np.id = $1`, id).
		Scan(&p.ID, &p.NodeID, &p.NodeName, &p.Slug, &p.RemoteProjectID,
			&p.DiskLimitBytes, &p.FiveHourLimit, &p.SevenDayLimit, &p.SyncedAt)
	if err != nil {
		if isNoRows(err) {
			return NodeProject{}, ErrNotFound
		}
		return NodeProject{}, err
	}
	return p, nil
}
