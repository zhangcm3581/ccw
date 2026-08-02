package console

import (
	"fmt"
	"net/http"
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
	// Stale为true表示很久没有新用量了。**这是判断采集有没有在工作的唯一线索**：
	// 采集链路断掉（最常见是 compose 里那个只读挂载漏了）的表现不是报错，
	// 而是"一切正常、表永远是空的"。
	Stale     bool
	LastSeen  string
	FiveHrPct int
	SevenDPct int
}

func (s *Server) registerUsage(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/usage", s.Auth.requireAdmin(s.adminUsage))
}

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	ctx := r.Context()
	nodes, err := s.Fleet.Store.ListNodes(ctx)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	var rows []usageRow
	var errs []string
	for _, n := range nodes {
		if s.Fleet.Orchestrator == nil {
			break
		}
		us, uerr := s.Fleet.Orchestrator.NodeUsage(ctx, n.ID)
		if uerr != nil {
			// 一个节点取不到不该让整页空白——如实列出是哪台、什么原因。
			errs = append(errs, fmt.Sprintf("%s：%v", n.Name, uerr))
			continue
		}
		for _, u := range us {
			rows = append(rows, makeUsageRow(u, n.Name, time.Now()))
		}
	}
	s.renderAdmin(w, "admin_usage.html", "usage", sess, s.Auth.issueCSRF(w, r), len(nodes),
		map[string]any{"Rows": rows, "Errors": errs, "NoOrchestrator": s.Fleet.Orchestrator == nil})
}

// staleAfter是"多久没有新用量就算可疑"。
//
// 采集每 30 秒跑一轮，但**没人用的时候本来就不会有新事件**——所以这个阈值
// 不能太短，否则一个正常的周末会被报成"采集坏了"。取 24 小时：
// 真的断链时它会一直亮着，而正常的空闲不会误报太久。
const staleAfter = 24 * time.Hour

func makeUsageRow(u provision.NodeProjectUsage, node string, now time.Time) usageRow {
	row := usageRow{NodeProjectUsage: u, Node: node, LastSeen: "从未采集到", Stale: true}
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
