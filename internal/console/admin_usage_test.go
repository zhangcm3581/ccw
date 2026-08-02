package console

import (
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
	s, fs, sess, csrf := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}
	req := httptest.NewRequest("GET", "/admin/nodes/n1/usage", nil)
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

// 用量挂在节点下面：项目、档位表、Claude 账号全是节点本地的东西。
// 平铺成一页会让"这个设置对哪台生效"变得含糊——之前那两个 bug
// （档位指派发到错节点、档位表看着像全机队设置）正是从平铺结构来的。
func TestUsageIsScopedToNode(t *testing.T) {
	s, fs, sess, csrf := newFleetServer(t)
	fs.nodes["n1"] = consolestore.Node{ID: "n1", Name: "node-1"}

	// 不存在的节点应 404，而不是渲染一个空页面
	req := httptest.NewRequest("GET", "/admin/nodes/nope/usage", nil)
	req.AddCookie(sess)
	req.AddCookie(csrf)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在的节点应 404，got %d", w.Code)
	}

	// 旧的平铺地址重定向到机队列表，不留死链
	w = get(t, s, "/admin/usage", nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/admin/nodes" {
		t.Errorf("/admin/usage 应重定向到 /admin/nodes，got %d %s", w.Code, w.Header().Get("Location"))
	}
}

// **指派档位会立刻生效并杀掉活跃会话**，所以要让人在按下去之前就看见
// "这一档等于多少单位、会不会当场受限"，而不是指派完看结果。
// 2026-08-02 真机上就是这样：给 project-b 指派 2x，终端当场被关。
func TestTierOptionsWarnBeforeLockout(t *testing.T) {
	u := provision.NodeProjectUsage{
		Slug:         "project-b",
		PoolFiveHour: 9_000_000, PoolSevenDay: 9_000_000,
		SevenDay: provision.UsageTotals{Weighted: 1_606_051}, // 已用 1.6M
	}
	tiers := []provision.QuotaTier{{Name: "2x", ShareBP: 1000}, {Name: "7x", ShareBP: 3300}}
	got := tierOptionsFor(u, tiers)
	if len(got) != 2 {
		t.Fatalf("应给出两档，got %+v", got)
	}
	// 2x = 10% × 9M = 900K，而已用 1.6M → 会当场受限
	if got[0].SevenLim != 900_000 {
		t.Errorf("2x 的 7d 限额应为 900000，got %d", got[0].SevenLim)
	}
	if !got[0].WouldLimit {
		t.Error("已用 1.6M 超过 2x 的 900K，必须提前标出会受限")
	}
	// 7x = 33% × 9M = 2.97M > 1.6M → 不受限
	if got[1].WouldLimit {
		t.Errorf("7x 的限额 %d 高于已用量，不该标记受限", got[1].SevenLim)
	}
}

// 容量未校准时不摆数字——那时档位本来就不生效，
// 摆一堆算不出来的值只会误导。
func TestTierOptionsEmptyWhenUncalibrated(t *testing.T) {
	tiers := []provision.QuotaTier{{Name: "2x", ShareBP: 1000}}
	for _, pool := range []int64{0, -1, math.MaxInt64} {
		u := provision.NodeProjectUsage{PoolFiveHour: pool, PoolSevenDay: pool}
		if got := tierOptionsFor(u, tiers); got != nil {
			t.Errorf("容量=%d 时不该给出档位数字，got %+v", pool, got)
		}
	}
}

// 指派了档位就必须显示出来——哪怕容量还没校准、档位暂时不生效。
//
// 页面上写着"不用档位"而实际挂着，是在说假话；管理员会以为没指派成功，
// 于是重复指派或去别处找原因。
func TestAssignedTierIsShownEvenWhenNotYetEffective(t *testing.T) {
	tpl := readUsageTemplate(t)
	// 未校准的降级分支里也必须有 selected 判定
	if !strings.Contains(tpl, `{{if eq $row.Tier .Name}} selected{{end}}`) {
		t.Error("容量未校准时也要标出当前挂的档位，否则页面会显示成「不用档位」")
	}
	// 「不用档位」只在真的没挂时才是选中态
	if !strings.Contains(tpl, `{{if not $row.Tier}} selected{{end}}`) {
		t.Error("「不用档位」应仅在未指派时选中")
	}
	// 挂了档位但算不出限额时，要明说"暂未生效"
	if !strings.Contains(tpl, "账号窗口容量还没校准") {
		t.Error("挂了档位却不生效时，必须说明原因")
	}
}

func readUsageTemplate(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("templates/admin_usage.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 校准状态必须在页面上看得到。
//
// 它是档位生不生效的前提，而原本只能去节点上 grep worker-agent 的日志——
// 用户已经两次跑到 Console 主机上找 ccw-worker-agent 容器了（它在节点上）。
// 需要登机才能回答的问题，就是后台缺的功能。
func TestUsagePageShowsCalibrationState(t *testing.T) {
	tpl := readUsageTemplate(t)
	for _, want := range []string{"账号 5 小时窗口容量", "还没校准", "活跃会话"} {
		if !strings.Contains(tpl, want) {
			t.Errorf("用量页应显示校准状态，缺 %q", want)
		}
	}
}
