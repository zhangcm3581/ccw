package console

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ccw/internal/consolestore"
)

// 解除纳管必须打对节点名才执行。
//
// 删除不可撤销（项目、CDK签发记录、运行历史都随外键一起没），
// 而红按钮太容易误点——所以要求把名字原样打一遍。
func TestNodeRetireRequiresTypedName(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-hk-1"}

	// 确认文本不对：不执行
	form := url.Values{"csrf_token": {csrf.Value}, "confirm": {"node-hk"}}
	w := postForm(t, s, "/admin/nodes/n1/delete", form, []*http.Cookie{sess, csrf}, "203.0.113.5")
	if w.Code != http.StatusFound {
		t.Fatalf("应重定向回节点页，got %d", w.Code)
	}
	if len(fs.retired) != 0 {
		t.Fatal("确认文本不符时不得删除")
	}
	if _, ok := fs.nodes["n1"]; !ok {
		t.Fatal("节点不该消失")
	}

	// 打对了才执行
	form.Set("confirm", "node-hk-1")
	w = postForm(t, s, "/admin/nodes/n1/delete", form, []*http.Cookie{sess, csrf}, "203.0.113.5")
	if w.Code != http.StatusFound {
		t.Fatalf("应重定向到机队页，got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/admin/nodes?") {
		t.Errorf("删除后应回机队页，got %s", loc)
	}
	if len(fs.retired) != 1 || fs.retired[0] != "n1" {
		t.Errorf("应调用RetireNode: %v", fs.retired)
	}
}

// 解除纳管必须带CSRF——它会不可撤销地删掉一台机器的全部记录。
func TestNodeRetireRequiresCSRF(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}

	form := url.Values{"confirm": {"node-1"}}
	w := postForm(t, s, "/admin/nodes/n1/delete", form, []*http.Cookie{sess}, "203.0.113.5")
	if w.Code != http.StatusForbidden {
		t.Errorf("无CSRF应403，got %d", w.Code)
	}
	if len(fs.retired) != 0 {
		t.Error("CSRF失败时不得删除")
	}
}

// 审计必须**先于**删除写入：删完节点行就没了，事后再记会记不上是谁删的哪台。
func TestNodeRetireAuditsBeforeDeleting(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	as := s.Auth.Store.(*fakeAdminStore)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1", Host: "203.0.113.7"}
	as.auditErr = errors.New("db down") // 审计写不进去

	form := url.Values{"csrf_token": {csrf.Value}, "confirm": {"node-1"}}
	postForm(t, s, "/admin/nodes/n1/delete", form, []*http.Cookie{sess, csrf}, "203.0.113.5")

	if len(fs.retired) != 0 {
		t.Error("审计写入失败时必须中止删除（§8.5：不允许无审计的特权操作）")
	}
	if _, ok := fs.nodes["n1"]; !ok {
		t.Error("节点应还在")
	}
}

// 禁用CDK需要CSRF与public_id，并且会把镜像标成已撤销。
func TestCDKDisableRequiresCSRF(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	fs.projects["p1"] = consolestore.NodeProject{ID: "p1", NodeID: "n1", Slug: "project-a"}

	form := url.Values{"project_id": {"p1"}, "public_id": {"abc123"}}
	w := postForm(t, s, "/admin/cdks/disable", form, []*http.Cookie{sess}, "203.0.113.5")
	if w.Code != http.StatusForbidden {
		t.Errorf("无CSRF应403，got %d", w.Code)
	}
}

// 授权相关的四个入口都必须带CSRF——它们会在真实机器上起进程、送凭据。
func TestClaudeAuthRequiresCSRF(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	fs.projects["p1"] = consolestore.NodeProject{ID: "p1", NodeID: "n1", Slug: "project-a"}

	for _, path := range []string{"start", "refresh", "code", "cancel"} {
		form := url.Values{"code": {"abc"}}
		w := postForm(t, s, "/admin/nodes/n1/claude/"+path, form, []*http.Cookie{sess}, "203.0.113.5")
		if w.Code != http.StatusForbidden {
			t.Errorf("claude/%s 无CSRF应403，got %d", path, w.Code)
		}
	}
}

// 节点上还没有项目容器时不能装作能授权：给出可操作的提示而不是一个会失败的按钮。
func TestClaudeAuthWithoutProjectsSaysSo(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	// 故意不建 projects

	form := url.Values{"csrf_token": {csrf.Value}}
	w := postForm(t, s, "/admin/nodes/n1/claude/start", form, []*http.Cookie{sess, csrf}, "203.0.113.5")
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("应带错误提示回节点页，got %s", loc)
	}
	if dec, _ := url.QueryUnescape(loc); !strings.Contains(dec, "还没有项目容器") {
		t.Errorf("提示应说明原因: %s", loc)
	}
}
