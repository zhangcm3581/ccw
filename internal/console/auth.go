package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	stdsync "sync"
	"time"

	"ccw/internal/auth"
	"ccw/internal/consolestore"
	"ccw/internal/secretbox"
	"ccw/internal/totp"
)

// C3管理员认证（console-fleet-design §8.1–§8.3、§8.5）。
//
// 权限等同全机队root，因此安全要求高于现有任何组件：
// 密码+TOTP缺一不可、统一错误、限速、服务端可撤销会话、IP白名单应用层复核、CSRF。

const (
	sessionCookie   = "ccw_admin_session"
	csrfCookie      = "ccw_admin_csrf"
	sessionAbsolute = 12 * time.Hour   // 绝对超时（§8.2）
	sessionIdle     = 30 * time.Minute // 空闲超时（§8.2）
)

// AdminStore是认证需要的存储能力面。
type AdminStore interface {
	AdminByUsername(ctx context.Context, username string) (consolestore.AdminUser, error)
	CreateSession(ctx context.Context, userID, tokenHash, clientIP string, ttl time.Duration) (string, error)
	SessionByTokenHash(ctx context.Context, tokenHash string, idle time.Duration) (consolestore.AdminSession, error)
	RevokeSession(ctx context.Context, tokenHash string) error
	WriteAudit(ctx context.Context, e consolestore.AuditEntry) error
}

// Auth持有管理后台的认证依赖。为nil时Server不注册任何/admin路由——
// 没有认证就不上管理页面（DEPLOY-CONSOLE.md §8）。
type Auth struct {
	Store AdminStore
	Box   *secretbox.Box // 解TOTP secret用
	// Allowlist是IP白名单（CIDR或单IP）。**应用层独立校验，不只依赖Caddy**（§8.3）：
	// 反代配置改错时应用层要兜底。为空表示不限制（仅限本机开发）。
	Allowlist []*net.IPNet
	// Secure控制cookie的Secure属性；生产必须为true（HTTPS），本地HTTP测试可关。
	Secure bool

	rlMu     stdsync.Mutex
	attempts map[string][]time.Time
}

// ParseAllowlist解析空格/逗号分隔的IP与CIDR列表（CCW_ADMIN_ALLOWLIST）。
func ParseAllowlist(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		if !strings.Contains(tok, "/") {
			ip := net.ParseIP(tok)
			if ip == nil {
				return nil, errors.New("console: 无法解析白名单条目: " + tok)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(tok)
		if err != nil {
			return nil, errors.New("console: 无法解析白名单CIDR: " + tok)
		}
		out = append(out, n)
	}
	return out, nil
}

func (a *Auth) ipAllowed(ipStr string) bool {
	if len(a.Allowlist) == 0 {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range a.Allowlist {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// allowLogin：每IP与每用户名各自每分钟5次（§8.1）。
// 两个维度都要：只按IP限不住分布式撞库，只按用户名会让攻击者用一个IP扫遍全部用户名。
func (a *Auth) allowLogin(keys ...string) bool {
	a.rlMu.Lock()
	defer a.rlMu.Unlock()
	if a.attempts == nil {
		a.attempts = map[string][]time.Time{}
	}
	cutoff := time.Now().Add(-time.Minute)
	// 先检查全部维度，任一超限即拒绝，且**不记录本次尝试**——
	// 否则被限速者的持续重试会让窗口永远滑不出去。
	for _, k := range keys {
		kept := a.attempts[k][:0]
		for _, t := range a.attempts[k] {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		a.attempts[k] = kept
		if len(kept) >= 5 {
			return false
		}
	}
	now := time.Now()
	for _, k := range keys {
		a.attempts[k] = append(a.attempts[k], now)
	}
	return true
}

// hashToken对会话令牌做SHA-256：库里只存哈希（§8.2）。
// 这里用SHA-256而不是Argon2id——令牌是128位随机值、无需抗暴力，
// 且每次请求都要查库，慢哈希会让每个页面请求多花百毫秒。
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// currentAdmin返回当前登录会话；未登录/已撤销/超时返回ErrNotFound。
func (a *Auth) currentAdmin(r *http.Request) (consolestore.AdminSession, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return consolestore.AdminSession{}, consolestore.ErrNotFound
	}
	return a.Store.SessionByTokenHash(r.Context(), hashToken(c.Value), sessionIdle)
}

// audit写审计；写失败时**动作也失败**（§8.5：不允许存在无审计的特权操作）。
func (a *Auth) audit(ctx context.Context, e consolestore.AuditEntry) error {
	return a.Store.WriteAudit(ctx, e)
}

// requireAdmin是管理页面的中间件：IP白名单 → 会话 → CSRF（写操作）。
//
// 未通过一律404而不是401/403——与Caddy的白名单行为一致，不暴露后台存在（§8.3）。
func (a *Auth) requireAdmin(next func(http.ResponseWriter, *http.Request, consolestore.AdminSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !a.ipAllowed(ip) {
			http.NotFound(w, r)
			return
		}
		s, err := a.currentAdmin(r)
		if err != nil {
			// 未登录：跳登录页（GET）或直接404（其它方法）
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/admin/login", http.StatusFound)
				return
			}
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !a.checkCSRF(r) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		next(w, r, s)
	}
}

// checkCSRF用double-submit cookie：cookie里的随机值必须与表单字段一致。
// SameSite=Strict已挡住多数跨站请求，这是第二道（§8.2要求所有写操作有CSRF token）。
func (a *Auth) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	if err := r.ParseForm(); err != nil {
		return false
	}
	got := r.PostFormValue("csrf_token")
	return got != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(got)) == 1
}

func (a *Auth) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: name == sessionCookie, // CSRF token要给页面JS/表单读，不能HttpOnly
		Secure:   a.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// issueCSRF为登录页与管理页提供CSRF token。
//
// **已有合法token时复用，不每次渲染都换**：换了会让此前打开的其它标签页
// 手里的表单立刻作废，提交时莫名其妙得到403。token是随机的、随会话过期，
// 在会话生命周期内复用不降低防护（double-submit防的是跨站方读不到cookie）。
func (a *Auth) issueCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && csrfTokenRe.MatchString(c.Value) {
		return c.Value
	}
	tok, err := randomToken()
	if err != nil {
		return ""
	}
	a.setCookie(w, csrfCookie, tok, int(sessionAbsolute.Seconds()))
	return tok
}

// csrfTokenRe校验cookie里的token形态，避免把客户端塞进来的任意值当成自己发的。
var csrfTokenRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// login执行密码+TOTP双因子校验。
//
// **统一错误（§8.1、验收A4）**：用户不存在、密码错、TOTP错、账号被禁用
// 返回完全相同的invalid_credentials，不区分——否则可用错误差异枚举用户名。
func (a *Auth) login(ctx context.Context, username, password, code, clientIP string) (userID string, token string, err error) {
	fail := errors.New("invalid_credentials")

	u, uerr := a.Store.AdminByUsername(ctx, username)
	if uerr != nil {
		if errors.Is(uerr, consolestore.ErrNotFound) {
			// 用户不存在也走一次Argon2id，避免用响应时间区分用户是否存在。
			auth.VerifySecret(password, dummyHash)
			return "", "", fail
		}
		return "", "", uerr // 基础设施错误：如实上抛，不伪装成认证失败
	}
	if u.DisabledAt != nil {
		auth.VerifySecret(password, dummyHash)
		return "", "", fail
	}
	if !auth.VerifySecret(password, u.PasswordHash) {
		return "", "", fail
	}
	secret, derr := a.Box.Open(u.TOTPSecretEnc, u.TOTPNonce, totpContext)
	if derr != nil {
		// 解不开TOTP secret是配置/密钥问题（CCW_SECRET_KEY换过？），不是凭据错。
		return "", "", derr
	}
	if !totp.Verify(string(secret), code, time.Now()) {
		return "", "", fail
	}

	tok, terr := randomToken()
	if terr != nil {
		return "", "", terr
	}
	if _, serr := a.Store.CreateSession(ctx, u.ID, hashToken(tok), clientIP, sessionAbsolute); serr != nil {
		return "", "", serr
	}
	return u.ID, tok, nil
}

// totpContext是TOTP secret信封加密的AAD用途标签（见internal/secretbox）。
const totpContext = "admin-totp"

// dummyHash是一个真实可解析的Argon2id哈希，用于"用户不存在/已禁用"时的等时校验：
// 让这两条路径也付出一次完整的Argon2id开销，避免用响应时间区分用户是否存在。
// 它对应一个固定的无用密码，永远不会被任何真实输入匹配（生成后已验证VerifySecret为false）。
const dummyHash = "argon2id$v=19$m=65536,t=3,p=2$onK41/pzgAnXM32iTPY9Dg$PoIEjEgIuFI2k6Tr4s61otFX0LGzXI6E/qSAPW6vcpI"
