package consolestore

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// 管理员认证的存储层（console-fleet-design §8.1、§8.2）。
//
// 会话是**服务端会话表**不是无状态HMAC：管理面必须能立即撤销（设备丢失、人员变动）。
// 这与连接令牌的取舍不同——那里选无状态是因为2分钟TTL够短且worker每次实时复查额度，
// 管理会话两个前提都不成立。

type AdminUser struct {
	ID            string
	Username      string
	PasswordHash  string
	TOTPSecretEnc []byte
	TOTPNonce     []byte
	DisabledAt    *time.Time
}

// CreateAdmin新建管理员；TOTP secret以信封加密的形态传入（明文不进本层）。
func (s *Store) CreateAdmin(ctx context.Context, username, passwordHash string, totpEnc, totpNonce []byte) (string, error) {
	id := uuid.NewString()
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO admin_users (id, username, password_hash, totp_secret_enc, totp_nonce)
		VALUES ($1,$2,$3,$4,$5)`, id, username, passwordHash, totpEnc, totpNonce)
	return id, err
}

// AdminByUsername查管理员；查无返回ErrNotFound。
// 调用方**必须**把ErrNotFound与密码错、TOTP错折叠成同一个错误（§8.1）。
func (s *Store) AdminByUsername(ctx context.Context, username string) (AdminUser, error) {
	var u AdminUser
	err := s.Pool.QueryRow(ctx, `
		SELECT id, username, password_hash, totp_secret_enc, totp_nonce, disabled_at
		FROM admin_users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecretEnc, &u.TOTPNonce, &u.DisabledAt)
	if err != nil {
		if isNoRows(err) {
			return AdminUser{}, ErrNotFound
		}
		return AdminUser{}, err
	}
	return u, nil
}

// CountAdmins供首次安装引导判断是否已有管理员。
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_users`).Scan(&n)
	return n, err
}

// SetAdminPassword用于改密（同时使该用户的全部会话失效由调用方决定）。
func (s *Store) SetAdminPassword(ctx context.Context, userID, passwordHash string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE admin_users SET password_hash=$2 WHERE id=$1`, userID, passwordHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type AdminSession struct {
	ID         string
	UserID     string
	Username   string
	ClientIP   string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// CreateSession写入会话。tokenHash是cookie明文的哈希——**库里只存哈希**，
// 与CDK同一原则：库被读走也不能直接冒充会话。
func (s *Store) CreateSession(ctx context.Context, userID, tokenHash, clientIP string, ttl time.Duration) (string, error) {
	id := uuid.NewString()
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO admin_sessions (id, user_id, token_hash, client_ip, expires_at)
		VALUES ($1,$2,$3,$4, now() + make_interval(secs => $5::double precision))`,
		id, userID, tokenHash, clientIP, ttl.Seconds())
	return id, err
}

// SessionByTokenHash查活跃会话并**顺带推进last_seen_at**（空闲超时的依据）。
//
// 三条失效条件在SQL里一次判完，全部用数据库now()（CLAUDE.md：时间窗口用数据库now()）：
//   - revoked_at非空：已被显式撤销（立即生效，这是选会话表的理由）
//   - expires_at过期：绝对超时
//   - last_seen_at早于空闲阈值：空闲超时
//
// 用户被禁用时同样不返回——禁用账号必须立刻失去访问，而不是等会话自然过期。
func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash string, idle time.Duration) (AdminSession, error) {
	var a AdminSession
	err := s.Pool.QueryRow(ctx, `
		UPDATE admin_sessions se SET last_seen_at = now()
		FROM admin_users u
		WHERE se.token_hash = $1
		  AND u.id = se.user_id AND u.disabled_at IS NULL
		  AND se.revoked_at IS NULL
		  AND se.expires_at > now()
		  AND se.last_seen_at > now() - make_interval(secs => $2::double precision)
		RETURNING se.id, se.user_id, u.username, se.client_ip, se.created_at, se.last_seen_at, se.expires_at`,
		tokenHash, idle.Seconds()).
		Scan(&a.ID, &a.UserID, &a.Username, &a.ClientIP, &a.CreatedAt, &a.LastSeenAt, &a.ExpiresAt)
	if err != nil {
		if isNoRows(err) {
			return AdminSession{}, ErrNotFound
		}
		return AdminSession{}, err
	}
	return a, nil
}

// RevokeSession撤销单个会话（登出）。幂等：已撤销的再撤销不报错。
func (s *Store) RevokeSession(ctx context.Context, tokenHash string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE admin_sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash)
	return err
}

// RevokeUserSessions撤销某管理员的全部会话（设备丢失、改密后）。返回影响行数。
func (s *Store) RevokeUserSessions(ctx context.Context, userID string) (int64, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE admin_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AuditEntry是一条审计记录（§8.5）。Detail写入前必须已经过redact。
type AuditEntry struct {
	Actor    string // 管理员ID；未登录动作为空
	Action   string
	Target   string
	Result   string // ok|denied|error
	Detail   map[string]any
	ClientIP string
}

// WriteAudit写审计日志。**调用方必须把它的错误当成动作失败**（§8.5：
// 不允许存在无审计的特权操作）。
func (s *Store) WriteAudit(ctx context.Context, e AuditEntry) error {
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	var actor any
	if e.Actor != "" {
		actor = e.Actor
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO audit_log (actor, action, target, result, detail, client_ip)
		VALUES ($1,$2,$3,$4,$5,$6)`, actor, e.Action, e.Target, e.Result, detail, e.ClientIP)
	return err
}
