package console

import (
	"strings"
	"testing"
	"time"

	"ccw/internal/provision"
)

// 用**带档位的真实数据**整页渲染一遍。
//
// 2026-08-02 真机：`.TierEffective` 字段漏加进 provision.NodeProjectUsage，
// 而模板引用不存在的字段**只在运行时报错**——构建通过、既有测试全绿，
// 因为没有一条测试用 Tier 非空的行渲染过页面（模板里那段在 {{if .Tier}} 之内）。
//
// 后果比"少显示一块"重得多：Go 模板出错时，**已经写出的部分照样送给浏览器**，
// 后面直接截断。于是 project-a 的限额显示完就没了——下拉框不见、project-b 不见，
// 看起来像"项目丢了"和"功能坏了"，实际是渲染断在那一行。
//
// 这条测试就是那时缺的：整页渲染 + 断言关键片段都在。
func TestRenderUsageWithRows(t *testing.T) {
	s, _, _, _ := newFleetServer(t)
	tiers := []provision.QuotaTier{{Name: "2x", ShareBP: 1000}, {Name: "7x", ShareBP: 3300}}
	mk := func(slug, tier string, used5, used7, lim5, lim7 int64) usageRow {
		u := provision.NodeProjectUsage{
			Slug: slug, Tier: tier, TierEffective: tier != "",
			FiveHour:    provision.UsageTotals{Weighted: used5},
			SevenDay:    provision.UsageTotals{Weighted: used7},
			FiveHourLim: lim5, SevenDayLim: lim7,
			PoolFiveHour: 1_817_541, PoolSevenDay: 8_541_919,
			LastEventAt: func() *time.Time { n := time.Now(); return &n }(),
		}
		r := makeUsageRow(u, "Node-NY-02", "n1", time.Now())
		r.TierOpts = tierOptionsFor(u, tiers)
		return r
	}
	rows := []usageRow{
		mk("project-a", "2x", 0, 3_053_579, 181_754, 854_191),
		mk("project-b", "7x", 38_418, 1_644_469, 599_788, 2_818_833),
	}
	var sb strings.Builder
	err := s.tmpl["admin_usage.html"].ExecuteTemplate(&sb, "layout", map[string]any{
		"Node": struct{ ID, Name string }{"n1", "Node-NY-02"},
		"Rows": rows, "Tiers": tiers, "NodeID": "n1", "CSRF": "x",
		"Cap5": int64(1_817_541), "Cap7": int64(8_541_919), "Calibrated": true,
		"Nav": "nodes", "Username": "a", "Initial": "a", "NodeCount": 1,
	})
	out := sb.String()
	if err != nil {
		t.Fatalf("模板执行出错（浏览器会收到截断的半页）：%v", err)
	}
	for _, want := range []string{"project-a", "project-b", `name="tier"`, "指派"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里缺 %q —— 说明渲染在这之前断了", want)
		}
	}
}
