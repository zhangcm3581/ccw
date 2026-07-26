package consolestore

import "context"

// ResolveAPIDomain是/connect查询的后端（设计§6.6）：
// cdk_issues → node_projects → nodes → node_domains，全在Console库内完成，
// 不访问任何节点。查无、已撤销、域名已退役统一返回ErrNotFound——
// 调用方一律回「未找到」，不区分原因（延伸invalid_cdk统一错误规则）。
//
// 入参只能是CDK的public-id部分；secret绝不该到达这里（HTTP层已拒绝含"."的输入）。
func (s *Store) ResolveAPIDomain(ctx context.Context, publicID string) (string, error) {
	var fqdn string
	// 一个节点可能有多条域名记录（别名/迁移中）：优先insync，其次pending
	// （DNS尚未生效时把域名给用户仍然有用——生效后即可连）；退役记录排除。
	err := s.Pool.QueryRow(ctx, `
		SELECT nd.fqdn
		FROM cdk_issues ci
		JOIN node_projects np ON np.id = ci.node_project_id
		JOIN nodes n          ON n.id = np.node_id
		JOIN node_domains nd  ON nd.node_id = n.id
		WHERE ci.public_id = $1 AND ci.revoked_at IS NULL
		  AND nd.released_at IS NULL
		  AND nd.record_state IN ('insync', 'pending')
		ORDER BY (nd.record_state = 'insync') DESC, nd.seq
		LIMIT 1`, publicID).Scan(&fqdn)
	if err != nil {
		if isNoRows(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	return fqdn, nil
}
