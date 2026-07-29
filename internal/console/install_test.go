package console

import (
	"net/http/httptest"
	"strings"
	"testing"
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
