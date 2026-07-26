package console

import (
	"net/http"
	"net/url"
	"strings"

	"ccw/internal/consolestore"
	"ccw/internal/dns"
)

// C16 域名页：zone与子域名分配的全景，外加**每条待办的DNS记录长什么样**。
//
// 这一页要回答的是三个问题：有哪些zone、哪个子域名给了哪台机器、
// 现在卡在哪条没加的A记录上。第三个是真正会拦住部署的那个——
// dns-allocate不通过，后面九步都不会跑。

func (s *Server) registerDomains(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/domains", s.Auth.requireAdmin(s.adminDomains))
	mux.HandleFunc("POST /admin/domains/zones", s.Auth.requireAdmin(s.adminZoneCreate))
}

type domainRow struct {
	consolestore.DomainRow
	StateText   string
	Tone        string
	Verified    string
	Instruction string // 待生效时给出要添加的记录原文
}

func (s *Server) adminDomains(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	ctx := r.Context()
	zones, err := s.Fleet.Store.ListZones(ctx)
	if err != nil {
		s.Logf("console: 列zone失败: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	domains, err := s.Fleet.Store.ListDomains(ctx)
	if err != nil {
		s.Logf("console: 列域名失败: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	rows := make([]domainRow, 0, len(domains))
	pending := 0
	for _, d := range domains {
		row := domainRow{DomainRow: d, Verified: humanWhen(d.VerifiedAt)}
		switch d.RecordState {
		case "insync":
			row.StateText, row.Tone = "已生效", "ok"
		case "pending":
			row.StateText, row.Tone = "等待添加记录", "warn"
			row.Instruction = dns.Instructions(d.FQDN, d.TargetIP)
			pending++
		default:
			row.StateText, row.Tone = d.RecordState, "idle"
		}
		if row.NodeName == "" {
			row.NodeName = "—"
		}
		rows = append(rows, row)
	}

	nodes, _ := s.Fleet.Store.ListNodes(ctx)
	s.renderAdmin(w, "admin_domains.html", "domains", sess, s.Auth.issueCSRF(w, r), len(nodes),
		map[string]any{
			"Zones": zones, "Domains": rows, "Pending": pending,
			"Error": r.URL.Query().Get("err"), "Notice": r.URL.Query().Get("ok"),
		})
}

func (s *Server) adminZoneCreate(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	if !s.Auth.checkCSRF(r) {
		http.Error(w, "csrf", http.StatusForbidden)
		return
	}
	domain := strings.TrimSpace(r.PostFormValue("domain"))
	prefix := strings.TrimSpace(r.PostFormValue("prefix"))
	if prefix == "" {
		prefix = "api"
	}
	if domain == "" {
		s.redirectDomains(w, r, "", "域名必填")
		return
	}
	// provider固定manual：Route 53自动化是C9，未实施。
	// 这里**不给一个选不动的下拉**假装支持——没实现的东西不出现在界面上。
	if _, err := s.Fleet.Store.CreateZone(r.Context(), domain, "manual", prefix); err != nil {
		s.redirectDomains(w, r, "", "创建失败："+err.Error())
		return
	}
	if aerr := s.Auth.audit(r.Context(), consolestore.AuditEntry{
		Actor: sess.UserID, Action: "zone.create", Target: domain, Result: "ok", ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败: %v", aerr)
	}
	s.redirectDomains(w, r, "已创建 zone "+domain, "")
}

func (s *Server) redirectDomains(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	u := "/admin/domains"
	switch {
	case errMsg != "":
		u += "?err=" + urlQueryEscape(errMsg)
	case notice != "":
		u += "?ok=" + urlQueryEscape(notice)
	}
	http.Redirect(w, r, u, http.StatusFound)
}

// urlQueryEscape把提示语放进查询串。提示语是我们自己拼的固定文案+slug/域名，
// 不含用户自由文本；转义仍然要做，页面渲染时也会被模板转义一次。
func urlQueryEscape(s string) string { return url.QueryEscape(s) }
