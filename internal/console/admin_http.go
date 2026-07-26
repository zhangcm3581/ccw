package console

import (
	"errors"
	"net/http"

	"ccw/internal/consolestore"
)

// 管理后台的HTTP层（console-fleet-design §4.1、§8）。
// 当前只有登录/登出与总览骨架；节点纳管等页面随C11/C15实施。

func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/login", s.adminLoginPage)
	mux.HandleFunc("POST /admin/login", s.adminLoginSubmit)
	mux.HandleFunc("POST /admin/logout", s.Auth.requireAdmin(s.adminLogout))
	mux.HandleFunc("GET /admin", s.Auth.requireAdmin(s.adminHome))
	mux.HandleFunc("GET /admin/{$}", s.Auth.requireAdmin(s.adminHome))
}

// adminLoginPage：白名单外一律404（与Caddy一致，不暴露后台存在，§8.3）。
func (s *Server) adminLoginPage(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.ipAllowed(clientIP(r)) {
		http.NotFound(w, r)
		return
	}
	// 已登录直接进总览
	if _, err := s.Auth.currentAdmin(r); err == nil {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	s.render(w, "admin_login.html", map[string]any{"CSRF": s.Auth.issueCSRF(w, r)})
}

func (s *Server) adminLoginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.Auth.ipAllowed(ip) {
		http.NotFound(w, r)
		return
	}
	if !s.Auth.checkCSRF(r) {
		http.Error(w, "csrf", http.StatusForbidden)
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	code := r.PostFormValue("code")

	// 限速：IP与用户名两个维度（§8.1）。超限不透露是哪一维触发。
	if !s.Auth.allowLogin("ip:"+ip, "user:"+username) {
		s.renderLoginError(w, r, "尝试过于频繁，请稍后再试。")
		return
	}

	userID, token, err := s.Auth.login(r.Context(), username, password, code, ip)
	if err != nil {
		// 审计失败尝试；**不记录密码与验证码**，用户名记入target供排查撞库。
		if aerr := s.Auth.audit(r.Context(), consolestore.AuditEntry{
			Action: "admin.login", Target: username, Result: "denied", ClientIP: ip,
		}); aerr != nil {
			s.Logf("console: 审计写入失败: %v", aerr)
		}
		if err.Error() != "invalid_credentials" {
			// 基础设施错误（数据库、密钥解不开）：日志留痕，页面仍给统一提示。
			s.Logf("console: 登录处理失败: %v", err)
		}
		// 统一错误（§8.1、A4）：不区分用户不存在/密码错/TOTP错。
		s.renderLoginError(w, r, "用户名、密码或验证码不正确。")
		return
	}

	// 审计写入失败＝登录失败（§8.5：不允许无审计的特权操作）。
	if aerr := s.Auth.audit(r.Context(), consolestore.AuditEntry{
		Actor: userID, Action: "admin.login", Target: username, Result: "ok", ClientIP: ip,
	}); aerr != nil {
		s.Logf("console: 审计写入失败，登录中止: %v", aerr)
		s.renderLoginError(w, r, "服务暂时不可用，请稍后再试。")
		return
	}

	s.Auth.setCookie(w, sessionCookie, token, int(sessionAbsolute.Seconds()))
	s.Auth.issueCSRF(w, r)
	http.Redirect(w, r, "/admin", http.StatusFound)
}

func (s *Server) renderLoginError(w http.ResponseWriter, r *http.Request, msg string) {
	w.WriteHeader(http.StatusUnauthorized)
	s.render(w, "admin_login.html", map[string]any{"CSRF": s.Auth.issueCSRF(w, r), "Error": msg})
}

func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if rerr := s.Auth.Store.RevokeSession(r.Context(), hashToken(c.Value)); rerr != nil &&
			!errors.Is(rerr, consolestore.ErrNotFound) {
			s.Logf("console: 撤销会话失败: %v", rerr)
		}
	}
	if aerr := s.Auth.audit(r.Context(), consolestore.AuditEntry{
		Actor: sess.UserID, Action: "admin.logout", Target: sess.Username, Result: "ok", ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败: %v", aerr)
	}
	s.Auth.setCookie(w, sessionCookie, "", -1)
	s.Auth.setCookie(w, csrfCookie, "", -1)
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

func (s *Server) adminHome(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.render(w, "admin_home.html", map[string]any{"Session": sess, "CSRF": s.Auth.issueCSRF(w, r)})
}
