package console

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

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

// adminHome是总览：先给出需要动手的事，再给机队、发布与域名的当前状态。
// 刻意不做「节点总数」这类计数卡片——3 台以内的机队里，计数不构成信息，
// 状态与进度才是。
func (s *Server) adminHome(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	ctx := r.Context()
	csrf := s.Auth.issueCSRF(w, r)

	data := map[string]any{
		"Now": time.Now().Local().Format("2006-01-02 15:04"),
	}

	var rows []nodeRow
	var runs []runView
	var attention []attentionItem

	if s.Fleet != nil {
		nodes, err := s.Fleet.Store.ListNodes(ctx)
		if err != nil {
			s.Logf("console: 列节点失败: %v", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		for _, n := range nodes {
			row := s.nodeRow(ctx, n)
			rows = append(rows, row)
			attention = append(attention, s.attentionFor(ctx, row)...)
			// 每台节点取最近一次运行；机队上限 3 台，这个循环规模可控。
			if rs, err := s.Fleet.Store.ListRuns(ctx, n.ID, 2); err == nil {
				for _, run := range rs {
					full, ferr := s.Fleet.Store.GetRun(ctx, run.ID)
					if ferr != nil {
						continue
					}
					runs = append(runs, makeRunView(full, n.Name))
				}
			}
		}
		zones, _ := s.Fleet.Store.ListZones(ctx)
		data["Zones"] = zones
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Started > runs[j].Started })
	if len(runs) > 6 {
		runs = runs[:6]
	}

	if rel, arts, err := s.Store.LatestPublished(ctx); err == nil {
		data["Release"] = rel
		data["ArtifactCount"] = len(arts)
	} else {
		attention = append(attention, attentionItem{
			Tone: "", Title: "还没有发布客户端",
			Detail: "下载页现在是空的。在 Console 主机上构建产物后用 register-release --publish 发布。",
			Link:   "/download", LinkText: "查看下载页",
		})
	}

	data["Nodes"] = rows
	data["Runs"] = runs
	data["Attention"] = attention
	s.renderAdmin(w, "admin_dashboard.html", "dashboard", sess, csrf, len(rows), data)
}

// attentionItem是总览顶部「需要你动手」的一条。
// 只放**当前确实卡着、且有明确下一步**的事，不放泛泛的提醒。
type attentionItem struct {
	Tone, Title, Detail, Link, LinkText string
}

func (s *Server) attentionFor(ctx context.Context, row nodeRow) []attentionItem {
	var out []attentionItem
	switch row.Node.Status {
	case "degraded":
		out = append(out, attentionItem{
			Tone: "bad", Title: row.Node.Name + " 部署未完成",
			Detail: "最近一次运行失败了。打开节点看步骤表，修好外部原因后从断点续跑。",
			Link:   "/admin/nodes/" + row.Node.ID, LinkText: "查看节点",
		})
	case "host_key_changed":
		out = append(out, attentionItem{
			Tone: "bad", Title: row.Node.Name + " 的 host key 变了",
			Detail: "可能是重装，也可能是中间人。已中止全部操作，需要你带外核对指纹后再继续。",
			Link:   "/admin/nodes/" + row.Node.ID, LinkText: "查看指纹",
		})
	}
	if d, err := s.Fleet.Store.DomainByNode(ctx, row.Node.ID); err == nil && d.RecordState == "pending" {
		out = append(out, attentionItem{
			Tone: "warn", Title: "等待 DNS 记录：" + d.FQDN,
			Detail: "部署停在 dns-allocate。添加 A 记录指向 " + d.TargetIP + " 后回到节点页点「继续」。",
			Link:   "/admin/nodes/" + row.Node.ID, LinkText: "查看记录内容",
		})
	}
	return out
}

// renderAdmin把页面数据与侧边栏外壳数据合并后渲染。
func (s *Server) renderAdmin(w http.ResponseWriter, page, nav string,
	sess consolestore.AdminSession, csrf string, nodeCount int, data map[string]any) {
	base := s.baseFor(nav, sess, csrf, nodeCount)
	data["Nav"] = base.Nav
	data["Username"] = base.Username
	data["Initial"] = base.Initial
	data["CSRF"] = base.CSRF
	data["NodeCount"] = base.NodeCount
	s.render(w, page, data)
}
