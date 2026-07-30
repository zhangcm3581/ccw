package console

import (
	"context"
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

	for _, path := range []string{"start", "refresh", "key", "code", "cancel"} {
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

// 重置节点同样要求打对节点名——它会不可撤销地擦掉远端的卷（含 Claude 凭据）。
func TestNodeResetRequiresTypedName(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-hk-1"}

	// 名字不对：连编排器都不该碰
	form := url.Values{"csrf_token": {csrf.Value}, "confirm": {"node-hk"}}
	w := postForm(t, s, "/admin/nodes/n1/reset", form, []*http.Cookie{sess, csrf}, "203.0.113.5")
	loc := w.Header().Get("Location")
	dec, _ := url.QueryUnescape(loc)
	if !strings.Contains(dec, "确认文本") {
		t.Errorf("名字不符应拦下并说明原因，got %s", loc)
	}

	// 名字对了，但本测试里没有编排器——应报"编排器未启用"而不是静默成功
	form.Set("confirm", "node-hk-1")
	w = postForm(t, s, "/admin/nodes/n1/reset", form, []*http.Cookie{sess, csrf}, "203.0.113.5")
	dec, _ = url.QueryUnescape(w.Header().Get("Location"))
	if !strings.Contains(dec, "编排器") {
		t.Errorf("名字对了应进入执行路径，got %s", dec)
	}
}

func TestNodeResetRequiresCSRF(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	form := url.Values{"confirm": {"node-1"}}
	w := postForm(t, s, "/admin/nodes/n1/reset", form, []*http.Cookie{sess}, "203.0.113.5")
	if w.Code != http.StatusForbidden {
		t.Errorf("无CSRF应403，got %d", w.Code)
	}
}

// Console 重启后内存里的部署参数就没了——而「更新 Console」就是重建镜像＋重启，
// 所以这是常态。续跑必须能从库里把参数重建出来，否则「重置 → 重新部署」
// 这个测试循环一过重启就断。
func TestResumeRebuildsInputAfterRestart(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1", Host: "203.0.113.9"}
	// 模拟重启：lastInput 是空的，但库里有项目镜像
	s.Fleet.lastInput = nil
	pid, err := fs.UpsertNodeProject(context.Background(), "n1", "alpha", "r-1", 15<<30, 100, 500)
	if err != nil {
		t.Fatal(err)
	}
	_ = pid

	in, rerr := s.reconstructBootstrap(context.Background(), "n1")
	if rerr != nil {
		t.Fatalf("有项目镜像却重建失败: %v", rerr)
	}
	if len(in.Slugs) != 1 || in.Slugs[0] != "alpha" {
		t.Errorf("slug 应从 node_projects 重建，got %v", in.Slugs)
	}
	if in.NodeID != "n1" {
		t.Errorf("NodeID = %q", in.NodeID)
	}

	// 没有任何项目记录时**不猜**：空 slug 列表会渲染出一个没有项目的 compose。
	fs.nodes["n2"] = consolestore.Node{ID: "n2", Name: "node-2"}
	if _, err := s.reconstructBootstrap(context.Background(), "n2"); err == nil {
		t.Error("没有项目镜像应报错而不是用空列表跑")
	}

	// 端到端：POST resume 不该再回「没有部署参数」
	form := url.Values{"csrf_token": {csrf.Value}}
	w := postForm(t, s, "/admin/nodes/n1/resume", form, []*http.Cookie{sess, csrf}, "203.0.113.5")
	if strings.Contains(w.Body.String(), "本次Console启动后没有该节点的部署参数") {
		t.Errorf("重启后仍报缺参数：%s", w.Body.String())
	}
}
