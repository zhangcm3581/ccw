package terminal

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNamesDeterministic(t *testing.T) {
	s1, n1 := Names("pid-1")
	s2, n2 := Names("pid-1")
	if s1 != s2 || n1 != n2 || n1 != "main" || s1 != "pid-1" {
		t.Fatalf("names must be stable: %s/%s vs %s/%s", s1, n1, s2, n2)
	}
}

func TestAttachCmdNeverKills(t *testing.T) {
	cmd := AttachCmd("ccw-project-a", "pid-1")
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
	cmds := EnsureSessionCmds("ccw-project-a", "pid-1")
	if len(cmds) != 2 {
		t.Fatalf("want has-session then new-session, got %d cmds", len(cmds))
	}
	if !strings.Contains(strings.Join(cmds[0], " "), "has-session") {
		t.Fatalf("first cmd must probe session: %v", cmds[0])
	}
	joined := strings.Join(cmds[1], " ")
	if !strings.Contains(joined, "new-session -d") || strings.Contains(joined, " -A") {
		t.Fatalf("second cmd must create detached session without -A: %v", cmds[1])
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
