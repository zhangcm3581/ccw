package console

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccw/internal/consolestore"
	"ccw/internal/provision"
)

// 用量页（2026-08-02）。
//
// 此前用量只在节点的库里，要看得登机敲 SQL。这一页把它取回来。
//
// **两套数必须分开呈现**：
//   - token 数是**真实的**，逐条来自 Claude 写的会话 JSONL；
//   - 内部额度单位是本仓库自己算的（token × CCW_USAGE_WEIGHTS），闸门用它，
//     但它是估算口径，不等于 Claude 账号的真实消耗（spec §10 明令不得
//     标成官方订阅百分比）。
//
// 混在一起最容易造成的误解是"我这个月一共用了 3 万"——那个数是估算单位，
// 不是钱也不是官方额度。

type usageRow struct {
	provision.NodeProjectUsage
	Node string
	// NodeID是这一行所属的节点。**每行各带一个**：档位表与 quota_tiers 都是
	// 节点本地的，用全页共用的一个 node 去指派，多节点时会把项目挂到
	// 另一台机器上——那台要么没这个 slug（报错），要么有同名项目（改错人）。
	NodeID string
	// Stale为true表示很久没有新用量了。**这是判断采集有没有在工作的唯一线索**：
	// 采集链路断掉（最常见是 compose 里那个只读挂载漏了）的表现不是报错，
	// 而是"一切正常、表永远是空的"。
	Stale     bool
	LastSeen  string
	FiveHrPct int
	SevenDPct int
}

func (s *Server) registerUsage(mux *http.ServeMux) {
	// **用量挂在节点下面**：项目、档位表（quota_tiers）、Claude 账号全都是
	// 节点本地的东西，平铺成一页会让"这个设置对哪台生效"变得含糊——
	// 之前那两个 bug（档位指派发到错的节点、档位表看着像全机队设置）
	// 正是从那个平铺结构来的。按节点分开之后，页面上只有一台，不会指错。
	mux.HandleFunc("GET /admin/nodes/{id}/usage", s.Auth.requireAdmin(s.adminUsage))
	// 旧地址仍指过来：导航与书签不该变死链。
	mux.HandleFunc("GET /admin/usage", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/nodes", http.StatusFound)
	})
	mux.HandleFunc("POST /admin/usage/tier", s.Auth.requireAdmin(s.adminSetTier))
	mux.HandleFunc("POST /admin/usage/assign", s.Auth.requireAdmin(s.adminAssignTier))
}

// adminSetTier改一个档位的百分比。
//
// **改完立刻影响全部挂该档位的项目**——限额是推导出来的，闸门下一轮（30秒内）
// 就按新值判。所以要审计。
func (s *Server) adminSetTier(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.tierAction(w, r, sess, "quota.tier.set", func(nodeID string) error {
		name := strings.TrimSpace(r.PostFormValue("name"))
		pct, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("percent")), 64)
		if err != nil || pct <= 0 || pct > 100 {
			return errBadPercent
		}
		return s.Fleet.Orchestrator.SetNodeTier(r.Context(), nodeID, name, pct)
	})
}

// adminAssignTier把项目挂到档位；tier 为空表示改回绝对限额。
func (s *Server) adminAssignTier(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.tierAction(w, r, sess, "quota.tier.assign", func(nodeID string) error {
		return s.Fleet.Orchestrator.AssignNodeTier(r.Context(), nodeID,
			strings.TrimSpace(r.PostFormValue("slug")), strings.TrimSpace(r.PostFormValue("tier")))
	})
}

func usageURL(nodeID string) string { return "/admin/nodes/" + nodeID + "/usage" }

var errBadPercent = errors.New("百分比需在 (0,100]——0 会让该档位的项目立刻全员受限")

func (s *Server) tierAction(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession,
	action string, do func(nodeID string) error) {
	if !s.Auth.checkCSRF(r) {
		http.Error(w, "csrf", http.StatusForbidden)
		return
	}
	nodeID := strings.TrimSpace(r.PostFormValue("node"))
	if s.Fleet.Orchestrator == nil || nodeID == "" {
		http.Redirect(w, r, "/admin/nodes?err="+urlQueryEscape("机队编排器未启用或未指定节点"), http.StatusFound)
		return
	}
	if aerr := s.Auth.audit(r.Context(), consolestore.AuditEntry{
		Actor: sess.UserID, Action: action, Target: nodeID, Result: "ok",
		Detail: map[string]any{"name": r.PostFormValue("name"), "slug": r.PostFormValue("slug"),
			"tier": r.PostFormValue("tier"), "percent": r.PostFormValue("percent")},
		ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败，档位改动中止: %v", aerr)
		http.Redirect(w, r, usageURL(nodeID)+"?err="+urlQueryEscape("服务暂时不可用"), http.StatusFound)
		return
	}
	if err := do(nodeID); err != nil {
		http.Redirect(w, r, usageURL(nodeID)+"?err="+urlQueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, usageURL(nodeID)+"?ok="+urlQueryEscape("已更新；闸门在下一轮（约30秒内）按新值判定"), http.StatusFound)
}

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	ctx := r.Context()
	node, err := s.Fleet.Store.GetNode(ctx, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	nodes, _ := s.Fleet.Store.ListNodes(ctx)

	var rows []usageRow
	var errs []string
	var tiers []provision.QuotaTier
	var account provision.ClaudeAccount
	if s.Fleet.Orchestrator != nil {
		if ts, terr := s.Fleet.Orchestrator.NodeTiers(ctx, node.ID); terr == nil {
			tiers = ts
		}
		us, uerr := s.Fleet.Orchestrator.NodeUsage(ctx, node.ID)
		if uerr != nil {
			errs = append(errs, uerr.Error())
		}
		var containers []string
		for _, u := range us {
			rows = append(rows, makeUsageRow(u, node.Name, node.ID, time.Now()))
			containers = append(containers, "ccw-"+u.Slug)
		}
		if len(containers) > 0 {
			if a, aerr := s.Fleet.Orchestrator.ClaudeAccountInfo(ctx, node.ID, containers); aerr == nil {
				account = a
			}
		}
	}
	s.renderAdmin(w, "admin_usage.html", "nodes", sess, s.Auth.issueCSRF(w, r), len(nodes),
		map[string]any{
			"Node": node, "Rows": rows, "Errors": errs,
			"NoOrchestrator": s.Fleet.Orchestrator == nil,
			"Tiers":          tiers, "NodeID": node.ID,
			"Account":    account,
			"AccountAge": humanWhen(&account.SnapshotAt),
			"Notice":     r.URL.Query().Get("ok"), "Error": r.URL.Query().Get("err"),
		})
}

// staleAfter是"多久没有新用量就算可疑"。
//
// 采集每 30 秒跑一轮，但**没人用的时候本来就不会有新事件**——所以这个阈值
// 不能太短，否则一个正常的周末会被报成"采集坏了"。取 24 小时：
// 真的断链时它会一直亮着，而正常的空闲不会误报太久。
const staleAfter = 24 * time.Hour

func makeUsageRow(u provision.NodeProjectUsage, node, nodeID string, now time.Time) usageRow {
	row := usageRow{NodeProjectUsage: u, Node: node, NodeID: nodeID, LastSeen: "从未采集到", Stale: true}
	if u.LastEventAt != nil {
		row.LastSeen = humanWhen(u.LastEventAt)
		row.Stale = now.Sub(*u.LastEventAt) > staleAfter
	}
	row.FiveHrPct = pctOf(u.FiveHour.Weighted, u.FiveHourLim)
	row.SevenDPct = pctOf(u.SevenDay.Weighted, u.SevenDayLim)
	return row
}

// pctOf算占比。限额为0（未配置）时返回0而不是除零。
func pctOf(used, limit int64) int {
	if limit <= 0 {
		return 0
	}
	p := int(float64(used) / float64(limit) * 100)
	if p > 100 {
		return 100
	}
	if p < 0 {
		return 0
	}
	return p
}
