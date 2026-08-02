package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ccw/internal/consolestore"
	"ccw/internal/provision"
)

func ts(t time.Time) *time.Time { return &t }

// 「最近无采集」是判断采集链路死没死的唯一线索——采集断掉的表现不是报错，
// 而是"一切正常、表永远是空的"。
func TestUsageRowFlagsStaleCollection(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	// 从没采集到 → 一定是 stale
	r := makeUsageRow(provision.NodeProjectUsage{Slug: "a"}, "n1", "nid", now)
	if !r.Stale || r.LastSeen != "从未采集到" {
		t.Errorf("无数据应标为 stale，got %+v", r)
	}

	// 刚采到 → 正常
	r = makeUsageRow(provision.NodeProjectUsage{Slug: "a", LastEventAt: ts(now.Add(-time.Hour))}, "n1", "nid", now)
	if r.Stale {
		t.Error("1 小时前有数据不该算 stale")
	}

	// **周末没人用不该报警**：阈值取 24 小时，正常空闲不会误报
	r = makeUsageRow(provision.NodeProjectUsage{Slug: "a", LastEventAt: ts(now.Add(-20 * time.Hour))}, "n1", "nid", now)
	if r.Stale {
		t.Error("20 小时前有数据仍不该算 stale（正常空闲）")
	}
	r = makeUsageRow(provision.NodeProjectUsage{Slug: "a", LastEventAt: ts(now.Add(-30 * time.Hour))}, "n1", "nid", now)
	if !r.Stale {
		t.Error("超过 24 小时应标为 stale")
	}
}

// 占比不能除零，也不能算出超过 100% 或负数。
func TestPctOf(t *testing.T) {
	cases := []struct {
		used, limit int64
		want        int
	}{
		{0, 1000, 0}, {250, 1000, 25}, {1000, 1000, 100},
		{5000, 1000, 100}, // 超了钳到 100
		{100, 0, 0},       // 限额未配置：不能除零，也不该编一个百分比
		{-5, 1000, 0},
	}
	for _, c := range cases {
		if got := pctOf(c.used, c.limit); got != c.want {
			t.Errorf("pctOf(%d,%d) = %d, want %d", c.used, c.limit, got, c.want)
		}
	}
}

// 页面必须把"真实 token"与"内部额度单位"分开讲。
// 混在一起最容易让人把估算值当成账号的实际消耗，而 spec §10 明令不得
// 把内部计量标成官方订阅百分比。
func TestUsagePageSeparatesRealFromEstimated(t *testing.T) {
	s, _, sess, csrf := newFleetServer(t)
	req := httptest.NewRequest("GET", "/admin/usage", nil)
	req.AddCookie(sess)
	req.AddCookie(csrf)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{"真实", "内部额度单位", "CCW_USAGE_WEIGHTS"} {
		if !strings.Contains(body, want) {
			t.Errorf("用量页应说明 %q", want)
		}
	}
	// **这里不做措辞黑名单。**页面上正确的写法本来就要提到"不是官方订阅额度"，
	// 黑名单会把否定句一起判成违规——写这条测试时就先误报了一次。
	// 越界措辞的风险在对外的落地页，那边有 TestHomePage 的黑名单守着；
	// 这一页是给管理员看的，真正要保证的是"两套数分得清"，即上面那几条正向断言。
	if !strings.Contains(body, "不是") {
		t.Error("必须明确否定'内部单位＝官方额度'这个误解，而不是只摆数字")
	}
}

// 档位百分比越界要拒绝：0 会让该档位的项目立刻全员受限，>100 是超卖账号池。
func TestSetTierRejectsOutOfRangePercent(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	for _, p := range []string{"0", "-1", "101", "abc", ""} {
		form := url.Values{"csrf_token": {csrf.Value}, "node": {"n1"}, "name": {"7x"}, "percent": {p}}
		w := postForm(t, s, "/admin/usage/tier", form, []*http.Cookie{sess, csrf}, "203.0.113.5")
		dec, _ := url.QueryUnescape(w.Header().Get("Location"))
		if !strings.Contains(dec, "百分比") && !strings.Contains(dec, "编排器") {
			t.Errorf("percent=%q 应被拒绝，got %s", p, dec)
		}
	}
}

func TestTierFormsRequireCSRF(t *testing.T) {
	s, fs, sess, _ := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	for _, path := range []string{"/admin/usage/tier", "/admin/usage/assign"} {
		form := url.Values{"node": {"n1"}, "name": {"7x"}, "percent": {"33"}}
		w := postForm(t, s, path, form, []*http.Cookie{sess}, "203.0.113.5")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s 无CSRF应403，got %d", path, w.Code)
		}
	}
}

// 每行的档位指派必须发到**这一行所属的节点**。
//
// quota_tiers 与 projects 都在节点本地的库里；用全页共用的一个 node 去指派，
// 多节点时会把项目挂到另一台机器上——那台要么没这个 slug（报错），
// 要么恰好有同名项目（**改错了人，还不报错**）。
func TestUsageRowCarriesItsOwnNodeID(t *testing.T) {
	a := makeUsageRow(provision.NodeProjectUsage{Slug: "alice"}, "node-1", "id-1", time.Now())
	b := makeUsageRow(provision.NodeProjectUsage{Slug: "bob"}, "node-2", "id-2", time.Now())
	if a.NodeID != "id-1" || b.NodeID != "id-2" {
		t.Errorf("每行应带自己的节点 ID，got %q %q", a.NodeID, b.NodeID)
	}
	if a.NodeID == b.NodeID {
		t.Error("不同节点的行不该共用一个 node ID")
	}
}
