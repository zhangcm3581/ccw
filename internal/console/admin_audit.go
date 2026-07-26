package console

import (
	"encoding/json"
	"net/http"
	"strconv"

	"ccw/internal/consolestore"
)

// C19 审计页（§8.5的读侧）。
//
// 审计一直在写——登录、纳管、续跑、CDK动作全都记着——但在此之前
// **只能登机用psql看**。一个查不了的审计日志在出事时等于没有。

const auditPageSize = 100

func (s *Server) registerAudit(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/audit", s.Auth.requireAdmin(s.adminAudit))
}

type auditRow struct {
	Actor    string
	Action   string
	Target   string
	Result   string
	Tone     string
	ClientIP string
	At       string
	Detail   string // 紧凑JSON；空对象不显示
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	ctx := r.Context()
	action := r.URL.Query().Get("action")
	result := r.URL.Query().Get("result")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	// 多取一条判断还有没有下一页——比再跑一次COUNT便宜，也不会在
	// 边界上显示一个点进去是空的「下一页」。
	recs, err := s.Auth.Store.ListAudit(ctx, action, result, auditPageSize+1, (page-1)*auditPageSize)
	if err != nil {
		s.Logf("console: 读审计失败: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	hasNext := len(recs) > auditPageSize
	if hasNext {
		recs = recs[:auditPageSize]
	}

	rows := make([]auditRow, 0, len(recs))
	for _, rec := range recs {
		row := auditRow{
			Actor: rec.Actor, Action: rec.Action, Target: rec.Target,
			Result: rec.Result, ClientIP: rec.ClientIP,
			At: rec.At.Local().Format("2006-01-02 15:04:05"),
		}
		if row.Actor == "" {
			row.Actor = "—" // 未登录动作（登录失败本身）
		}
		switch rec.Result {
		case "ok":
			row.Tone = "ok"
		case "denied":
			row.Tone = "warn"
		default:
			row.Tone = "bad"
		}
		if len(rec.Detail) > 0 {
			// detail在写入前已经过redact（§8.5），这里只做紧凑序列化。
			if b, err := json.Marshal(rec.Detail); err == nil && string(b) != "{}" {
				row.Detail = string(b)
			}
		}
		rows = append(rows, row)
	}

	actions, _ := s.Auth.Store.AuditActions(ctx)
	nodeCount := 0
	if s.Fleet != nil {
		if nodes, err := s.Fleet.Store.ListNodes(ctx); err == nil {
			nodeCount = len(nodes)
		}
	}
	s.renderAdmin(w, "admin_audit.html", "audit", sess, s.Auth.issueCSRF(w, r), nodeCount,
		map[string]any{
			"Rows": rows, "Actions": actions,
			"Action": action, "Result": result,
			"Page": page, "HasNext": hasNext, "HasPrev": page > 1,
			"NextPage": page + 1, "PrevPage": page - 1,
			"Query": auditQuery(action, result),
		})
}

// auditQuery把当前过滤条件拼成可以接在分页链接后面的查询串。
func auditQuery(action, result string) string {
	q := ""
	if action != "" {
		q += "&action=" + urlQueryEscape(action)
	}
	if result != "" {
		q += "&result=" + urlQueryEscape(result)
	}
	return q
}
