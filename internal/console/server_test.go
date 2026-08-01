package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccw/internal/consolestore"
)

type fakeSite struct {
	rel     consolestore.Release
	arts    []consolestore.Artifact
	hasRel  bool
	resolve map[string]string
}

func (f *fakeSite) LatestPublished(context.Context) (consolestore.Release, []consolestore.Artifact, error) {
	if !f.hasRel {
		return consolestore.Release{}, nil, consolestore.ErrNotFound
	}
	return f.rel, f.arts, nil
}
func (f *fakeSite) ArtifactByFilename(_ context.Context, name string) (consolestore.Artifact, error) {
	if !f.hasRel {
		return consolestore.Artifact{}, consolestore.ErrNotFound
	}
	for _, a := range f.arts {
		if a.Filename == name {
			return a, nil
		}
	}
	return consolestore.Artifact{}, consolestore.ErrNotFound
}
func (f *fakeSite) ResolveAPIDomain(_ context.Context, pid string) (string, error) {
	if d, ok := f.resolve[pid]; ok {
		return d, nil
	}
	return "", consolestore.ErrNotFound
}

func newTestServer(t *testing.T) (*Server, *fakeSite, *bytes.Buffer, string) {
	t.Helper()
	dist := t.TempDir()
	now := time.Now()
	f := &fakeSite{
		hasRel: true,
		rel:    consolestore.Release{Version: "v0.1.0", PublishedAt: &now},
		arts: []consolestore.Artifact{
			{Version: "v0.1.0", OS: "darwin", Arch: "arm64", Filename: "cclaude_v0.1.0_darwin_arm64", SizeBytes: 3, SHA256: strings.Repeat("aa", 32)},
			{Version: "v0.1.0", OS: "linux", Arch: "amd64", Filename: "cclaude_v0.1.0_linux_amd64", SizeBytes: 3, SHA256: strings.Repeat("bb", 32)},
			{Version: "v0.1.0", OS: "windows", Arch: "amd64", Filename: "cclaude_v0.1.0_windows_amd64.exe", SizeBytes: 3, SHA256: strings.Repeat("cc", 32)},
		},
		resolve: map[string]string{"aaaa000000000001": "api-03.example.com"},
	}
	var logs bytes.Buffer
	s := New(f, dist, func(format string, a ...any) { fmt.Fprintf(&logs, format+"\n", a...) })
	return s, f, &logs, dist
}

func get(t *testing.T, s *Server, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// 旧的 /quickstart 已并入 /connect（那里能给出填好域名的真命令，
// 而 quickstart 只能给占位符）。**必须重定向而不是404**：下载页与
// 外部链接都指过它。
func TestQuickstartRedirectsToConnect(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	w := get(t, s, "/quickstart", nil)
	if w.Code != 301 || w.Header().Get("Location") != "/connect" {
		t.Errorf("应301到/connect，got %d %s", w.Code, w.Header().Get("Location"))
	}
}

// 页面自包含：不引用任何外部CDN/字体/脚本（设计§3.3）。
func TestPagesSelfContained(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	for _, p := range []string{"/", "/download", "/connect"} {
		w := get(t, s, p, nil)
		if w.Code != 200 {
			t.Fatalf("%s: code=%d", p, w.Code)
		}
		body := w.Body.String()
		for _, banned := range []string{"cdn.", "googleapis", "unpkg", "jsdelivr", `src="http`, `href="http`} {
			if strings.Contains(body, banned) {
				t.Errorf("%s 引用了外部资源（%q）", p, banned)
			}
		}
	}
}

// 落地页措辞边界（CLAUDE.md）：用量只称"内部额度"，不得出现官方订阅百分比暗示。
func TestHomeCopyBoundary(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	body := get(t, s, "/", nil).Body.String()
	if !strings.Contains(body, "内部额度") {
		t.Error("落地页提及额度时应使用「内部额度」措辞")
	}
	for _, banned := range []string{"官方订阅", "订阅百分比", "Max额度", "保证不超过"} {
		if strings.Contains(body, banned) {
			t.Errorf("落地页出现越界措辞: %q", banned)
		}
	}
}

func TestDownloadPage(t *testing.T) {
	s, f, _, _ := newTestServer(t)
	body := get(t, s, "/download", map[string]string{"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"}).Body.String()
	for _, a := range f.arts {
		if !strings.Contains(body, a.Filename) {
			t.Errorf("下载表应列出 %s", a.Filename)
		}
	}
	// SHA256 不再逐行显示——那一列在页面上被截成 "88d167a6…"，既核对不了也读不懂。
	// **校验能力必须还在**：改由 SHA256SUMS 承担，这条断言守的就是它没被一起简化掉。
	if !strings.Contains(body, "/dist/SHA256SUMS") {
		t.Error("必须提供 SHA256SUMS 供校验")
	}
	if !strings.Contains(body, "shasum -a 256 -c") {
		t.Error("要给出校验命令，否则等于没提供校验")
	}
	if !strings.Contains(body, "windows/amd64") {
		t.Error("Windows UA应得到windows/amd64推荐")
	}

	f.hasRel = false
	body = get(t, s, "/download", nil).Body.String()
	if !strings.Contains(body, "暂无发布") {
		t.Error("无发布版本时应显示「暂无发布」")
	}
}

func TestDownloadRedirect(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	w := get(t, s, "/download/linux/amd64", nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/dist/cclaude_v0.1.0_linux_amd64" {
		t.Errorf("code=%d location=%s", w.Code, w.Header().Get("Location"))
	}
	if w := get(t, s, "/download/plan9/mips", nil); w.Code != 404 {
		t.Errorf("未知平台应404，got %d", w.Code)
	}
}

func TestSums(t *testing.T) {
	s, f, _, _ := newTestServer(t)
	body := get(t, s, "/dist/SHA256SUMS", nil).Body.String()
	want := fmt.Sprintf("%s  %s\n", f.arts[0].SHA256, f.arts[0].Filename)
	if !strings.Contains(body, want) {
		t.Errorf("SHA256SUMS格式应兼容shasum -c，got:\n%s", body)
	}
}

// /dist只发已登记的文件：磁盘上多出的任何文件都不可达（半成品保护，设计§3.2）。
func TestDistServesOnlyRegistered(t *testing.T) {
	s, _, logs, dist := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dist, "cclaude_v0.1.0_linux_amd64"), []byte("bin"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "draft-binary"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := get(t, s, "/dist/cclaude_v0.1.0_linux_amd64", nil)
	if w.Code != 200 || w.Body.String() != "bin" {
		t.Errorf("已登记产物应可下载: code=%d body=%q", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("应以附件形式下发，got %q", cd)
	}

	if w := get(t, s, "/dist/draft-binary", nil); w.Code != 404 {
		t.Errorf("未登记文件必须404，got %d", w.Code)
	}
	// 已登记但磁盘缺失：404并留一条漂移日志（只含文件名）。
	if w := get(t, s, "/dist/cclaude_v0.1.0_darwin_arm64", nil); w.Code != 404 {
		t.Errorf("磁盘缺失应404，got %d", w.Code)
	}
	if !strings.Contains(logs.String(), "磁盘缺失") {
		t.Error("发布漂移应留日志")
	}
}

func TestConnectPageSafety(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	body := get(t, s, "/connect", nil).Body.String()
	for _, want := range []string{`autocomplete="off"`, "noscript", "publicIDOf", "密钥部分永不离开你的设备"} {
		if !strings.Contains(body, want) {
			t.Errorf("/connect缺少%q", want)
		}
	}
}

func postResolve(t *testing.T, s *Server, payload string, ip string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/resolve", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestResolveHappyPath(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	w := postResolve(t, s, `{"public_id":"aaaa000000000001"}`, "203.0.113.9")
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["api"] != "https://api-03.example.com" {
		t.Errorf("api=%q", resp["api"])
	}
}

// 设计§6.6：收到含"."的输入（完整CDK被直接POST）→ 立即拒绝且不记录请求体。
func TestResolveRejectsFullCDKAndNeverLogsBody(t *testing.T) {
	s, _, logs, _ := newTestServer(t)
	secret := "SUPERSECRETMARKER1234"
	w := postResolve(t, s, `{"public_id":"aaaa000000000001.`+secret+`"}`, "203.0.113.9")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("含.的输入应400，got %d", w.Code)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("请求体（可能含CDK secret）绝不能进日志")
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("响应也不得回显输入")
	}
}

// 统一「未找到」：未知、格式不对都是404 not_found，不区分原因。
func TestResolveUniformNotFound(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	for _, payload := range []string{
		`{"public_id":"ffffffffffffffff"}`, // 不存在
		`{"public_id":"XYZ"}`,              // 格式不对
		`{"public_id":""}`,
	} {
		w := postResolve(t, s, payload, "203.0.113.9")
		if w.Code != 404 {
			t.Errorf("%s: code=%d, want 404", payload, w.Code)
		}
		if !strings.Contains(w.Body.String(), "not_found") {
			t.Errorf("%s: 应统一not_found，got %s", payload, w.Body.String())
		}
	}
}

func TestResolveRateLimit(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	s.MaxResolvePerMin = 3
	for i := 0; i < 3; i++ {
		if w := postResolve(t, s, `{"public_id":"aaaa000000000001"}`, "198.51.100.7"); w.Code != 200 {
			t.Fatalf("第%d次应通过，got %d", i+1, w.Code)
		}
	}
	if w := postResolve(t, s, `{"public_id":"aaaa000000000001"}`, "198.51.100.7"); w.Code != http.StatusTooManyRequests {
		t.Errorf("超限应429，got %d", w.Code)
	}
	// 其他IP不受影响（按IP维度限速）。
	if w := postResolve(t, s, `{"public_id":"aaaa000000000001"}`, "198.51.100.8"); w.Code != 200 {
		t.Errorf("不同IP不应被牵连，got %d", w.Code)
	}
}

func TestResolveOversizedBody(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	w := postResolve(t, s, `{"public_id":"`+strings.Repeat("a", 4096)+`"}`, "203.0.113.9")
	if w.Code != http.StatusBadRequest {
		t.Errorf("超大请求体应400，got %d", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	if w := get(t, s, "/healthz", nil); w.Code != 200 || w.Body.String() != "ok" {
		t.Errorf("healthz: %d %q", w.Code, w.Body.String())
	}
}

func TestDetectPlatform(t *testing.T) {
	cases := []struct{ ua, os, arch string }{
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64)", "windows", "amd64"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", "darwin", "arm64"}, // mac UA不区分芯片，默认arm64
		{"Mozilla/5.0 (X11; Linux x86_64)", "linux", "amd64"},
		{"Mozilla/5.0 (X11; Linux aarch64)", "linux", "arm64"},
	}
	for _, c := range cases {
		osName, arch, ok := detectPlatform(c.ua)
		if !ok || osName != c.os || arch != c.arch {
			t.Errorf("detectPlatform(%q) = %s/%s/%v, want %s/%s", c.ua, osName, arch, ok, c.os, c.arch)
		}
	}
	if _, _, ok := detectPlatform("curl/8.0"); ok {
		t.Error("未知UA不应给推荐")
	}
}
