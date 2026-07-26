package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"ccw/internal/consolestore"
	"ccw/internal/dns"
	"ccw/internal/provision"
	"ccw/internal/totp"
)

type fakeFleetStore struct {
	nodes   map[string]consolestore.Node
	zones   []dns.Zone
	domains map[string]consolestore.NodeDomain
	creds   map[string]bool
	runs    map[string]consolestore.RunSummary
	created []string
}

func newFleetStore() *fakeFleetStore {
	return &fakeFleetStore{
		nodes: map[string]consolestore.Node{}, domains: map[string]consolestore.NodeDomain{},
		creds: map[string]bool{}, runs: map[string]consolestore.RunSummary{},
	}
}

func (f *fakeFleetStore) ListNodes(context.Context) ([]consolestore.Node, error) {
	var out []consolestore.Node
	for _, n := range f.nodes {
		out = append(out, n)
	}
	return out, nil
}
func (f *fakeFleetStore) GetNode(_ context.Context, id string) (consolestore.Node, error) {
	n, ok := f.nodes[id]
	if !ok {
		return consolestore.Node{}, consolestore.ErrNotFound
	}
	return n, nil
}
func (f *fakeFleetStore) CreateNode(_ context.Context, name, host string, port int, user string) (string, error) {
	id := "node-" + name
	f.nodes[id] = consolestore.Node{ID: id, Name: name, Host: host, SSHPort: port, SSHUser: user, Status: "new"}
	f.created = append(f.created, name)
	return id, nil
}
func (f *fakeFleetStore) DomainByNode(_ context.Context, nodeID string) (consolestore.NodeDomain, error) {
	d, ok := f.domains[nodeID]
	if !ok {
		return consolestore.NodeDomain{}, consolestore.ErrNotFound
	}
	return d, nil
}
func (f *fakeFleetStore) NodeCredential(_ context.Context, nodeID string) ([]byte, []byte, string, error) {
	if !f.creds[nodeID] {
		return nil, nil, "", consolestore.ErrNotFound
	}
	return []byte("enc"), []byte("nonce"), "ssh-ed25519 AAAA", nil
}
func (f *fakeFleetStore) ListZones(context.Context) ([]dns.Zone, error) { return f.zones, nil }
func (f *fakeFleetStore) CreateZone(_ context.Context, domain, provider, prefix string) (string, error) {
	id := "zone-" + domain
	f.zones = append(f.zones, dns.Zone{ID: id, Domain: domain, Provider: provider, SubdomainPrefix: prefix})
	return id, nil
}
func (f *fakeFleetStore) ListRuns(_ context.Context, nodeID string, _ int) ([]consolestore.RunSummary, error) {
	var out []consolestore.RunSummary
	for _, r := range f.runs {
		if r.NodeID == nodeID {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeFleetStore) GetRun(_ context.Context, runID string) (consolestore.RunSummary, error) {
	r, ok := f.runs[runID]
	if !ok {
		return consolestore.RunSummary{}, consolestore.ErrNotFound
	}
	return r, nil
}

// newFleetServer返回一个已登录的Server与会话cookie。
func newFleetServer(t *testing.T) (*Server, *fakeFleetStore, *http.Cookie, *http.Cookie) {
	t.Helper()
	s, _, secret := newAuthServer(t)
	fs := newFleetStore()
	s.Fleet = &Fleet{Store: fs, Logs: NewLogHub("")}

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
	if sess == nil {
		t.Fatal("登录失败")
	}
	return s, fs, sess, csrf
}

func authGet(t *testing.T, s *Server, path string, sess *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	req.AddCookie(sess)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// authGetCtx用于SSE这类长连接：到期自动断开，避免测试挂死。
func authGetCtx(t *testing.T, s *Server, path string, sess *http.Cookie, d time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	req.AddCookie(sess)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// SSE：已结束的运行应立即发done并关闭，而不是把浏览器吊在那里。
func TestRunStreamFinishedRunClosesImmediately(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	fs.runs["r1"] = consolestore.RunSummary{ID: "r1", NodeID: "n1", Status: "succeeded"}
	s.Fleet.Logs.Append("r1", "步骤完成")
	s.Fleet.Logs.Finish("r1")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- authGetCtx(t, s, "/admin/nodes/n1/runs/r1/stream", sess, 5*time.Second) }()
	select {
	case w := <-done:
		body := w.Body.String()
		if !strings.Contains(body, "步骤完成") || !strings.Contains(body, "event: done") {
			t.Errorf("应回放历史并立即发done: %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("已结束的运行不应让SSE挂住")
	}
}

// Fleet为nil时机队路由不存在（与Auth同款约束）。
func TestFleetRoutesAbsentWithoutFleet(t *testing.T) {
	s, _, _ := newAuthServer(t)
	if w := get(t, s, "/admin/nodes", nil); w.Code != 404 {
		t.Errorf("未配置Fleet时/admin/nodes应404，got %d", w.Code)
	}
}

// 机队页需要登录：未登录跳登录页而不是泄露内容。
func TestFleetRequiresAuth(t *testing.T) {
	s, _, _, _ := newFleetServer(t)
	w := get(t, s, "/admin/nodes", map[string]string{"X-Forwarded-For": "203.0.113.5"})
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/admin/login" {
		t.Errorf("未登录应跳登录页，got %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestFleetListEmpty(t *testing.T) {
	s, _, sess, _ := newFleetServer(t)
	body := authGet(t, s, "/admin/nodes", sess).Body.String()
	if !strings.Contains(body, "还没有纳管任何节点") {
		t.Error("空机队应有引导文案")
	}
}

func TestFleetListShowsNodes(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-hk-1", Host: "203.0.113.7",
		SSHPort: 22, SSHUser: "root", Status: "ready"}
	fs.domains["n1"] = consolestore.NodeDomain{FQDN: "api-01.example.com", RecordState: "insync"}

	body := authGet(t, s, "/admin/nodes", sess).Body.String()
	for _, want := range []string{"node-hk-1", "203.0.113.7", "api-01.example.com", "就绪"} {
		if !strings.Contains(body, want) {
			t.Errorf("列表缺%q", want)
		}
	}
}

// 新增节点：slug校验与ccwadmin/渲染器共用同一条规则，超上限当场拒绝。
func TestNodeCreateValidatesSlugs(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	fs.zones = append(fs.zones, dns.Zone{ID: "z1", Domain: "example.com", SubdomainPrefix: "api"})

	form := url.Values{
		"name": {"n1"}, "host": {"203.0.113.7"}, "ssh_user": {"root"},
		"password": {"pw"}, "zone_id": {"z1"}, "csrf_token": {csrf.Value},
		"slugs": {"a,b,c,d"}, // 4个＝超上限
	}
	w := postForm(t, s, "/admin/nodes/new", form, []*http.Cookie{sess, csrf}, "203.0.113.5")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超上限应400，got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "7.6") {
		t.Errorf("应说明上限来源: %s", w.Body.String())
	}
	if len(fs.created) != 0 {
		t.Error("校验失败时不得创建节点")
	}

	// 非法slug同样拒绝
	form.Set("slugs", "BAD_SLUG")
	if w := postForm(t, s, "/admin/nodes/new", form, []*http.Cookie{sess, csrf}, "203.0.113.5"); w.Code != http.StatusBadRequest {
		t.Errorf("非法slug应400，got %d", w.Code)
	}
}

func TestNodeCreateRequiresCSRF(t *testing.T) {
	s, _, sess, _ := newFleetServer(t)
	form := url.Values{"name": {"n1"}, "host": {"1.2.3.4"}, "ssh_user": {"root"}, "password": {"pw"}}
	w := postForm(t, s, "/admin/nodes/new", form, []*http.Cookie{sess}, "203.0.113.5")
	if w.Code != http.StatusForbidden {
		t.Errorf("缺CSRF应403，got %d", w.Code)
	}
}

// 节点详情：DNS未生效时展示待添加的记录（manual模式的关键提示）。
func TestNodeDetailShowsPendingDNS(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1", Host: "203.0.113.7", Status: "provisioning"}
	fs.domains["n1"] = consolestore.NodeDomain{
		FQDN: "api-01.example.com", TargetIP: "203.0.113.7", RecordState: "pending"}

	body := authGet(t, s, "/admin/nodes/n1", sess).Body.String()
	if !strings.Contains(body, "等待 DNS 记录生效") {
		t.Error("应提示等待DNS")
	}
	for _, want := range []string{"api-01.example.com", "203.0.113.7"} {
		if !strings.Contains(body, want) {
			t.Errorf("应展示待添加的记录内容%q", want)
		}
	}
}

// 运行详情：不能用别的节点ID拼URL看到不属于它的运行。
func TestRunDetailChecksNodeOwnership(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	fs.nodes["n2"] = consolestore.Node{ID: "n2", Name: "node-2"}
	fs.runs["r1"] = consolestore.RunSummary{ID: "r1", NodeID: "n1", Kind: "bootstrap", Status: "running"}

	if w := authGet(t, s, "/admin/nodes/n1/runs/r1", sess); w.Code != 200 {
		t.Errorf("本节点的运行应可见，got %d", w.Code)
	}
	if w := authGet(t, s, "/admin/nodes/n2/runs/r1", sess); w.Code != 404 {
		t.Errorf("跨节点访问运行应404，got %d", w.Code)
	}
	// SSE是长连接：用可取消的ctx，收到历史后即断开（生产里由浏览器关闭）。
	if w := authGetCtx(t, s, "/admin/nodes/n1/runs/r1/stream", sess, 300*time.Millisecond); w.Code != 200 {
		t.Errorf("SSE端点应可用，got %d", w.Code)
	}
	if w := authGetCtx(t, s, "/admin/nodes/n2/runs/r1/stream", sess, 300*time.Millisecond); w.Code != 404 {
		t.Errorf("跨节点SSE应404，got %d", w.Code)
	}
}

// 续跑：没有托管密钥时明确拒绝，而不是拿猜的参数去跑。
func TestResumeRequiresCredential(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1", Status: "degraded"}
	s.Fleet.remember("n1", provision.BootstrapInput{NodeID: "n1", ZoneID: "z1", Slugs: []string{"a1"}})

	w := postForm(t, s, "/admin/nodes/n1/resume", url.Values{"csrf_token": {csrf.Value}},
		[]*http.Cookie{sess, csrf}, "203.0.113.5")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("无托管密钥应拒绝，got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "托管密钥") {
		t.Errorf("应说明原因: %s", w.Body.String())
	}
}

// Console重启后没有部署参数：如实拒绝，不猜。
func TestResumeWithoutRememberedInput(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	fs.creds["n1"] = true

	w := postForm(t, s, "/admin/nodes/n1/resume", url.Values{"csrf_token": {csrf.Value}},
		[]*http.Cookie{sess, csrf}, "203.0.113.5")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("无部署参数应拒绝，got %d", w.Code)
	}
}

// ---- LogHub ----

func TestLogHubBroadcastAndHistory(t *testing.T) {
	h := NewLogHub("")
	h.Append("r1", "第一行")
	history, ch, cancel := h.Subscribe("r1")
	defer cancel()
	if len(history) != 1 || history[0] != "第一行" {
		t.Errorf("新订阅者应收到历史: %v", history)
	}
	h.Append("r1", "第二行")
	select {
	case line := <-ch:
		if line != "第二行" {
			t.Errorf("got %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到广播")
	}
}

// 日志入库前脱敏（第二道防线；sshexec已在源头脱敏）。
func TestLogHubRedacts(t *testing.T) {
	h := NewLogHub("")
	secret := "SuperSecretPassword123"
	h.Append("r1", "POSTGRES_PASSWORD="+secret)
	history, _, cancel := h.Subscribe("r1")
	defer cancel()
	if strings.Contains(strings.Join(history, "\n"), secret) {
		t.Fatal("日志未脱敏——它会写盘并推给浏览器")
	}
}

// 订阅者跟不上时丢行而不是阻塞整条流水线。
func TestLogHubSlowSubscriberDoesNotBlock(t *testing.T) {
	h := NewLogHub("")
	_, _, cancel := h.Subscribe("r1")
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ { // 远超通道容量
			h.Append("r1", "line")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("慢订阅者阻塞了日志写入——流水线会被拖死")
	}
}

func TestLogHubFinish(t *testing.T) {
	h := NewLogHub("")
	_, ch, cancel := h.Subscribe("r1")
	defer cancel()
	h.Finish("r1")
	if !h.IsDone("r1") {
		t.Error("应标记为已结束")
	}
	select {
	case line := <-ch:
		if line != doneMarker {
			t.Errorf("应收到结束标记，got %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到结束标记")
	}
}

func TestLogHubWritesFile(t *testing.T) {
	dir := t.TempDir()
	h := NewLogHub(dir)
	h.Append("r1", "落盘的一行")
	b, err := readFile(dir + "/r1.log")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b, "落盘的一行") {
		t.Errorf("日志未落盘: %s", b)
	}
}

func TestWriteSSEMultiline(t *testing.T) {
	w := httptest.NewRecorder()
	writeSSE(w, "message", "第一行\n第二行")
	got := w.Body.String()
	// 多行必须逐行加data:前缀，否则浏览器只收到第一行
	if strings.Count(got, "data: ") != 2 {
		t.Errorf("多行应逐行加前缀: %q", got)
	}
	w2 := httptest.NewRecorder()
	writeSSE(w2, "done", "")
	if !strings.Contains(w2.Body.String(), "event: done") {
		t.Errorf("命名事件应带event行: %q", w2.Body.String())
	}
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
