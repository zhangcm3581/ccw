package console

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	stdsync "sync"
	"time"

	"ccw/internal/consolestore"
	"ccw/internal/deploy"
	"ccw/internal/dns"
	"ccw/internal/provision"
)

// 机队管理页（console-fleet-design §4.1、§5.1、C15）。
// 只有Fleet被注入时才注册这些路由——与Auth同款约束：能力不齐就不上页面。

// FleetStore是机队页需要的存储能力面。
type FleetStore interface {
	ListNodes(ctx context.Context) ([]consolestore.Node, error)
	GetNode(ctx context.Context, id string) (consolestore.Node, error)
	CreateNode(ctx context.Context, name, host string, port int, user string) (string, error)
	DomainByNode(ctx context.Context, nodeID string) (consolestore.NodeDomain, error)
	NodeCredential(ctx context.Context, nodeID string) ([]byte, []byte, string, error)
	ListZones(ctx context.Context) ([]dns.Zone, error)
	CreateZone(ctx context.Context, domain, provider, prefix string) (string, error)
	ListRuns(ctx context.Context, nodeID string, limit int) ([]consolestore.RunSummary, error)
	GetRun(ctx context.Context, runID string) (consolestore.RunSummary, error)

	// 域名页
	ListDomains(ctx context.Context) ([]consolestore.DomainRow, error)
	// CDK页：项目与签发事件的镜像。**只有public_id，没有明文**（§8.4）。
	ListNodeProjects(ctx context.Context) ([]consolestore.NodeProject, error)
	GetNodeProject(ctx context.Context, id string) (consolestore.NodeProject, error)
	ListCDKIssues(ctx context.Context) ([]consolestore.CDKIssue, error)
	RecordCDKIssue(ctx context.Context, projectID, publicID, issuedBy string) error
	RevokeCDKIssue(ctx context.Context, publicID string) error
	// RetireNode解除纳管：退役域名并删节点行。**不碰远端机器**。
	RetireNode(ctx context.Context, nodeID string) error
	// UpsertNodeProject供「从节点同步项目」补齐镜像。
	UpsertNodeProject(ctx context.Context, nodeID, slug, remoteID string,
		diskBytes, fiveHour, sevenDay int64) (string, error)
}

// Fleet持有机队页的依赖。
type Fleet struct {
	Store        FleetStore
	Orchestrator *provision.Orchestrator
	Logs         *LogHub
	// LastInput记住每个节点最近一次纳管的输入，供「继续部署」复用
	// （不含密码——续跑时已有托管密钥，不需要密码）。
	lastInput map[string]provision.BootstrapInput
	mu        stdsync.Mutex
	// vault是CDK明文的一次性中转站：只在内存、取走即清（§8.4）。
	vault cdkVault
	// diag是最近一次节点诊断的结果，screen是授权会话的最近一屏。
	// **都不是凭据**，所以不做取走即清——刷新页面还能看到，方便对照着排查。
	//
	// 为什么不经查询串回传：一屏中文终端输出百分号编码后接近 10 KB，
	// 超过常见反代 8192 的请求行上限，会变成 414 而不是页面。实测见提交说明。
	diagMu stdsync.Mutex
	diag   map[string][]provision.DiagSection
	screen map[string]string
}

func (f *Fleet) putScreen(nodeID, s string) {
	f.diagMu.Lock()
	defer f.diagMu.Unlock()
	if f.screen == nil {
		f.screen = map[string]string{}
	}
	if s == "" {
		delete(f.screen, nodeID)
		return
	}
	f.screen[nodeID] = s
}

func (f *Fleet) getScreen(nodeID string) string {
	f.diagMu.Lock()
	defer f.diagMu.Unlock()
	return f.screen[nodeID]
}

func (f *Fleet) putDiag(nodeID string, secs []provision.DiagSection) {
	f.diagMu.Lock()
	defer f.diagMu.Unlock()
	if f.diag == nil {
		f.diag = map[string][]provision.DiagSection{}
	}
	f.diag[nodeID] = secs
}

func (f *Fleet) getDiag(nodeID string) []provision.DiagSection {
	f.diagMu.Lock()
	defer f.diagMu.Unlock()
	return f.diag[nodeID]
}

// StashCDK把纳管过程中签发的CDK明文交给UI一次性展示。
// 接给Orchestrator.OnCDKIssued；键用runID，运行详情页据此取。
func (f *Fleet) StashCDK(runID, slug, publicID, cdk string) {
	f.vault.put(runID, provision.IssuedCDK{Slug: slug, PublicID: publicID, CDK: cdk})
}

func (s *Server) registerFleet(mux *http.ServeMux) {
	s.registerDomains(mux)
	s.registerCDKs(mux)
	s.registerClaudeAuth(mux)
	s.registerDiag(mux)
	mux.HandleFunc("GET /admin/nodes", s.Auth.requireAdmin(s.adminNodes))
	mux.HandleFunc("GET /admin/nodes/new", s.Auth.requireAdmin(s.adminNodeNew))
	mux.HandleFunc("POST /admin/nodes/new", s.Auth.requireAdmin(s.adminNodeCreate))
	mux.HandleFunc("GET /admin/nodes/{id}", s.Auth.requireAdmin(s.adminNodeDetail))
	mux.HandleFunc("POST /admin/nodes/{id}/resume", s.Auth.requireAdmin(s.adminNodeResume))
	mux.HandleFunc("POST /admin/nodes/{id}/delete", s.Auth.requireAdmin(s.adminNodeDelete))
	mux.HandleFunc("GET /admin/nodes/{id}/runs/{run}", s.Auth.requireAdmin(s.adminRunDetail))
	mux.HandleFunc("GET /admin/nodes/{id}/runs/{run}/stream", s.Auth.requireAdmin(s.adminRunStream))
}

type nodeRow struct {
	Node       consolestore.Node
	FQDN       string
	StatusText string
	Tone       string
	LastSeen   string
	OSRelease  string
}

func (s *Server) nodeRow(ctx context.Context, n consolestore.Node) nodeRow {
	text, tone := nodeTone(n.Status)
	r := nodeRow{Node: n, StatusText: text, Tone: tone, LastSeen: humanWhen(n.LastSeenAt)}
	if n.OSRelease != nil {
		r.OSRelease = *n.OSRelease
	}
	if d, err := s.Fleet.Store.DomainByNode(ctx, n.ID); err == nil {
		r.FQDN = d.FQDN
	}
	return r
}

func (s *Server) adminNodes(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	nodes, err := s.Fleet.Store.ListNodes(r.Context())
	if err != nil {
		s.Logf("console: 列节点失败: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	zones, _ := s.Fleet.Store.ListZones(r.Context())
	rows := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, s.nodeRow(r.Context(), n))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Node.Name < rows[j].Node.Name })
	s.renderAdmin(w, "admin_nodes.html", "nodes", sess, s.Auth.issueCSRF(w, r), len(rows),
		map[string]any{"Nodes": rows, "Zones": zones})
}

func (s *Server) adminNodeNew(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	zones, _ := s.Fleet.Store.ListZones(r.Context())
	nodes, _ := s.Fleet.Store.ListNodes(r.Context())
	s.renderAdmin(w, "admin_node_new.html", "nodes", sess, s.Auth.issueCSRF(w, r), len(nodes),
		map[string]any{"Zones": zones, "Steps": plannedSteps()})
}

func (s *Server) adminNodeCreate(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	ctx := r.Context()
	renderErr := func(msg string) {
		zones, _ := s.Fleet.Store.ListZones(ctx)
		nodes, _ := s.Fleet.Store.ListNodes(ctx)
		w.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(w, "admin_node_new.html", "nodes", sess, s.Auth.issueCSRF(w, r), len(nodes),
			map[string]any{"Zones": zones, "Steps": plannedSteps(), "Error": msg})
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	host := strings.TrimSpace(r.PostFormValue("host"))
	user := strings.TrimSpace(r.PostFormValue("ssh_user"))
	password := r.PostFormValue("password")
	privateKey := strings.TrimSpace(r.PostFormValue("private_key"))
	// 首登凭据用完即弃（§8.4）：**永不落库、永不进日志**。
	// 它们会被复制进BootstrapInput交给纳管goroutine（凭据交接需要），
	// 那个结构体是瞬时的、随goroutine结束一起回收；下面的ZeroString只清本函数
	// 这一份副本——Go的字符串不可变，无法真正擦除内存，这是尽力而为不是保证。
	defer provision.ZeroString(&password)
	defer provision.ZeroString(&privateKey)

	port := 22
	if v := strings.TrimSpace(r.PostFormValue("ssh_port")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			renderErr("SSH端口不合法")
			return
		}
		port = n
	}
	if name == "" || host == "" || user == "" {
		renderErr("节点名称、公网 IP 与 SSH 用户名都必填")
		return
	}
	// 密码与私钥二选一即可。**云厂商默认镜像多半禁掉了密码登录**
	// （DigitalOcean/AWS/GCP 出厂就是 PasswordAuthentication no），
	// 那种机器只能走私钥——所以这里不能强制要密码。
	if password == "" && privateKey == "" {
		renderErr("请填写 SSH 密码，或粘贴一把能登录这台机器的私钥")
		return
	}

	// slug校验与ccwadmin/渲染器共用同一条规则，避免建完节点才在节点侧被拒。
	var slugs []string
	for _, sl := range strings.Split(r.PostFormValue("slugs"), ",") {
		if sl = strings.TrimSpace(sl); sl != "" {
			slugs = append(slugs, sl)
		}
	}
	if err := deploy.ValidateSlugs(slugs); err != nil {
		renderErr(err.Error())
		return
	}

	zoneID := r.PostFormValue("zone_id")
	if nz := strings.TrimSpace(r.PostFormValue("new_zone_domain")); nz != "" {
		id, err := s.Fleet.Store.CreateZone(ctx, nz, "manual", "api")
		if err != nil {
			renderErr("创建zone失败：" + err.Error())
			return
		}
		zoneID = id
	}
	if zoneID == "" {
		renderErr("请选择或创建一个zone")
		return
	}

	nodeID, err := s.Fleet.Store.CreateNode(ctx, name, host, port, user)
	if err != nil {
		renderErr("创建节点失败：" + err.Error())
		return
	}
	if aerr := s.Auth.audit(ctx, consolestore.AuditEntry{
		Actor: sess.UserID, Action: "node.bootstrap", Target: name, Result: "ok",
		Detail: map[string]any{"host": host, "slugs": slugs}, ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败，纳管中止: %v", aerr)
		renderErr("服务暂时不可用，请稍后再试")
		return
	}

	in := provision.BootstrapInput{
		NodeID: nodeID, ZoneID: zoneID,
		Password: password, PrivateKey: privateKey,
		Slugs: slugs, TriggeredBy: sess.UserID,
	}
	runID, err := s.Fleet.Orchestrator.Bootstrap(ctx, in)
	if err != nil {
		renderErr("启动部署失败：" + err.Error())
		return
	}
	// 记住输入供「继续部署」复用；**不含密码**（续跑用托管密钥）。
	s.Fleet.remember(nodeID, provision.BootstrapInput{
		NodeID: nodeID, ZoneID: zoneID, Slugs: slugs, TriggeredBy: sess.UserID,
	})
	http.Redirect(w, r, fmt.Sprintf("/admin/nodes/%s/runs/%s", nodeID, runID), http.StatusFound)
}

func (f *Fleet) remember(nodeID string, in provision.BootstrapInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastInput == nil {
		f.lastInput = map[string]provision.BootstrapInput{}
	}
	f.lastInput[nodeID] = in
}

func (f *Fleet) recall(nodeID string) (provision.BootstrapInput, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.lastInput[nodeID]
	return in, ok
}

func (s *Server) adminNodeDetail(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	ctx := r.Context()
	node, err := s.Fleet.Store.GetNode(ctx, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	row := s.nodeRow(ctx, node)
	runs, _ := s.Fleet.Store.ListRuns(ctx, node.ID, 20)
	var rvs []runView
	for _, run := range runs {
		full, err := s.Fleet.Store.GetRun(ctx, run.ID)
		if err != nil {
			continue
		}
		rvs = append(rvs, makeRunView(full, node.Name))
	}
	_, _, _, cerr := s.Fleet.Store.NodeCredential(ctx, node.ID)
	d, derr := s.Fleet.Store.DomainByNode(ctx, node.ID)
	nodes, _ := s.Fleet.Store.ListNodes(ctx)

	// 授权Claude账号是纳管之后必须手动做的一步（DEPLOY.md的A7），后台还没法代劳。
	// 但Console库里有项目的remote_project_id——那正是tmux的socket名——
	// 所以命令可以填好了给出来，省掉"上机查id再粘回来"这一趟。
	var authProjects []map[string]string
	if all, err := s.Fleet.Store.ListNodeProjects(ctx); err == nil {
		for _, p := range all {
			if p.NodeID == node.ID {
				authProjects = append(authProjects, map[string]string{
					"Slug": p.Slug, "Container": "ccw-" + p.Slug, "RemoteID": p.RemoteProjectID,
				})
			}
		}
	}

	data := map[string]any{
		"Node": node, "StatusText": row.StatusText, "Tone": row.Tone,
		"FQDN": row.FQDN, "LastSeen": row.LastSeen, "Runs": rvs,
		"HasCredential": cerr == nil,
		"CanResume":     node.Status != "provisioning",
		"AuthProjects":  authProjects,
		"SSHTarget":     node.SSHUser + "@" + node.Host,
		"Screen":        s.Fleet.getScreen(node.ID),
		"Diag":          s.Fleet.getDiag(node.ID),
		"Error":         r.URL.Query().Get("err"),
		"Notice":        r.URL.Query().Get("ok"),
	}
	if derr == nil && d.RecordState == "pending" {
		data["DomainPending"] = true
		data["DNSInstruction"] = dns.Instructions(d.FQDN, d.TargetIP)
	}
	s.renderAdmin(w, "admin_node.html", "nodes", sess, s.Auth.issueCSRF(w, r), len(nodes), data)
}

// adminNodeResume从上次失败处继续部署（A9）。
// 续跑不需要密码——托管密钥已经在库里；若凭据交接那一步就失败了，
// 则需要重新走「新增节点」（会再要一次密码）。
func (s *Server) adminNodeResume(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	nodeID := r.PathValue("id")
	in, ok := s.Fleet.recall(nodeID)
	if !ok {
		// Console重启后内存里的输入没了：从最近一次运行恢复不了slug列表，
		// 如实告诉管理员而不是用猜的参数去跑。
		http.Error(w, "本次Console启动后没有该节点的部署参数，请重新走「新增节点」流程", http.StatusBadRequest)
		return
	}
	if _, _, _, err := s.Fleet.Store.NodeCredential(r.Context(), nodeID); err != nil {
		http.Error(w, "该节点尚未建立托管密钥，请重新走「新增节点」流程（需要再次输入密码）", http.StatusBadRequest)
		return
	}
	if aerr := s.Auth.audit(r.Context(), consolestore.AuditEntry{
		Actor: sess.UserID, Action: "node.resume", Target: nodeID, Result: "ok", ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败: %v", aerr)
		http.Error(w, "服务暂时不可用", http.StatusInternalServerError)
		return
	}
	in.TriggeredBy = sess.UserID
	in.Kind = "resume" // 续跑与首次纳管在运行列表里是两件事
	runID, err := s.Fleet.Orchestrator.Bootstrap(r.Context(), in)
	if err != nil {
		http.Error(w, "启动失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/nodes/%s/runs/%s", nodeID, runID), http.StatusFound)
}

// adminNodeDelete解除纳管。
//
// **只清Console这边的账，绝不碰远端机器**：容器、数据卷、Claude凭据都还在
// 那台服务器上跑，要真正下线得自己登机处理。让后台去销毁一台还在服务的机器，
// 是无法撤销且后果不成比例的操作——这条边界在页面上也写明了。
//
// 要求把节点名原样打一遍才执行：删除不可撤销，而误点一个红按钮太容易。
func (s *Server) adminNodeDelete(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	if !s.Auth.checkCSRF(r) {
		http.Error(w, "csrf", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	nodeID := r.PathValue("id")
	node, err := s.Fleet.Store.GetNode(ctx, nodeID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.TrimSpace(r.PostFormValue("confirm")) != node.Name {
		s.redirectNode(w, r, nodeID, "", "确认文本与节点名不一致，未执行删除")
		return
	}
	// 审计**先于**动作：删完就没有节点行了，事后再记会记不上是谁删的哪台。
	if aerr := s.Auth.audit(ctx, consolestore.AuditEntry{
		Actor: sess.UserID, Action: "node.retire", Target: node.Name, Result: "ok",
		Detail: map[string]any{"host": node.Host}, ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败，删除中止: %v", aerr)
		s.redirectNode(w, r, nodeID, "", "服务暂时不可用，请稍后再试")
		return
	}
	if err := s.Fleet.Store.RetireNode(ctx, nodeID); err != nil {
		s.Logf("console: 解除纳管失败: %v", err)
		s.redirectNode(w, r, nodeID, "", "解除纳管失败："+err.Error())
		return
	}
	http.Redirect(w, r, "/admin/nodes?ok="+urlQueryEscape("已解除纳管 "+node.Name+"（远端机器未改动）"),
		http.StatusFound)
}

func (s *Server) adminRunDetail(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	run, err := s.Fleet.Store.GetRun(r.Context(), r.PathValue("run"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	node, err := s.Fleet.Store.GetNode(r.Context(), r.PathValue("id"))
	if err != nil || run.NodeID != node.ID {
		http.NotFound(w, r) // 防止用别的节点ID拼URL看到不属于它的运行
		return
	}
	steps, progress := makeStepViews(run)
	statusText, tone := runTone(run.Status)
	nodes, _ := s.Fleet.Store.ListNodes(r.Context())
	s.renderAdmin(w, "admin_run.html", "nodes", sess, s.Auth.issueCSRF(w, r), len(nodes),
		map[string]any{
			"Run": run, "NodeID": node.ID, "NodeName": node.Name,
			"Started": run.StartedAt.Local().Format("2006-01-02 15:04:05"),
			"History": s.Fleet.Logs.History(run.ID),
			"Steps":   steps, "Progress": progress,
			"StatusText": statusText, "Tone": tone,
			// 本次纳管签发的CDK明文：**取走即清**，刷新页面就没了（§8.4）。
			// 这是明文唯一的交付路径——之前它只能从节点侧输出人工取回。
			"Issued": s.Fleet.vault.take(run.ID),
			// 「实时」以数据库里的运行状态为准，内存里的 done 标记只是补充：
			// Console 重启后 LogHub 里没有这次运行的任何记录，
			// 只看 IsDone 会把一次早已结束的运行显示成还在跑。
			"Live": run.Status == "running" && !s.Fleet.Logs.IsDone(run.ID),
		})
}

// adminRunStream是SSE端点（§5.4）：把流水线日志实时推给浏览器。
func (s *Server) adminRunStream(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	runID := r.PathValue("run")
	run, err := s.Fleet.Store.GetRun(r.Context(), runID)
	if err != nil || run.NodeID != r.PathValue("id") {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 防止反代缓冲整条流

	history, ch, cancel := s.Fleet.Logs.Subscribe(runID)
	defer cancel()
	for _, line := range history {
		writeSSE(w, "message", line)
	}
	flusher.Flush()
	if s.Fleet.Logs.IsDone(runID) {
		writeSSE(w, "done", "")
		flusher.Flush()
		return
	}

	// 心跳：反代与浏览器都会掐断长时间无数据的连接。
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case line := <-ch:
			if line == doneMarker {
				writeSSE(w, "done", "")
				flusher.Flush()
				return
			}
			writeSSE(w, "message", line)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// writeSSE按SSE格式输出。多行内容要逐行加data:前缀，否则浏览器只收到第一行。
func writeSSE(w http.ResponseWriter, event, data string) {
	if event != "message" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}
