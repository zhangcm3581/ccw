package terminal

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNamesDeterministic(t *testing.T) {
	s1, n1 := Names("pid-1", "code-3f9a1b7c")
	s2, n2 := Names("pid-1", "code-3f9a1b7c")
	if s1 != s2 || n1 != n2 || s1 != "pid-1" || n1 != "code-3f9a1b7c" {
		t.Fatalf("names must be stable: %s/%s vs %s/%s", s1, n1, s2, n2)
	}
	// 不同工作区必须是不同会话，否则两个本地目录会附着到同一个终端
	if _, other := Names("pid-1", "work-a1b2c3d4"); other == n1 {
		t.Error("不同工作区应得到不同会话名")
	}
}

func TestAttachCmdNeverKills(t *testing.T) {
	cmd := AttachCmd("ccw-project-a", "pid-1", "code-3f9a1b7c", "xterm-256color")
	joined := strings.Join(cmd, " ")
	if strings.Contains(joined, "kill") {
		t.Fatalf("attach must never contain kill: %q", joined)
	}
	if !strings.Contains(joined, "attach-session") {
		t.Fatalf("must attach existing session: %q", joined)
	}
	// 审查§2.1：必须为容器内分配TTY，否则tmux attach失败
	if !strings.Contains(joined, "-it") {
		t.Fatalf("attach must allocate a container tty (-it): %q", joined)
	}
}

func TestEnsureSessionCmdsOrder(t *testing.T) {
	cmds := EnsureSessionCmds("ccw-project-a", "pid-1", "code-3f9a1b7c", "xterm-256color")
	if len(cmds) != 3 {
		t.Fatalf("want has-session / mkdir / new-session, got %d cmds", len(cmds))
	}
	if !strings.Contains(strings.Join(cmds[0], " "), "has-session") {
		t.Fatalf("first cmd must probe session: %v", cmds[0])
	}
	// 建目录必须排在new-session之前：-c指向不存在的目录时tmux会静默退到$HOME，
	// 表现是"终端里看不到任何同步过来的文件"。
	if !strings.Contains(strings.Join(cmds[1], " "), "mkdir -p") {
		t.Fatalf("second cmd must create the workdir: %v", cmds[1])
	}
	joined := strings.Join(cmds[2], " ")
	if !strings.Contains(joined, "new-session -d") || strings.Contains(joined, " -A") {
		t.Fatalf("third cmd must create detached session without -A: %v", cmds[2])
	}
	// 工作目录必须是工作区子目录，与同步落盘位置一致
	if !strings.Contains(joined, "-c /workspace/code-3f9a1b7c") {
		t.Fatalf("workdir must follow the workspace: %v", cmds[2])
	}
}

// 真实tmux集成：断开后会话仍在，重连能看到断开前写入的标记。
// 开发机已有tmux；无tmux的环境跳过。
func TestTmuxSurvivesDetach(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := "ccw-test-detach"
	defer exec.Command("tmux", "-L", sock, "kill-server").Run()
	if err := exec.Command("tmux", "-L", sock, "new-session", "-d", "-s", "main", "sh").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("tmux", "-L", sock, "send-keys", "-t", "main", "echo MARKER-42", "Enter").Run(); err != nil {
		t.Fatal(err)
	}
	// 模拟断开：从未attach即为detached状态；重连=capture-pane。轮询避免竞态。
	var out []byte
	for i := 0; i < 30; i++ {
		o, err := exec.Command("tmux", "-L", sock, "capture-pane", "-t", "main", "-p").Output()
		if err == nil && strings.Contains(string(o), "MARKER-42") {
			out = o
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(string(out), "MARKER-42") {
		t.Fatalf("marker lost after detach: %q", out)
	}
}

// TERM 必须显式传给容器。
//
// `docker exec -it` 会写死 TERM=xterm（2026-07-31 实测，与调用方环境无关），
// 而 xterm 只宣告 8 色——tmux 据此低估外层终端能力，Claude Code 这种重绘频繁的
// TUI 会留下重绘残影（真机上滚动后左侧一列出现上一帧的字）。
func TestTermIsPassedToContainer(t *testing.T) {
	joined := strings.Join(AttachCmd("ccw-a", "pid", "code-3f9a1b7c", "xterm-256color"), " ")
	if !strings.Contains(joined, "-e TERM=xterm-256color") {
		t.Errorf("attach 未传 TERM：%s", joined)
	}
	// 建会话那条也要传：会话建立时的 TERM 决定 tmux server 的初始判断
	for i, c := range EnsureSessionCmds("ccw-a", "pid", "code-3f9a1b7c", "screen-256color") {
		if !strings.Contains(strings.Join(c, " "), "-e TERM=screen-256color") {
			t.Errorf("第%d条命令未传 TERM：%v", i, c)
		}
	}
	// 非法值退回默认，而不是原样塞进容器环境
	joined = strings.Join(AttachCmd("ccw-a", "pid", "code-3f9a1b7c", "xterm; rm -rf /"), " ")
	if !strings.Contains(joined, "-e TERM="+DefaultTerm) {
		t.Errorf("非法 TERM 应退回默认值：%s", joined)
	}
	// 空值同样退回默认——绝不能让 docker 的 xterm 兜底
	joined = strings.Join(AttachCmd("ccw-a", "pid", "code-3f9a1b7c", ""), " ")
	if !strings.Contains(joined, "-e TERM="+DefaultTerm) {
		t.Errorf("空 TERM 应退回默认值：%s", joined)
	}
}

func TestValidTerm(t *testing.T) {
	for _, ok := range []string{"xterm", "xterm-256color", "screen.linux", "rxvt-unicode-256color", "eterm+color"} {
		if !ValidTerm(ok) {
			t.Errorf("%q 应通过", ok)
		}
	}
	for _, bad := range []string{"", "a b", "x;y", "$TERM", "x\ny", strings.Repeat("x", 65)} {
		if ValidTerm(bad) {
			t.Errorf("%q 应被拒", bad)
		}
	}
}
