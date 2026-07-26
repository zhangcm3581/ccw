package consolestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newAdmin(t *testing.T, st *Store) (id, username string) {
	t.Helper()
	ctx := context.Background()
	username = "admin-" + uuid.NewString()[:8]
	id, err := st.CreateAdmin(ctx, username, "argon2id$fake", []byte("enc"), []byte("nonce"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Pool.Exec(ctx, `DELETE FROM admin_sessions WHERE user_id=$1`, id)
		st.Pool.Exec(ctx, `DELETE FROM audit_log WHERE actor=$1`, id)
		st.Pool.Exec(ctx, `DELETE FROM admin_users WHERE id=$1`, id)
	})
	return id, username
}

func TestAdminByUsername(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	id, username := newAdmin(t, st)

	u, err := st.AdminByUsername(ctx, username)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != id || u.PasswordHash != "argon2id$fake" || string(u.TOTPSecretEnc) != "enc" {
		t.Errorf("字段错误: %+v", u)
	}
	if _, err := st.AdminByUsername(ctx, "ghost-user"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知用户应ErrNotFound，got %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	userID, username := newAdmin(t, st)

	if _, err := st.CreateSession(ctx, userID, "hash-A", "203.0.113.5", 12*time.Hour); err != nil {
		t.Fatal(err)
	}
	s, err := st.SessionByTokenHash(ctx, "hash-A", 30*time.Minute)
	if err != nil {
		t.Fatalf("有效会话应可查: %v", err)
	}
	if s.UserID != userID || s.Username != username || s.ClientIP != "203.0.113.5" {
		t.Errorf("会话字段错误: %+v", s)
	}

	// 撤销后立刻失效——这正是选服务端会话表而非无状态HMAC的理由（A6）。
	if err := st.RevokeSession(ctx, "hash-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionByTokenHash(ctx, "hash-A", 30*time.Minute); !errors.Is(err, ErrNotFound) {
		t.Errorf("撤销后应立即失效，got %v", err)
	}
	// 幂等
	if err := st.RevokeSession(ctx, "hash-A"); err != nil {
		t.Errorf("重复撤销应幂等: %v", err)
	}
	if _, err := st.SessionByTokenHash(ctx, "unknown-hash", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知token应ErrNotFound，got %v", err)
	}
}

func TestSessionAbsoluteTimeout(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	userID, _ := newAdmin(t, st)

	// TTL为负＝已过期（用数据库now()判定，不依赖测试机时钟）
	if _, err := st.CreateSession(ctx, userID, "hash-exp", "203.0.113.5", -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionByTokenHash(ctx, "hash-exp", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("绝对超时的会话不应返回，got %v", err)
	}
}

func TestSessionIdleTimeoutAndRefresh(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	userID, _ := newAdmin(t, st)
	if _, err := st.CreateSession(ctx, userID, "hash-idle", "203.0.113.5", 12*time.Hour); err != nil {
		t.Fatal(err)
	}
	// 把last_seen_at拨回1小时前，模拟空闲
	if _, err := st.Pool.Exec(ctx,
		`UPDATE admin_sessions SET last_seen_at = now() - interval '1 hour' WHERE token_hash='hash-idle'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionByTokenHash(ctx, "hash-idle", 30*time.Minute); !errors.Is(err, ErrNotFound) {
		t.Errorf("空闲超时应失效，got %v", err)
	}
	// 放宽空闲阈值即可再次命中，且命中会推进last_seen_at
	s, err := st.SessionByTokenHash(ctx, "hash-idle", 2*time.Hour)
	if err != nil {
		t.Fatalf("阈值内应命中: %v", err)
	}
	if time.Since(s.LastSeenAt) > time.Minute {
		t.Errorf("命中应推进last_seen_at，got %v", s.LastSeenAt)
	}
	if _, err := st.SessionByTokenHash(ctx, "hash-idle", 30*time.Minute); err != nil {
		t.Errorf("刚推进过last_seen_at，30分钟阈值应命中: %v", err)
	}
}

// 用户被禁用后会话立刻不可用——不等自然过期。
func TestDisabledUserSessionsRejected(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	userID, _ := newAdmin(t, st)
	st.CreateSession(ctx, userID, "hash-dis", "203.0.113.5", 12*time.Hour)
	if _, err := st.SessionByTokenHash(ctx, "hash-dis", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE admin_users SET disabled_at=now() WHERE id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionByTokenHash(ctx, "hash-dis", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("禁用用户的会话应立即失效，got %v", err)
	}
}

func TestRevokeUserSessions(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	userID, _ := newAdmin(t, st)
	st.CreateSession(ctx, userID, "hash-1", "ip", time.Hour)
	st.CreateSession(ctx, userID, "hash-2", "ip", time.Hour)

	n, err := st.RevokeUserSessions(ctx, userID)
	if err != nil || n != 2 {
		t.Fatalf("应撤销2个会话: n=%d err=%v", n, err)
	}
	for _, h := range []string{"hash-1", "hash-2"} {
		if _, err := st.SessionByTokenHash(ctx, h, time.Hour); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s应已失效", h)
		}
	}
	if n, _ := st.RevokeUserSessions(ctx, userID); n != 0 {
		t.Errorf("重复撤销应影响0行，got %d", n)
	}
}

func TestWriteAudit(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	userID, _ := newAdmin(t, st)

	if err := st.WriteAudit(ctx, AuditEntry{
		Actor: userID, Action: "admin.login", Target: "session", Result: "ok",
		Detail: map[string]any{"ua": "test"}, ClientIP: "203.0.113.5",
	}); err != nil {
		t.Fatal(err)
	}
	// 未登录动作：actor为空应写成NULL而不是报外键错
	if err := st.WriteAudit(ctx, AuditEntry{
		Action: "admin.login", Target: "session", Result: "denied", ClientIP: "203.0.113.9",
	}); err != nil {
		t.Fatalf("匿名审计应可写: %v", err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE client_ip IN ('203.0.113.5','203.0.113.9')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("应有2条审计，got %d", n)
	}
	st.Pool.Exec(ctx, `DELETE FROM audit_log WHERE client_ip IN ('203.0.113.5','203.0.113.9')`)
}
