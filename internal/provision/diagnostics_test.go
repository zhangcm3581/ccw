package provision

import "testing"

// 诊断输出按标记切分；登录状态只在能明确判定时才给布尔。
func TestSplitDiag(t *testing.T) {
	raw := diagMarker + "容器状态\nccw-project-a\tUp 2 hours\tccw-claude\n" +
		diagMarker + "登录状态 ccw-project-a\nloggedIn: true\n" +
		diagMarker + "登录状态 ccw-project-b\nloggedIn: false\n" +
		diagMarker + "磁盘与 data-root\n/dev/vda1  75G  20G  55G  27% /\n"

	secs := splitDiag(raw)
	if len(secs) != 4 {
		t.Fatalf("应切出4段，got %d: %+v", len(secs), secs)
	}
	if secs[0].Title != "容器状态" || !contains(secs[0].Output, "Up 2 hours") {
		t.Errorf("第一段错: %+v", secs[0])
	}
	if secs[1].LoggedIn == nil || !*secs[1].LoggedIn {
		t.Errorf("应判定为已登录: %+v", secs[1])
	}
	if secs[2].LoggedIn == nil || *secs[2].LoggedIn {
		t.Errorf("应判定为未登录: %+v", secs[2])
	}
	if secs[3].LoggedIn != nil {
		t.Error("非登录段不该有登录判定")
	}
}

// **判不出来时必须留空，不能当成未登录**——那会让人白折腾一轮重新授权。
func TestSplitDiagUnknownLoginStaysNil(t *testing.T) {
	raw := diagMarker + "登录状态 ccw-a\nError: command not found\n"
	secs := splitDiag(raw)
	if len(secs) != 1 {
		t.Fatalf("got %d", len(secs))
	}
	if secs[0].LoggedIn != nil {
		t.Errorf("输出无法判定时应为nil，got %v", *secs[0].LoggedIn)
	}
	// 原文仍要给出来，让人自己看
	if !contains(secs[0].Output, "command not found") {
		t.Error("原始输出必须保留")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// 授权码只拒绝控制字符与空白，不猜码的字母表。
//
// 猜错字母表的后果是把一个合法的码挡在门外，而管理员在后台里没有任何
// 绕过的办法——base64url 有 -_，有的实现还带 = + % ?，都得能过。
func TestAuthCodeCharset(t *testing.T) {
	ok := []string{
		"abc123", "AbC-1_2", "code=with+plus/and=equals",
		"pct%20encoded", "q?a=b&c=d", "有效但含中文的码",
	}
	bad := []string{
		"", "  ", "has space", "two\nlines", "tab\there", "bell\x07",
	}
	for _, c := range ok {
		if err := checkAuthCode(c); err != nil {
			t.Errorf("应接受 %q: %v", c, err)
		}
	}
	for _, c := range bad {
		if err := checkAuthCode(c); err == nil {
			t.Errorf("应拒绝 %q", c)
		}
	}
}

// capture-pane 必须带 -J。
//
// 这条守的是一个实测过的坑：登录 URL 有 407 字符，在 200 列的 pane 里必然折行，
// capture-pane 默认按**显示行**返回，管理员从后台复制到的会是断成两截的链接。
// 2026-07-30 在 ubuntu:24.04 + tmux 3.4 上验过：不加 -J 时 URL 落在 2 行，
// 加了之后是完整的 1 行。
//
// 单测无法验证 tmux 的真实行为（CI 里没有 tmux），但能守住这个决定不被顺手删掉。
func TestCapturePaneJoinsWrappedLines(t *testing.T) {
	cmd := capturePane("sudo -n ", "ccw-project-a")
	if !contains(cmd, " -J ") {
		t.Errorf("capture-pane 必须带 -J，否则长 URL 会被按显示行截断：%s", cmd)
	}
	if !contains(cmd, "capture-pane") || !contains(cmd, "ccw-project-a") {
		t.Errorf("命令形状不对：%s", cmd)
	}
}
