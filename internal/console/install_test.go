package console

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ccw/internal/consolestore"
)

// 一键安装脚本是**要被 shell / PowerShell 直接执行的纯文本**。
//
// 两条硬性质：
//   - 不能经 HTML 转义。用 html/template 渲染的话引号会变成 &#34;，
//     脚本一执行就语法错误——这是最容易在"顺手统一模板引擎"时踩回去的坑。
//   - 站点地址跟随请求 Host。脚本里的下载地址写死域名，换域名或本地开发就断。
func TestInstallScriptIsExecutablePlainText(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	cases := []struct{ path, wantVerify string }{
		{"/install.sh", "sha256sum"},
		{"/install.ps1", "Get-FileHash"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		req.Host = "ccw.example.com"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s: got %d", c.path, w.Code)
		}
		body := w.Body.String()
		for _, bad := range []string{"&#34;", "&amp;", "&#39;"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: 输出含HTML转义%q，脚本无法执行", c.path, bad)
			}
		}
		// 只断言host跟随；scheme由X-Forwarded-Proto决定（见下一条），
		// 这里没有TLS也没有该头，得到http是对的。
		if !strings.Contains(body, "://ccw.example.com") {
			t.Errorf("%s: 站点地址应跟随请求Host", c.path)
		}
		if !strings.Contains(body, c.wantVerify) {
			t.Errorf("%s: 缺少校验和验证（%s）", c.path, c.wantVerify)
		}
	}
}

// 反代在前面时 scheme 取 X-Forwarded-Proto：Caddy 终止 TLS，
// 后端看到的是明文连接，只看 r.TLS 会把 https 站点的安装命令写成 http。
func TestInstallScriptHonorsForwardedProto(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/install.sh", nil)
	req.Host = "ccw.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "https://ccw.example.com") {
		t.Error("有X-Forwarded-Proto时应按https拼站点地址")
	}
}

// 校验和必须内嵌。脚本与产物同源，再去同源取一份SHA256SUMS校验不了任何东西；
// 内嵌之后用户可以先在浏览器读一遍脚本，看到的就是将要安装的那份二进制的指纹。
func TestInstallScriptEmbedsChecksums(t *testing.T) {
	s, f, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/install.sh", nil)
	req.Host = "ccw.example.com"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	for _, a := range f.arts {
		if a.OS == "windows" {
			continue // sh 脚本不含 windows 产物
		}
		if !strings.Contains(body, a.SHA256) {
			t.Errorf("install.sh 缺 %s/%s 的校验和", a.OS, a.Arch)
		}
		if !strings.Contains(body, a.Filename) {
			t.Errorf("install.sh 缺 %s/%s 的文件名", a.OS, a.Arch)
		}
	}
}

// 还没发布任何版本时，脚本不能是空的——`curl | sh` 执行空文件会静默成功，
// 用户只会看到"装完了但没有 cclaude 命令"，比明确报错难查得多。
func TestInstallScriptFailsLoudlyWithoutRelease(t *testing.T) {
	s, f, _, _ := newTestServer(t)
	f.hasRel = false

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/install.sh", nil))
	body := w.Body.String()
	if w.Code == 200 {
		t.Error("无已发布版本时不应返回200")
	}
	if !strings.Contains(body, "exit 1") {
		t.Errorf("脚本应以非零码退出而不是静默成功: %q", body)
	}
}

// 真正跑一遍安装脚本。
//
// **之前的测试只断言脚本文本里有校验和与文件名，从没执行过它**——
// 于是脚本里写着 `tar xzf` 而 build-release.sh 产出的是裸二进制这件事，
// 四条测试全绿地漏了过去。安装器是要在别人机器上跑的东西，
// 只测"文本里有那几个字"等于没测。
func TestInstallScriptActuallyInstalls(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	s, f, _, dist := newTestServer(t)

	// 产物就是可执行文件本身，与 scripts/build-release.sh 的产出形态一致
	const payload = "#!/bin/sh\necho fake-cclaude\n"
	sum := sha256.Sum256([]byte(payload))
	f.arts = []consolestore.Artifact{{
		Version: "v0.1.0", OS: runtime.GOOS, Arch: runtime.GOARCH,
		Filename:  "cclaude_v0.1.0_" + runtime.GOOS + "_" + runtime.GOARCH,
		SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(sum[:]),
	}}
	if err := os.WriteFile(filepath.Join(dist, f.arts[0].Filename), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	script := fetch(t, srv.URL+"/install.sh")
	dest := t.TempDir()
	out, err := runSh(t, script, "CCLAUDE_INSTALL_DIR="+dest)
	if err != nil {
		t.Fatalf("安装脚本执行失败: %v\n%s", err, out)
	}

	bin := filepath.Join(dest, "cclaude")
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("cclaude 没被装进目标目录: %v\n%s", err, out)
	}
	if fi.Mode()&0o111 == 0 {
		t.Error("安装后应是可执行的")
	}
	got, _ := os.ReadFile(bin)
	if string(got) != payload {
		t.Errorf("装进去的不是下载到的那个文件:\n%q", string(got))
	}
}

// 校验和不符必须中止且非零退出——这是这个脚本存在的全部意义。
func TestInstallScriptAbortsOnChecksumMismatch(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	s, f, _, dist := newTestServer(t)
	f.arts = []consolestore.Artifact{{
		Version: "v0.1.0", OS: runtime.GOOS, Arch: runtime.GOARCH,
		Filename:  "cclaude_v0.1.0_" + runtime.GOOS + "_" + runtime.GOARCH,
		SizeBytes: 3, SHA256: strings.Repeat("ab", 32), // 与实际内容不符
	}}
	if err := os.WriteFile(filepath.Join(dist, f.arts[0].Filename), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	dest := t.TempDir()
	out, err := runSh(t, fetch(t, srv.URL+"/install.sh"), "CCLAUDE_INSTALL_DIR="+dest)
	if err == nil {
		t.Fatalf("校验和不符时必须失败退出:\n%s", out)
	}
	if !strings.Contains(out, "校验和不符") {
		t.Errorf("应说明是校验和问题: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dest, "cclaude")); !os.IsNotExist(err) {
		t.Error("校验失败时不得留下任何文件")
	}
}

func fetch(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runSh(t *testing.T, script string, env ...string) (string, error) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(f, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", f)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Windows 装完后必须**在当前窗口就能用**。
//
// 2026-07-30 真机（PowerShell 7.6.4）暴露：脚本只写了用户级 PATH，
// 而 `[Environment]::SetEnvironmentVariable(...,'User')` 只对将来启动的进程生效。
// `irm ... | iex` 就跑在当前会话里，于是装完立刻敲 cclaude 必然是
// "不是可识别的命令"——那句"请新开一个终端"只是把缺陷写进了提示语。
//
// 同时锁住 PATH 去重的写法：`-notlike "*$dest*"` 会把已有的 `...\cclaude2`
// 当成"已经装过"而跳过写入，命令连开新终端都救不回来（PATH 里压根没加）。
func TestInstallPS1MakesCommandUsableInCurrentSession(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/install.ps1", nil)
	req.Host = "ccw.example.com"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	// 只看**实际执行的行**：注释里会引用被否决的写法（说明为什么不用它），
	// 拿整段正文做子串匹配会把解释文字当成代码。
	var code []string
	for _, line := range strings.Split(body, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			code = append(code, line)
		}
	}
	body = strings.Join(code, "\n")

	if !strings.Contains(body, "$env:PATH = ") {
		t.Error("必须同时更新当前会话的 $env:PATH，否则装完当场不可用")
	}
	if strings.Contains(body, `-notlike "*$dest*"`) {
		t.Error("PATH 去重不能用通配比对：会把 ...\\cclaude2 误判成已安装而跳过写入")
	}
	if !strings.Contains(body, "-notcontains $dest") {
		t.Error("PATH 去重应按分号切段做精确比对")
	}
	// 终端输出里不该有 markdown 记号——它会被原样打出来
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "Write-Host") && strings.Contains(line, "**") {
			t.Errorf("终端输出含 markdown 记号，会被原样打印：%s", strings.TrimSpace(line))
		}
	}
}
