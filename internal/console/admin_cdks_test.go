package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccw/internal/consolestore"
	"ccw/internal/provision"
)

// CDK明文的交付路径只有一条：内存中转 → 页面渲染一次 → 立即清除。
// 这条测试守住"一次"——第二次打开同一页面绝不能再看到明文。
func TestCDKPlaintextShownExactlyOnce(t *testing.T) {
	v := &cdkVault{}
	v.put("run-1", provision.IssuedCDK{Slug: "project-a", PublicID: "abc", CDK: "ccw_abc.SECRET"})

	first := v.take("run-1")
	if len(first) != 1 || first[0].CDK != "ccw_abc.SECRET" {
		t.Fatalf("第一次应取到明文: %+v", first)
	}
	if second := v.take("run-1"); len(second) != 0 {
		t.Errorf("取走即清——第二次必须为空，got %+v", second)
	}
}

// 过期清扫：没人来取的明文不该长期驻留在进程内存里。
func TestCDKVaultExpires(t *testing.T) {
	v := &cdkVault{items: map[string]vaultEntry{
		"old": {cdks: []provision.IssuedCDK{{CDK: "ccw_x.SECRET"}}, at: time.Now().Add(-vaultTTL - time.Minute)},
	}}
	if got := v.take("old"); len(got) != 0 {
		t.Errorf("超过TTL的明文应已被清掉，got %+v", got)
	}
}

// 运行详情页渲染一次明文后，再打开就没有了——**页面上也不能有残留**。
func TestRunPageRevealsCDKOnce(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	fs.runs["r1"] = consolestore.RunSummary{ID: "r1", NodeID: "n1", Status: "succeeded"}
	s.Fleet.StashCDK("r1", "project-a", "abc123", "ccw_abc123.SECRETVALUE")

	body := authGet(t, s, "/admin/nodes/n1/runs/r1", sess).Body.String()
	if !strings.Contains(body, "ccw_abc123.SECRETVALUE") {
		t.Fatal("首次打开运行详情页应显示本次签发的CDK明文")
	}
	again := authGet(t, s, "/admin/nodes/n1/runs/r1", sess).Body.String()
	if strings.Contains(again, "SECRETVALUE") {
		t.Error("刷新后明文必须消失（只显示一次）")
	}
}

// CDK页把镜像里的项目与签发记录渲染出来；**public_id可以出现，明文不存在于库中**。
func TestCDKPageListsProjectsAndIssues(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-hk-1"}
	fs.projects["p1"] = consolestore.NodeProject{
		ID: "p1", NodeID: "n1", Slug: "project-a", RemoteProjectID: "remote-1",
		DiskLimitBytes: 15 << 30, FiveHourLimit: 1000, SevenDayLimit: 5000,
	}
	fs.RecordCDKIssue(context.Background(), "p1", "a1b2c3d4e5f60718", "u1")

	body := authGet(t, s, "/admin/cdks", sess).Body.String()
	for _, want := range []string{"project-a", "node-hk-1", "a1b2c3d4e5f60718", "15"} {
		if !strings.Contains(body, want) {
			t.Errorf("CDK页缺%q", want)
		}
	}
}

// 写操作必须带CSRF——它们会在真实节点上签发/撤销凭据。
func TestCDKActionsRequireCSRF(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	fs.projects["p1"] = consolestore.NodeProject{ID: "p1", NodeID: "n1", Slug: "project-a"}

	for _, path := range []string{"/admin/cdks/issue", "/admin/cdks/rotate", "/admin/cdks/sync"} {
		form := "project_id=p1"
		req, _ := http.NewRequest("POST", path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", "203.0.113.5")
		req.AddCookie(sess)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s 无CSRF应403，got %d", path, w.Code)
		}
	}
}

// 审计页能读到写进去的记录，并支持按结果过滤。
func TestAuditPageListsAndFilters(t *testing.T) {
	s, _, sess, _ := newFleetServer(t)
	as := s.Auth.Store.(*fakeAdminStore)
	as.audits = append(as.audits,
		consolestore.AuditEntry{Actor: "u1", Action: "node.bootstrap", Target: "node-1",
			Result: "ok", ClientIP: "203.0.113.5"},
		consolestore.AuditEntry{Action: "admin.login", Target: "ghost",
			Result: "denied", ClientIP: "198.51.100.9"},
	)

	body := authGet(t, s, "/admin/audit", sess).Body.String()
	if !strings.Contains(body, "node.bootstrap") || !strings.Contains(body, "admin.login") {
		t.Error("审计页应列出全部记录")
	}
	// 过滤后按来源IP判断留下了哪几行——动作名在筛选下拉里也会出现，
	// 拿它做断言会永远为真。
	only := authGet(t, s, "/admin/audit?result=denied", sess).Body.String()
	if !strings.Contains(only, "198.51.100.9") {
		t.Error("按result=denied过滤应保留被拒的那条")
	}
	if strings.Contains(only, "203.0.113.5") {
		t.Error("按result=denied过滤不应保留成功的那条")
	}
}

// 域名页把「待添加的A记录」直接给出来——那是唯一会拦住部署的一步。
func TestDomainsPageShowsPendingRecord(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-hk-1"}
	fs.domains["n1"] = consolestore.NodeDomain{
		FQDN: "api-01.example.com", Seq: 1, TargetIP: "203.0.113.7", RecordState: "pending",
	}
	body := authGet(t, s, "/admin/domains", sess).Body.String()
	for _, want := range []string{"api-01.example.com", "203.0.113.7", "等待添加记录"} {
		if !strings.Contains(body, want) {
			t.Errorf("域名页缺%q", want)
		}
	}
}
