package console

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	stdsync "sync"
	"time"

	"ccw/internal/consolestore"
	"ccw/internal/provision"
)

// C17 CDK管理页：在浏览器里看清「哪台机器上有哪些项目、发过哪些CDK」，
// 并直接签发与轮换——此前这些只能SSH上去敲ccwadmin。
//
// **判定逻辑仍然只在节点那一份**：本页的写操作都是远程调用节点上的ccwadmin
// （见internal/provision/admincmd.go），Console不自己实现签发规则。
// Console库里的是镜像，用于列表与/connect解析。

// cdkVault是CDK明文的一次性中转站。
//
// **明文绝不落库**（§8.4）。它从节点回来后只在内存里存活很短一段时间，
// 供页面渲染一次；被取走即删，没被取走的也会过期清掉——
// 否则一个忘了关的标签页就等于把CDK留在了进程内存里。
type cdkVault struct {
	mu    stdsync.Mutex
	items map[string]vaultEntry
}

type vaultEntry struct {
	cdks []provision.IssuedCDK
	at   time.Time
}

// vaultTTL是明文在内存里的最长存活时间。纳管跑完到管理员看页面通常是秒级；
// 给到10分钟足够容错，又不至于让它长期驻留。
const vaultTTL = 10 * time.Minute

func (v *cdkVault) put(key string, c provision.IssuedCDK) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.items == nil {
		v.items = map[string]vaultEntry{}
	}
	v.sweepLocked()
	e := v.items[key]
	e.cdks = append(e.cdks, c)
	e.at = time.Now()
	v.items[key] = e
}

// take取走并清除。**取走即清**是这个结构的全部意义：
// 刷新一次页面CDK就没了，这正是"只显示一次"。
func (v *cdkVault) take(key string) []provision.IssuedCDK {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sweepLocked()
	e, ok := v.items[key]
	if !ok {
		return nil
	}
	delete(v.items, key)
	return e.cdks
}

func (v *cdkVault) sweepLocked() {
	cutoff := time.Now().Add(-vaultTTL)
	for k, e := range v.items {
		if e.at.Before(cutoff) {
			delete(v.items, k)
		}
	}
}

func (s *Server) registerCDKs(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/cdks", s.Auth.requireAdmin(s.adminCDKs))
	mux.HandleFunc("POST /admin/cdks/issue", s.Auth.requireAdmin(s.adminCDKIssue))
	mux.HandleFunc("POST /admin/cdks/rotate", s.Auth.requireAdmin(s.adminCDKRotate))
	mux.HandleFunc("POST /admin/cdks/sync", s.Auth.requireAdmin(s.adminCDKSync))
	mux.HandleFunc("POST /admin/cdks/disable", s.Auth.requireAdmin(s.adminCDKDisable))
}

// projectRow是CDK页的一行：一个项目 + 它名下的CDK。
type projectRow struct {
	consolestore.NodeProject
	DiskGiB int64
	CDKs    []cdkRow
}

type cdkRow struct {
	PublicID  string
	IssuedAt  string
	State     string // 有效 / 已撤销 /（同步后可能是）已过期、已禁用
	Tone      string
	IssuedBy  string
	FromNode  bool // 状态来自节点的权威查询而不是Console镜像
	Truncated string
}

func (s *Server) adminCDKs(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	ctx := r.Context()
	projects, err := s.Fleet.Store.ListNodeProjects(ctx)
	if err != nil {
		s.Logf("console: 列项目失败: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	issues, err := s.Fleet.Store.ListCDKIssues(ctx)
	if err != nil {
		s.Logf("console: 列CDK失败: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	byProject := map[string][]cdkRow{}
	for _, ci := range issues {
		row := cdkRow{
			PublicID: ci.PublicID,
			IssuedAt: ci.IssuedAt.Local().Format("2006-01-02 15:04"),
			State:    "有效", Tone: "ok",
		}
		if ci.RevokedAt != nil {
			row.State, row.Tone = "已撤销", "idle"
		}
		byProject[ci.ProjectID] = append(byProject[ci.ProjectID], row)
	}

	rows := make([]projectRow, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, projectRow{NodeProject: p, DiskGiB: p.DiskLimitBytes >> 30,
			CDKs: byProject[p.ID]})
	}

	// 每台节点的项目数与上限：这是产品硬规则（§7.6的3个），
	// 页面上要看得见，否则只有在ccwadmin拒绝时才知道满了。
	perNode := map[string]int{}
	for _, p := range projects {
		perNode[p.NodeName]++
	}
	type nodeQuota struct {
		Name  string
		Used  int
		Limit int
		Full  bool
	}
	var quotas []nodeQuota
	for name, n := range perNode {
		quotas = append(quotas, nodeQuota{Name: name, Used: n, Limit: 3, Full: n >= 3})
	}
	sort.Slice(quotas, func(i, j int) bool { return quotas[i].Name < quotas[j].Name })

	nodes, _ := s.Fleet.Store.ListNodes(ctx)
	data := map[string]any{
		"Projects": rows,
		"Quotas":   quotas,
		// 刚签发的明文：取走即清，刷新就没了。
		"Issued": s.Fleet.vault.take(sess.UserID),
		"Error":  r.URL.Query().Get("err"),
		"Notice": r.URL.Query().Get("ok"),
	}
	s.renderAdmin(w, "admin_cdks.html", "cdks", sess, s.Auth.issueCSRF(w, r), len(nodes), data)
}

// cdkAction是三个写操作的公共骨架：CSRF → 取项目 → 远程执行 → 审计 → 回列表页。
//
// 审计在**动作之后**写：动作本身发生在节点上，写审计失败不能撤销它，
// 因此如实记录已发生的事，写不进去就当成错误报出来（§8.5）。
func (s *Server) cdkAction(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession,
	action string, do func(ctx context.Context, p consolestore.NodeProject) (string, error)) {
	if !s.Auth.checkCSRF(r) {
		http.Error(w, "csrf", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	p, err := s.Fleet.Store.GetNodeProject(ctx, r.PostFormValue("project_id"))
	if err != nil {
		http.Error(w, "项目不存在", http.StatusNotFound)
		return
	}
	if s.Fleet.Orchestrator == nil {
		s.redirectCDKs(w, r, "", "机队编排器未启用，无法远程执行")
		return
	}

	notice, derr := do(ctx, p)
	result := "ok"
	if derr != nil {
		result = "error"
	}
	if aerr := s.Auth.audit(ctx, consolestore.AuditEntry{
		Actor: sess.UserID, Action: action, Target: p.NodeName + "/" + p.Slug,
		Result: result, ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败: %v", aerr)
	}
	if derr != nil {
		// **错误原文可能来自节点**，但RunAdmin已经把stderr挡掉了，
		// 这里只会是"退出码N"或连接类错误，不含目标信息。
		s.redirectCDKs(w, r, "", derr.Error())
		return
	}
	s.redirectCDKs(w, r, notice, "")
}

func (s *Server) adminCDKIssue(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.cdkAction(w, r, sess, "cdk.issue", func(ctx context.Context, p consolestore.NodeProject) (string, error) {
		issued, err := s.Fleet.Orchestrator.IssueCDK(ctx, p.NodeID, p.Slug)
		if err != nil {
			return "", err
		}
		s.recordIssued(ctx, p, issued, sess.UserID)
		return "已为 " + p.Slug + " 签发新 CDK", nil
	})
}

func (s *Server) adminCDKRotate(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	revokeNow := r.PostFormValue("revoke_now") == "1"
	s.cdkAction(w, r, sess, "cdk.rotate", func(ctx context.Context, p consolestore.NodeProject) (string, error) {
		issued, err := s.Fleet.Orchestrator.RotateCDK(ctx, p.NodeID, p.Slug, revokeNow)
		if err != nil {
			return "", err
		}
		s.recordIssued(ctx, p, issued, sess.UserID)
		if revokeNow {
			// 立即撤销：镜像里同项目的其它CDK一并标记撤销，
			// 否则/connect还会把老public-id解析成可用域名。
			s.revokeOthers(ctx, p.ID, issued.PublicID)
			return "已轮换 " + p.Slug + "，旧 CDK 立即失效", nil
		}
		return "已轮换 " + p.Slug + "，旧 CDK 在宽限期后失效", nil
	})
}

// adminCDKDisable禁用一张CDK。
//
// 与轮换的区别：轮换会签发替代品，这里只让这一张失效。用在
// "某个人不再需要访问"或"这张泄露了但项目还有别的可用CDK"。
func (s *Server) adminCDKDisable(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	publicID := strings.TrimSpace(r.PostFormValue("public_id"))
	s.cdkAction(w, r, sess, "cdk.disable", func(ctx context.Context, p consolestore.NodeProject) (string, error) {
		if publicID == "" {
			return "", errors.New("缺少 public_id")
		}
		if err := s.Fleet.Orchestrator.DisableCDK(ctx, p.NodeID, publicID); err != nil {
			return "", err
		}
		// 节点是权威，镜像跟着标撤销；漏了这步 /connect 还会把它解析成可用域名。
		if err := s.Fleet.Store.RevokeCDKIssue(ctx, publicID); err != nil {
			s.Logf("console: CDK禁用后镜像未标撤销: %v", err)
		}
		return "已禁用 " + publicID, nil
	})
}

// adminCDKSync从节点拉权威状态回来对账。
//
// Console镜像只知道"签发过、撤销过"；过期与禁用发生在节点侧
// （轮换的宽限期到点、disable-cdk），镜像看不见。这个动作把它们同步回来。
func (s *Server) adminCDKSync(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.cdkAction(w, r, sess, "cdk.sync", func(ctx context.Context, p consolestore.NodeProject) (string, error) {
		states, err := s.Fleet.Orchestrator.ListCDKs(ctx, p.NodeID, p.Slug)
		if err != nil {
			return "", err
		}
		var revoked int
		for _, st := range states {
			if st.State == "disabled" || st.State == "expired" {
				if err := s.Fleet.Store.RevokeCDKIssue(ctx, st.PublicID); err != nil {
					return "", err
				}
				revoked++
			} else {
				// 节点上有、镜像里没有的（例如管理员在机器上直接签发过）：补进来，
				// 否则/connect永远解析不到那张CDK。
				if err := s.Fleet.Store.RecordCDKIssue(ctx, p.ID, st.PublicID, ""); err != nil {
					return "", err
				}
			}
		}
		return "已从节点同步 " + p.Slug + " 的 " + itoa(len(states)) + " 张 CDK", nil
	})
}

// recordIssued把新CDK记进镜像，并把明文放进一次性中转站。
// 记账失败不回滚——CDK在节点上已经生效了，如实报出来比假装没发生好。
func (s *Server) recordIssued(ctx context.Context, p consolestore.NodeProject,
	issued provision.IssuedCDK, userID string) {
	if err := s.Fleet.Store.RecordCDKIssue(ctx, p.ID, issued.PublicID, userID); err != nil {
		s.Logf("console: CDK签发记录写入失败（节点上已生效）: %v", err)
	}
	s.Fleet.vault.put(userID, issued)
}

func (s *Server) revokeOthers(ctx context.Context, projectID, keepPublicID string) {
	issues, err := s.Fleet.Store.ListCDKIssues(ctx)
	if err != nil {
		s.Logf("console: 撤销旧CDK镜像失败: %v", err)
		return
	}
	for _, ci := range issues {
		if ci.ProjectID == projectID && ci.PublicID != keepPublicID && ci.RevokedAt == nil {
			if err := s.Fleet.Store.RevokeCDKIssue(ctx, ci.PublicID); err != nil {
				s.Logf("console: 撤销旧CDK镜像失败: %v", err)
			}
		}
	}
}

func (s *Server) redirectCDKs(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	u := "/admin/cdks"
	switch {
	case errMsg != "":
		u += "?err=" + urlQueryEscape(errMsg)
	case notice != "":
		u += "?ok=" + urlQueryEscape(notice)
	}
	http.Redirect(w, r, u, http.StatusFound)
}
