package dns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 假SeqAllocator：单调递增，模拟数据库的next_seq。
type fakeAlloc struct {
	next int
	err  error
}

func (f *fakeAlloc) NextSeq(context.Context, string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.next++
	return f.next, nil
}

func testZone() Zone {
	return Zone{ID: "z1", Domain: "example.com", Provider: "manual", SubdomainPrefix: "api"}
}

func TestFQDN(t *testing.T) {
	if got := FQDN("api", 3, "example.com"); got != "api-03.example.com" {
		t.Errorf("got %s", got)
	}
	// 两位数补零，但不截断更大的序号
	if got := FQDN("api", 137, "example.com"); got != "api-137.example.com" {
		t.Errorf("got %s", got)
	}
}

// §6.2：从第一台就带序号，不设特例（api-01而不是api）。
func TestAllocateStartsAtOne(t *testing.T) {
	a := &fakeAlloc{}
	seq, fqdn, err := Allocate(context.Background(), a, testZone(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 || fqdn != "api-01.example.com" {
		t.Errorf("首台应为api-01（不设特例），got seq=%d fqdn=%s", seq, fqdn)
	}
}

// 序号单调递增且**永不回收**：退役节点的号不再分配（A12）。
func TestAllocateMonotonicNeverReuses(t *testing.T) {
	a := &fakeAlloc{}
	var got []string
	for i := 0; i < 3; i++ {
		_, fqdn, err := Allocate(context.Background(), a, testZone(), nil)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fqdn)
	}
	want := "api-01.example.com,api-02.example.com,api-03.example.com"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v", got)
	}

	// api-02退役（taken返回false表示不再占用），下一个仍是04而不是回收02
	_, fqdn, err := Allocate(context.Background(), a, testZone(), func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if fqdn != "api-04.example.com" {
		t.Errorf("退役后的序号不得复用（旧客户端可能仍持有该域名），got %s", fqdn)
	}
}

// 已占用的名字跳过（并发分配或历史遗留）。
func TestAllocateSkipsTaken(t *testing.T) {
	a := &fakeAlloc{}
	taken := map[string]bool{"api-01.example.com": true, "api-02.example.com": true}
	seq, fqdn, err := Allocate(context.Background(), a, testZone(), func(f string) bool { return taken[f] })
	if err != nil {
		t.Fatal(err)
	}
	if seq != 3 || fqdn != "api-03.example.com" {
		t.Errorf("应跳到api-03，got seq=%d %s", seq, fqdn)
	}
}

func TestAllocatePropagatesError(t *testing.T) {
	a := &fakeAlloc{err: errors.New("db down")}
	if _, _, err := Allocate(context.Background(), a, testZone(), nil); err == nil {
		t.Fatal("分配器错误应上抛")
	}
}

// §6.2保留名单：这些不可分配给节点。
func TestIsReserved(t *testing.T) {
	for _, s := range []string{"www", "admin", "api", "app", "docs", "status", "mail",
		"ns", "ns1", "ns02", "_acme-challenge", "_dmarc", "WWW", "Admin"} {
		if !IsReserved(s) {
			t.Errorf("%q 应为保留名", s)
		}
	}
	for _, s := range []string{"api-01", "node-7", "worker"} {
		if IsReserved(s) {
			t.Errorf("%q 不应为保留名", s)
		}
	}
	// 站点/后台域名已占用的标签也要排除
	if !IsReserved("console", "console") {
		t.Error("额外保留名应生效")
	}
}

// 若前缀恰好撞上保留名，分配器跳过并继续消耗序号（不回收）。
func TestAllocateSkipsReservedLabel(t *testing.T) {
	a := &fakeAlloc{}
	z := testZone()
	z.SubdomainPrefix = "ns" // ns-01不是保留名，但用它验证跳过逻辑不误伤
	_, fqdn, err := Allocate(context.Background(), a, z, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fqdn != "ns-01.example.com" {
		t.Errorf("ns-01不是保留名（保留的是ns/ns1这类纯数字后缀），got %s", fqdn)
	}
}

// ---- Manual provider ----

func TestManualUpsertAndVerify(t *testing.T) {
	m := &Manual{Resolvers: []string{"r1:53", "r2:53"}}
	ctx := context.Background()
	change, err := m.UpsertA(ctx, testZone(), "api-01.example.com", "203.0.113.7", 60)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(change, "api-01.example.com") || !strings.Contains(change, "203.0.113.7") {
		t.Errorf("changeID应含记录信息供展示: %s", change)
	}

	// 两个解析器都正确 → 通过
	m.lookupFn = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"203.0.113.7"}, nil
	}
	if err := m.WaitPropagated(ctx, testZone(), change); err != nil {
		t.Fatalf("都指向正确IP应通过: %v", err)
	}
}

// 交叉验证：只要有一个解析器不一致就判为未生效——
// 单点缓存不一致时过早放行，会让Caddy在DNS没生效时就去要证书，
// 连续失败会撞上「每标识符5次/小时」的授权失败限额（§6.5）。
func TestManualVerifyRequiresAllResolvers(t *testing.T) {
	m := &Manual{Resolvers: []string{"r1:53", "r2:53"}}
	ctx := context.Background()
	m.lookupFn = func(_ context.Context, resolver, _ string) ([]string, error) {
		if resolver == "r1:53" {
			return []string{"203.0.113.7"}, nil
		}
		return []string{"198.51.100.1"}, nil // 旧记录/缓存
	}
	err := m.Verify(ctx, "api-01.example.com", "203.0.113.7")
	if !errors.Is(err, ErrNotPropagated) {
		t.Fatalf("应返回ErrNotPropagated，got %v", err)
	}
	// 错误信息要说清是哪个解析器、看到了什么——管理员据此排查
	if !strings.Contains(err.Error(), "r2:53") || !strings.Contains(err.Error(), "198.51.100.1") {
		t.Errorf("错误应指明分歧: %v", err)
	}
}

func TestManualVerifyNoRecord(t *testing.T) {
	m := &Manual{Resolvers: []string{"r1:53"}}
	m.lookupFn = func(context.Context, string, string) ([]string, error) {
		return nil, fmt.Errorf("no such host")
	}
	err := m.Verify(context.Background(), "api-01.example.com", "203.0.113.7")
	if !errors.Is(err, ErrNotPropagated) {
		t.Fatalf("查不到记录应为未生效，got %v", err)
	}
}

func TestParseManualChange(t *testing.T) {
	name, ip, ok := parseManualChange("manual:api-01.example.com A 203.0.113.7 (TTL 60)")
	if !ok || name != "api-01.example.com" || ip != "203.0.113.7" {
		t.Errorf("解析错误: %s %s %v", name, ip, ok)
	}
	if _, _, ok := parseManualChange("route53:C123456"); ok {
		t.Error("非manual的changeID应被拒绝")
	}
}

func TestInstructions(t *testing.T) {
	s := Instructions("api-01.example.com", "203.0.113.7")
	for _, want := range []string{"A", "api-01.example.com", "203.0.113.7", "60"} {
		if !strings.Contains(s, want) {
			t.Errorf("说明应含%q: %s", want, s)
		}
	}
}

// ---- CAA ----

func TestParseCAARdata(t *testing.T) {
	// flags=0, tag="issue"(5), value="letsencrypt.org"
	rdata := append([]byte{0, 5}, []byte("issue")...)
	rdata = append(rdata, []byte("letsencrypt.org")...)
	rec, err := parseCAARdata(rdata)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Tag != "issue" || rec.Value != "letsencrypt.org" || rec.Flags != 0 {
		t.Errorf("解析错误: %+v", rec)
	}

	for _, bad := range [][]byte{{}, {0}, {0, 99, 'x'}} {
		if _, err := parseCAARdata(bad); err == nil {
			t.Errorf("越界数据应报错: %v", bad)
		}
	}
}

func TestCAAAllowsLetsEncrypt(t *testing.T) {
	cases := []struct {
		name string
		recs []CAARecord
		want bool
	}{
		{"无记录＝不限制", nil, true},
		{"只有iodef不构成白名单", []CAARecord{{Tag: "iodef", Value: "mailto:x@example.com"}}, true},
		{"允许LE", []CAARecord{{Tag: "issue", Value: "letsencrypt.org"}}, true},
		{"允许LE带参数", []CAARecord{{Tag: "issue", Value: "letsencrypt.org; accounturi=https://acme-v02.api.letsencrypt.org/acme/acct/1"}}, true},
		{"只允许别家＝阻挡", []CAARecord{{Tag: "issue", Value: "digicert.com"}}, false},
		{"多条含LE", []CAARecord{{Tag: "issue", Value: "digicert.com"}, {Tag: "issue", Value: "letsencrypt.org"}}, true},
		{"issuewild允许LE", []CAARecord{{Tag: "issuewild", Value: "letsencrypt.org"}}, true},
		{"大小写不敏感", []CAARecord{{Tag: "ISSUE", Value: "LetsEncrypt.ORG"}}, true},
		{"禁止一切签发", []CAARecord{{Tag: "issue", Value: ";"}}, false},
	}
	for _, c := range cases {
		if got := CAAAllowsLetsEncrypt(c.recs); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
