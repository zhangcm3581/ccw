package console

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ccw/internal/auth"
	"ccw/internal/consolestore"
	"ccw/internal/secretbox"
	"ccw/internal/totp"
)

// C3认证的HTTP层测试（console-fleet-design §8）。存储用假实现，
// 真实SQL语义（撤销立即生效、空闲/绝对超时、禁用用户）由consolestore的PG集成测试覆盖。

type fakeAdminStore struct {
	user     consolestore.AdminUser
	found    bool
	sessions map[string]consolestore.AdminSession // by tokenHash
	audits   []consolestore.AuditEntry
	auditErr error
}

func (f *fakeAdminStore) AdminByUsername(_ context.Context, u string) (consolestore.AdminUser, error) {
	if !f.found || u != f.user.Username {
		return consolestore.AdminUser{}, consolestore.ErrNotFound
	}
	return f.user, nil
}
func (f *fakeAdminStore) CreateSession(_ context.Context, userID, tokenHash, ip string, ttl time.Duration) (string, error) {
	f.sessions[tokenHash] = consolestore.AdminSession{
		ID: "sess-1", UserID: userID, Username: f.user.Username, ClientIP: ip,
		ExpiresAt: time.Now().Add(ttl),
	}
	return "sess-1", nil
}
func (f *fakeAdminStore) SessionByTokenHash(_ context.Context, h string, _ time.Duration) (consolestore.AdminSession, error) {
	s, ok := f.sessions[h]
	if !ok {
		return consolestore.AdminSession{}, consolestore.ErrNotFound
	}
	return s, nil
}
func (f *fakeAdminStore) RevokeSession(_ context.Context, h string) error {
	delete(f.sessions, h)
	return nil
}
func (f *fakeAdminStore) WriteAudit(_ context.Context, e consolestore.AuditEntry) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, e)
	return nil
}

const testPassword = "correct-horse-battery-staple"

func newAuthServer(t *testing.T) (*Server, *fakeAdminStore, string) {
	t.Helper()
	key, _ := hex.DecodeString(strings.Repeat("ab", 32))
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := totp.GenerateSecret()
	enc, nonce, _ := box.Seal([]byte(secret), totpContext)
	hash, _ := auth.HashSecret(testPassword)

	fs := &fakeAdminStore{
		found:    true,
		user:     consolestore.AdminUser{ID: "u1", Username: "admin", PasswordHash: hash, TOTPSecretEnc: enc, TOTPNonce: nonce},
		sessions: map[string]consolestore.AdminSession{},
	}
	s, _, _, _ := newTestServer(t)
	s.Auth = &Auth{Store: fs, Box: box}
	return s, fs, secret
}

func postForm(t *testing.T, s *Server, path string, form url.Values, cookies []*http.Cookie, ip string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// 取登录页发的CSRF cookie，构造合法表单。
func loginForm(t *testing.T, s *Server, username, password, code string) (url.Values, []*http.Cookie) {
	t.Helper()
	w := get(t, s, "/admin/login", map[string]string{"X-Forwarded-For": "203.0.113.5"})
	if w.Code != 200 {
		t.Fatalf("登录页应200，got %d", w.Code)
	}
	cookies := w.Result().Cookies()
	var csrf string
	for _, c := range cookies {
		if c.Name == csrfCookie {
			csrf = c.Value
		}
	}
	if csrf == "" {
		t.Fatal("登录页应下发CSRF cookie")
	}
	return url.Values{
		"username": {username}, "password": {password}, "code": {code}, "csrf_token": {csrf},
	}, cookies
}

// Auth为nil时/admin完全不存在——没有认证就不上管理页面。
func TestAdminRoutesAbsentWithoutAuth(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	for _, p := range []string{"/admin", "/admin/login"} {
		if w := get(t, s, p, nil); w.Code != 404 {
			t.Errorf("%s 在未配置Auth时应404，got %d", p, w.Code)
		}
	}
}

func TestLoginSuccessAndSession(t *testing.T) {
	s, fs, secret := newAuthServer(t)
	code, _ := totp.Code(secret, time.Now())
	form, cookies := loginForm(t, s, "admin", testPassword, code)

	w := postForm(t, s, "/admin/login", form, cookies, "203.0.113.5")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/admin" {
		t.Fatalf("登录成功应302到/admin，got %d %s", w.Code, w.Header().Get("Location"))
	}
	var sess *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			sess = c
		}
	}
	if sess == nil || sess.Value == "" {
		t.Fatal("应下发会话cookie")
	}
	if !sess.HttpOnly || sess.SameSite != http.SameSiteStrictMode {
		t.Errorf("会话cookie必须HttpOnly+SameSite=Strict，got %+v", sess)
	}
	// 库里只存哈希，cookie明文不得出现在存储里（§8.2）
	if _, leaked := fs.sessions[sess.Value]; leaked {
		t.Error("会话表的键应是token哈希，不是明文")
	}
	if _, ok := fs.sessions[hashToken(sess.Value)]; !ok {
		t.Error("应按哈希存会话")
	}
	// 审计
	if len(fs.audits) != 1 || fs.audits[0].Result != "ok" || fs.audits[0].Actor != "u1" {
		t.Errorf("登录应写成功审计: %+v", fs.audits)
	}

	// 带会话访问总览
	req := httptest.NewRequest("GET", "/admin", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	req.AddCookie(sess)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "机队总览") {
		t.Errorf("带会话应能访问总览，got %d", rec.Code)
	}
}

// A4：用户不存在、密码错、TOTP错三种情况返回**完全相同**的错误。
func TestLoginUnifiedError(t *testing.T) {
	s, _, secret := newAuthServer(t)
	good, _ := totp.Code(secret, time.Now())

	var bodies []string
	cases := []struct{ user, pass, code string }{
		{"ghost", testPassword, good}, // 用户不存在
		{"admin", "wrong-password", good},
		{"admin", testPassword, "000000"}, // TOTP错
	}
	for i, c := range cases {
		// 每次换IP避免触发限速
		ip := "203.0.113." + string(rune('1'+i))
		form, cookies := loginForm(t, s, c.user, c.pass, c.code)
		w := postForm(t, s, "/admin/login", form, cookies, ip)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("case %d 应401，got %d", i, w.Code)
		}
		body := w.Body.String()
		if strings.Contains(body, "不存在") || strings.Contains(body, "密码错误") || strings.Contains(body, "验证码错误") {
			t.Errorf("case %d 泄露了失败原因", i)
		}
		bodies = append(bodies, body)
	}
	// 三个响应体必须逐字节相同（除CSRF token外——它每次随机，先剥掉）
	norm := func(s string) string {
		i := strings.Index(s, `name="csrf_token" value="`)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i+25:], `"`)
		return s[:i] + s[i+25+j:]
	}
	if norm(bodies[0]) != norm(bodies[1]) || norm(bodies[1]) != norm(bodies[2]) {
		t.Error("A4：三种失败原因的响应必须完全相同")
	}
}

// 密码对但没有TOTP码：必须失败（缺一不可，§8.1）。
func TestLoginRequiresTOTP(t *testing.T) {
	s, _, _ := newAuthServer(t)
	form, cookies := loginForm(t, s, "admin", testPassword, "")
	if w := postForm(t, s, "/admin/login", form, cookies, "203.0.113.7"); w.Code != http.StatusUnauthorized {
		t.Errorf("无TOTP码应401，got %d", w.Code)
	}
}

func TestLoginRequiresCSRF(t *testing.T) {
	s, _, secret := newAuthServer(t)
	code, _ := totp.Code(secret, time.Now())
	form := url.Values{"username": {"admin"}, "password": {testPassword}, "code": {code}}
	// 没有CSRF cookie与字段
	if w := postForm(t, s, "/admin/login", form, nil, "203.0.113.5"); w.Code != http.StatusForbidden {
		t.Errorf("缺CSRF应403，got %d", w.Code)
	}
	// cookie与字段不匹配
	form.Set("csrf_token", "mismatched-value")
	cookies := []*http.Cookie{{Name: csrfCookie, Value: "real-value"}}
	if w := postForm(t, s, "/admin/login", form, cookies, "203.0.113.5"); w.Code != http.StatusForbidden {
		t.Errorf("CSRF不匹配应403，got %d", w.Code)
	}
}

// §8.1：每IP每分钟5次。
func TestLoginRateLimit(t *testing.T) {
	s, _, _ := newAuthServer(t)
	for i := 0; i < 5; i++ {
		form, cookies := loginForm(t, s, "admin", "wrong", "000000")
		if w := postForm(t, s, "/admin/login", form, cookies, "198.51.100.1"); w.Code != http.StatusUnauthorized {
			t.Fatalf("第%d次应401，got %d", i+1, w.Code)
		}
	}
	form, cookies := loginForm(t, s, "admin", "wrong", "000000")
	w := postForm(t, s, "/admin/login", form, cookies, "198.51.100.1")
	if !strings.Contains(w.Body.String(), "频繁") {
		t.Error("超限应提示限速")
	}
}

// §8.3：应用层独立校验IP白名单，不只依赖Caddy；白名单外一律404。
func TestIPAllowlistEnforcedInApp(t *testing.T) {
	s, _, secret := newAuthServer(t)
	nets, err := ParseAllowlist("203.0.113.0/24, 198.51.100.9")
	if err != nil {
		t.Fatal(err)
	}
	s.Auth.Allowlist = nets

	if w := get(t, s, "/admin/login", map[string]string{"X-Forwarded-For": "203.0.113.5"}); w.Code != 200 {
		t.Errorf("白名单内应可访问，got %d", w.Code)
	}
	if w := get(t, s, "/admin/login", map[string]string{"X-Forwarded-For": "198.51.100.9"}); w.Code != 200 {
		t.Errorf("白名单内单IP应可访问，got %d", w.Code)
	}
	for _, ip := range []string{"192.0.2.1", "198.51.100.10"} {
		if w := get(t, s, "/admin/login", map[string]string{"X-Forwarded-For": ip}); w.Code != 404 {
			t.Errorf("白名单外(%s)应404而非403，got %d", ip, w.Code)
		}
	}
	// 白名单外即便凭据正确也进不去
	code, _ := totp.Code(secret, time.Now())
	form, cookies := loginForm(t, s, "admin", testPassword, code) // 用白名单内IP取CSRF
	if w := postForm(t, s, "/admin/login", form, cookies, "192.0.2.1"); w.Code != 404 {
		t.Errorf("白名单外提交登录应404，got %d", w.Code)
	}
}

func TestParseAllowlistErrors(t *testing.T) {
	if _, err := ParseAllowlist("not-an-ip"); err == nil {
		t.Error("非法条目应报错")
	}
	if _, err := ParseAllowlist("10.0.0.0/99"); err == nil {
		t.Error("非法CIDR应报错")
	}
	if nets, err := ParseAllowlist(""); err != nil || len(nets) != 0 {
		t.Errorf("空串应得到空列表: %v %v", nets, err)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	s, fs, secret := newAuthServer(t)
	code, _ := totp.Code(secret, time.Now())
	form, cookies := loginForm(t, s, "admin", testPassword, code)
	w := postForm(t, s, "/admin/login", form, cookies, "203.0.113.5")

	var sess, csrf *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case sessionCookie:
			sess = c
		case csrfCookie:
			csrf = c
		}
	}
	out := postForm(t, s, "/admin/logout", url.Values{"csrf_token": {csrf.Value}},
		[]*http.Cookie{sess, csrf}, "203.0.113.5")
	if out.Code != http.StatusFound {
		t.Fatalf("登出应302，got %d", out.Code)
	}
	if len(fs.sessions) != 0 {
		t.Error("登出应撤销服务端会话")
	}
	// 旧cookie立刻失效（A6）
	req := httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(sess)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("撤销后原cookie应失效并跳登录页，got %d", rec.Code)
	}
}

// §8.5：审计写入失败时登录也失败——不允许存在无审计的特权操作。
func TestLoginFailsWhenAuditFails(t *testing.T) {
	s, fs, secret := newAuthServer(t)
	fs.auditErr = context.DeadlineExceeded
	code, _ := totp.Code(secret, time.Now())
	form, cookies := loginForm(t, s, "admin", testPassword, code)
	w := postForm(t, s, "/admin/login", form, cookies, "203.0.113.5")
	if w.Code == http.StatusFound {
		t.Fatal("审计失败时不得登录成功")
	}
	if len(fs.sessions) != 0 {
		t.Log("注意：会话已创建但登录被拒（会话表里会留下一条无人使用的记录，随TTL自然过期）")
	}
}

// 登录页与管理页都不得把密码/验证码回显进HTML。
func TestNoCredentialEchoInHTML(t *testing.T) {
	s, _, _ := newAuthServer(t)
	form, cookies := loginForm(t, s, "admin", testPassword, "123456")
	w := postForm(t, s, "/admin/login", form, cookies, "203.0.113.5")
	body := w.Body.String()
	if strings.Contains(body, testPassword) || strings.Contains(body, "123456") {
		t.Error("失败页面不得回显密码或验证码")
	}
}
