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
	// 空工作区退回legacy：管理员授权那条路用的就是 -t main
	if _, legacy := Names("pid-1", ""); legacy != "main" {
		t.Errorf("空工作区应退回main，got %s", legacy)
	}
}

func TestAttachCmdNeverKills(t *testing.T) {
	cmd := AttachCmd("ccw-project-a", "pid-1", "code-3f9a1b7c")
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
	cmds := EnsureSessionCmds("ccw-project-a", "pid-1", "code-3f9a1b7c")
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
