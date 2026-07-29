package consolestore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"ccw/internal/dns"
)

// 节点、zone与域名分配的存储（console-fleet-design §10）。

type Node struct {
	ID           string
	Name         string
	Host         string
	SSHPort      int
	SSHUser      string
	HostKeyFP    *string
	Status       string
	OSRelease    *string
	Arch         *string
	StackVersion *string
	LastSeenAt   *time.Time
	CreatedAt    time.Time
}

// CreateNode登记一台待纳管的节点（状态new）。
func (s *Store) CreateNode(ctx context.Context, name, host string, port int, user string) (string, error) {
	if port == 0 {
		port = 22
	}
	id := uuid.NewString()
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO nodes (id, name, host, ssh_port, ssh_user, status)
		VALUES ($1,$2,$3,$4,$5,'new')`, id, name, host, port, user)
	return id, err
}

func (s *Store) GetNode(ctx context.Context, id string) (Node, error) {
	var n Node
	err := s.Pool.QueryRow(ctx, `
		SELECT id, name, host, ssh_port, ssh_user, host_key_fp, status,
		       os_release, arch, stack_version, last_seen_at, created_at
		FROM nodes WHERE id=$1`, id).
		Scan(&n.ID, &n.Name, &n.Host, &n.SSHPort, &n.SSHUser, &n.HostKeyFP, &n.Status,
			&n.OSRelease, &n.Arch, &n.StackVersion, &n.LastSeenAt, &n.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return Node{}, ErrNotFound
		}
		return Node{}, err
	}
	return n, nil
}

func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, name, host, ssh_port, ssh_user, host_key_fp, status,
		       os_release, arch, stack_version, last_seen_at, created_at
		FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Name, &n.Host, &n.SSHPort, &n.SSHUser, &n.HostKeyFP, &n.Status,
			&n.OSRelease, &n.Arch, &n.StackVersion, &n.LastSeenAt, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) SetNodeStatus(ctx context.Context, id, status string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE nodes SET status=$2 WHERE id=$1`, id, status)
	return err
}

// SetNodeHostKey固定host key指纹（TOFU首次连接后，§5.2）。
func (s *Store) SetNodeHostKey(ctx context.Context, id, fingerprint string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE nodes SET host_key_fp=$2 WHERE id=$1`, id, fingerprint)
	return err
}

// SetNodeFacts记录probe采集到的系统信息。
func (s *Store) SetNodeFacts(ctx context.Context, id, osRelease, arch string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE nodes SET os_release=$2, arch=$3 WHERE id=$1`, id, osRelease, arch)
	return err
}

func (s *Store) SetNodeStackVersion(ctx context.Context, id, version string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE nodes SET stack_version=$2, last_seen_at=now() WHERE id=$1`, id, version)
	return err
}

// SaveNodeCredential保存托管私钥（已由调用方信封加密）。
// **必须在密钥验证通过之后调用**（§9）：先落库后验证会留下连不上的托管密钥。
func (s *Store) SaveNodeCredential(ctx context.Context, nodeID string, privEnc, nonce []byte, publicKey string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO node_credentials (node_id, private_key_enc, nonce, public_key)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (node_id) DO UPDATE SET
		  private_key_enc = EXCLUDED.private_key_enc, nonce = EXCLUDED.nonce,
		  public_key = EXCLUDED.public_key, rotated_at = now()`,
		nodeID, privEnc, nonce, publicKey)
	return err
}

// NodeCredential返回加密的托管私钥；调用方用secretbox解开。
func (s *Store) NodeCredential(ctx context.Context, nodeID string) (privEnc, nonce []byte, publicKey string, err error) {
	err = s.Pool.QueryRow(ctx,
		`SELECT private_key_enc, nonce, public_key FROM node_credentials WHERE node_id=$1`, nodeID).
		Scan(&privEnc, &nonce, &publicKey)
	if err != nil {
		if isNoRows(err) {
			return nil, nil, "", ErrNotFound
		}
		return nil, nil, "", err
	}
	return privEnc, nonce, publicKey, nil
}

// ---- zone与子域名分配 ----

func (s *Store) CreateZone(ctx context.Context, domain, provider, prefix string) (string, error) {
	id := uuid.NewString()
	if prefix == "" {
		prefix = "api"
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO dns_zones (id, domain, provider, subdomain_prefix)
		VALUES ($1,$2,$3,$4)`, id, domain, provider, prefix)
	return id, err
}

func (s *Store) GetZone(ctx context.Context, id string) (dns.Zone, error) {
	var z dns.Zone
	err := s.Pool.QueryRow(ctx, `
		SELECT id, domain, provider, provider_ref, subdomain_prefix FROM dns_zones WHERE id=$1`, id).
		Scan(&z.ID, &z.Domain, &z.Provider, &z.ProviderRef, &z.SubdomainPrefix)
	if err != nil {
		if isNoRows(err) {
			return dns.Zone{}, ErrNotFound
		}
		return dns.Zone{}, err
	}
	return z, nil
}

func (s *Store) ListZones(ctx context.Context) ([]dns.Zone, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, domain, provider, provider_ref, subdomain_prefix FROM dns_zones ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dns.Zone
	for rows.Next() {
		var z dns.Zone
		if err := rows.Scan(&z.ID, &z.Domain, &z.Provider, &z.ProviderRef, &z.SubdomainPrefix); err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

// NextSeq实现dns.SeqAllocator：原子地取出并递增zone的next_seq。
//
// **单条UPDATE ... RETURNING保证原子性**：读出再写回会在并发纳管时把同一个序号
// 发给两台机器，两者抢同一个子域名。序号永不回收，因此这里只增不减（§6.2）。
func (s *Store) NextSeq(ctx context.Context, zoneID string) (int, error) {
	var seq int
	err := s.Pool.QueryRow(ctx,
		`UPDATE dns_zones SET next_seq = next_seq + 1 WHERE id=$1 RETURNING next_seq - 1`, zoneID).
		Scan(&seq)
	if err != nil {
		if isNoRows(err) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return seq, nil
}

// 编译期断言：*Store满足分配器接口。
var _ dns.SeqAllocator = (*Store)(nil)

type NodeDomain struct {
	ID          string
	ZoneID      string
	Seq         int
	FQDN        string
	NodeID      *string
	TargetIP    string
	RecordState string
	ReleasedAt  *time.Time
}

// AllocateDomain登记一条子域名分配。
func (s *Store) AllocateDomain(ctx context.Context, zoneID string, seq int, fqdn, nodeID, targetIP string) (string, error) {
	id := uuid.NewString()
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO node_domains (id, zone_id, seq, fqdn, node_id, target_ip, record_state)
		VALUES ($1,$2,$3,$4,$5,$6,'pending')`, id, zoneID, seq, fqdn, nodeID, targetIP)
	return id, err
}

// MarkDomainInSync在DNS校验通过后更新状态。
func (s *Store) MarkDomainInSync(ctx context.Context, fqdn string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE node_domains SET record_state='insync', dns_verified_at=now() WHERE fqdn=$1`, fqdn)
	return err
}

// DomainByNode返回节点当前的域名（未退役的）。
func (s *Store) DomainByNode(ctx context.Context, nodeID string) (NodeDomain, error) {
	var d NodeDomain
	err := s.Pool.QueryRow(ctx, `
		SELECT id, zone_id, seq, fqdn, node_id, target_ip, record_state, released_at
		FROM node_domains WHERE node_id=$1 AND released_at IS NULL
		ORDER BY (record_state='insync') DESC, seq LIMIT 1`, nodeID).
		Scan(&d.ID, &d.ZoneID, &d.Seq, &d.FQDN, &d.NodeID, &d.TargetIP, &d.RecordState, &d.ReleasedAt)
	if err != nil {
		if isNoRows(err) {
			return NodeDomain{}, ErrNotFound
		}
		return NodeDomain{}, err
	}
	return d, nil
}

// DomainRow是域名页的一行：域名 + 它属于谁 + DNS生效状态。
type DomainRow struct {
	FQDN        string
	Seq         int
	TargetIP    string
	RecordState string
	VerifiedAt  *time.Time
	Zone        string
	NodeName    string
	NodeID      string
}

// ListDomains列出全部未退役的域名分配。
//
// 域名页要回答的是「哪个子域名给了哪台机器、DNS生效没有」，
// 逐节点调DomainByNode答不了——没绑到节点的分配（分配了但纳管中断）就看不见了，
// 而那恰恰是最需要被看见的一种。
func (s *Store) ListDomains(ctx context.Context) ([]DomainRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT nd.fqdn, nd.seq, nd.target_ip, nd.record_state, nd.dns_verified_at,
		       z.domain, COALESCE(n.name, ''), COALESCE(n.id::text, '')
		FROM node_domains nd
		JOIN dns_zones z ON z.id = nd.zone_id
		LEFT JOIN nodes n ON n.id = nd.node_id
		WHERE nd.released_at IS NULL
		ORDER BY z.domain, nd.seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DomainRow
	for rows.Next() {
		var d DomainRow
		if err := rows.Scan(&d.FQDN, &d.Seq, &d.TargetIP, &d.RecordState, &d.VerifiedAt,
			&d.Zone, &d.NodeName, &d.NodeID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DomainTaken判断某个fqdn是否已被占用（含已退役的记录——序号永不回收，
// 退役后的域名也不该再分配给新节点，§6.2）。
func (s *Store) DomainTaken(ctx context.Context, fqdn string) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM node_domains WHERE fqdn=$1)`, fqdn).Scan(&exists)
	return exists, err
}

// RetireNode解除纳管：退役该节点的域名，然后删除节点行。
//
// **只清Console这边的账**，绝不碰远端机器：容器、数据卷、Claude凭据都还在
// 那台服务器上跑。要真正下线机器得自己登机处理——由后台替管理员销毁一台
// 还在服务的机器，是无法撤销且后果不成比例的操作。
//
// 域名行**保留**（node_id置NULL、记released_at）：子域名序号永不回收（设计§6.2），
// 删掉行就等于让下一台节点拿到同一个序号，撞上仍在DNS里的旧记录。
// 其余从表（凭据、项目、CDK签发、运行与步骤）由外键CASCADE一并删除。
func (s *Store) RetireNode(ctx context.Context, nodeID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE node_domains
		SET released_at = now(), record_state = 'removed'
		WHERE node_id = $1 AND released_at IS NULL`, nodeID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, nodeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}
