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
	for _, p := range []string{"/", "/connect"} {
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

// 下载页已移除（2026-08-01）：安装命令在 /connect 上，那里还能填好域名。
// 这条测试守的是移除之后**不能一起消失**的东西。
func TestDownloadRemovedButArtifactsStillReachable(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	// 旧地址重定向而不是 404——导航与外部链接都指过它
	w := get(t, s, "/download", nil)
	if w.Code != 301 || w.Header().Get("Location") != "/connect" {
		t.Errorf("/download 应 301 到 /connect，got %d %s", w.Code, w.Header().Get("Location"))
	}
	// **产物必须仍能拿到**：install.sh/ps1 就从 /dist/ 取，断了整条安装链路就断了
	if w := get(t, s, "/dist/SHA256SUMS", nil); w.Code != 200 {
		t.Errorf("SHA256SUMS 应仍可下载，got %d", w.Code)
	}
	if w := get(t, s, "/download/darwin/arm64", nil); w.Code != 302 {
		t.Errorf("/download/{os}/{arch} 应仍可用（移除页面后唯一的手动入口），got %d", w.Code)
	}
	// 安装命令要能在 /connect 上拿到
	body := get(t, s, "/connect", nil).Body.String()
	for _, want := range []string{"/install.sh | sh", "/install.ps1 | iex"} {
		if !strings.Contains(body, want) {
			t.Errorf("/connect 应给出安装命令 %q", want)
		}
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
	// 页面上那句隐私说明已按要求去掉（2026-08-01），但**机制必须还在**：
	// publicIDOf 在本地切分，只有 "." 之前的公开 ID 会发出去。
	// 这几条守的是行为，不是文案——文案可以改，行为不能悄悄变。
	for _, want := range []string{`autocomplete="off"`, "noscript", "publicIDOf"} {
		if !strings.Contains(body, want) {
			t.Errorf("/connect缺少%q", want)
		}
	}
	// 发出去的只能是 public_id；整串 CDK 绝不能进请求体。
	if !strings.Contains(body, "public_id: pid") {
		t.Error("/connect 必须只上传切分后的公开 ID")
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

// favicon 必须内联（data URI），不能是外链。
// 公开站有一条"不引用任何外部资源"的规矩，一个 <link rel=icon href=http...>
// 就会破掉它，而且那类断链平时不会报错、只是图标不见了。
func TestFaviconIsInline(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	body := get(t, s, "/", nil).Body.String()
	if !strings.Contains(body, `rel="icon" href="data:image/svg+xml,`) {
		t.Error("favicon 应内联为 data URI")
	}
}

// 首页介绍的必须是**已经实现**的能力。这几条各自对应一处真实实现，
// 少了任何一条都说明介绍与产品脱节了。
func TestHomeDescribesShippedCapabilities(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	body := get(t, s, "/", nil).Body.String()
	for _, want := range []string{
		"tmux", // 断线不中断
		"冲突副本", // 三方同步的冲突处理
		"同步目录", // 桌面同步目录与项目选择器
		"云端",   // 云端副本管理
		".env", // 排除清单（安全边界）
	} {
		if !strings.Contains(body, want) {
			t.Errorf("首页应介绍到 %q", want)
		}
	}
}
