package console

import (
	"strings"
	"testing"

	"ccw/internal/consolestore"
)

// 管理后台的外壳是**契约不是装饰**：每个管理页都必须落在同一个左侧栏骨架里，
// 且当前位置要在导航上标出来。新增页面时忘了走renderAdmin，
// 表现是「页面能打开但整个后台的导航突然消失了」——这里守住它。
func TestAdminPagesShareShell(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-hk-1", Host: "203.0.113.7",
		SSHPort: 22, SSHUser: "root", Status: "ready"}
	fs.runs["r1"] = consolestore.RunSummary{ID: "r1", NodeID: "n1", Kind: "bootstrap", Status: "succeeded"}

	// 导航只有两个可点的项，每页恰好高亮其中一个。
	const (
		home  = `href="/admin" `
		fleet = `href="/admin/nodes" `
	)
	cases := []struct{ path, current, other string }{
		{"/admin", home, fleet},
		{"/admin/nodes", fleet, home},
		{"/admin/nodes/new", fleet, home},
		{"/admin/nodes/n1", fleet, home},
		{"/admin/nodes/n1/runs/r1", fleet, home},
	}

	for _, c := range cases {
		w := authGet(t, s, c.path, sess)
		if w.Code != 200 {
			t.Errorf("%s: got %d", c.path, w.Code)
			continue
		}
		body := w.Body.String()
		// 骨架件：左轨、顶栏、当前用户、退出。
		// 用户名连着标签一起断言——光找"admin"是白断言，
		// 每页都有href="/admin"，用户名丢了也照样通过。
		for _, want := range []string{
			`class="rail"`, `class="topbar"`, `class="nm">admin<`, "退出登录",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: 外壳缺%q", c.path, want)
			}
		}
		if !strings.Contains(body, c.current+`aria-current="page"`) {
			t.Errorf("%s: 应高亮 %s", c.path, c.current)
		}
		if strings.Contains(body, c.other+`aria-current="page"`) {
			t.Errorf("%s: 不该同时高亮 %s", c.path, c.other)
		}
	}
}

// 主题三态靠外壳里的那段脚本驱动。它是纯前端行为、没有服务端状态可断言，
// 但**三个档位必须都在页面上**——少了auto，选过浅/深的人就再也回不到
// 跟随系统。
func TestAdminShellHasThemeControl(t *testing.T) {
	s, _, sess, _ := newFleetServer(t)
	body := authGet(t, s, "/admin", sess).Body.String()

	for _, want := range []string{
		`data-theme-set="auto"`, `data-theme-set="light"`, `data-theme-set="dark"`, "ccw-theme",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("外壳缺%q", want)
		}
	}
}

// 登录页没有侧边栏（此时还没登录），但必须沿用同一份主题偏好——
// 否则选了深色的人每次登录都被闪一次白底。
func TestLoginPageHonorsThemePreference(t *testing.T) {
	s, _ := newAuthServer(t)
	body := get(t, s, "/admin/login", map[string]string{"X-Forwarded-For": "203.0.113.5"}).Body.String()

	if !strings.Contains(body, "ccw-theme") {
		t.Error("登录页应读取与后台同一份主题偏好")
	}
	if strings.Contains(body, `class="rail"`) {
		t.Error("登录页不应有侧边栏")
	}
}
