package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 工作区键：同一目录稳定、不同目录必然不同。
func TestWorkspaceKeyStableAndDistinct(t *testing.T) {
	a1 := WorkspaceKey("/Users/neo/code")
	a2 := WorkspaceKey("/Users/neo/code/")  // 尾斜杠不该换一个工作区
	a3 := WorkspaceKey("/Users/neo/./code") // 同一目录的另一种写法
	if a1 != a2 || a1 != a3 {
		t.Errorf("同一目录应得到同一键: %s / %s / %s", a1, a2, a3)
	}

	b := WorkspaceKey("/Users/neo/work")
	if a1 == b {
		t.Error("不同目录必须是不同工作区")
	}
	// **同名不同路径也必须分开**：只用目录名做键的话这两个会撞在一起，
	// 那就是本次要修的跨目录污染的另一种形态。
	c1 := WorkspaceKey("/Users/neo/a/code")
	c2 := WorkspaceKey("/Users/neo/b/code")
	if c1 == c2 {
		t.Error("同名不同路径必须是不同工作区")
	}

	// 键要保留可读的目录名，管理员在服务器上得认得出是哪个文件夹
	if !strings.HasPrefix(a1, "code-") {
		t.Errorf("键应带可读目录名前缀: %s", a1)
	}
	if !ValidWorkspace(a1) {
		t.Errorf("自己生成的键必须能通过服务端校验: %s", a1)
	}
}

// 键里的特殊字符要被清理干净，且极端情况不能生成非法键。
func TestWorkspaceKeyAlwaysValid(t *testing.T) {
	for _, dir := range []string{
		"/", "/tmp/我的项目", `C:\Users\neo\My Code`, "/a/....", "/x/--weird--",
		"/very/" + strings.Repeat("long", 30),
	} {
		k := WorkspaceKey(dir)
		if !ValidWorkspace(k) {
			t.Errorf("WorkspaceKey(%q)=%q 未通过自己的校验", dir, k)
		}
	}
}

// 服务端校验是路径安全边界的一部分：键会成为目录名与索引路径的第一段。
func TestValidWorkspaceRejectsTraversal(t *testing.T) {
	for _, bad := range []string{
		"", "..", "../etc", "a/b", "/abs", "A-UPPER", "trailing-",
		"-leading", "has_underscore", "has.dot", "double--dash",
		strings.Repeat("x", 40), "空格 键",
	} {
		if ValidWorkspace(bad) {
			t.Errorf("应拒绝非法工作区键 %q", bad)
		}
	}
}

// 核心回归：两个工作区互不可见。
//
// 这条守的是2026-07-29真机上出的问题——在 ~/code 下用过一次，再进 ~/work，
// 云端把 code 的全部文件同步了下来。
func TestWorkspacesAreIsolated(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := newMemRevStore()

	newSess := func(ws string) *SyncSession {
		s := &SyncSession{ProjectID: "p1", Mode: "rw", Store: store, Root: root,
			MaxBytes: 1 << 20, AllowQuota: func(int64, int64, int64) error { return nil }}
		if err := s.SetWorkspace(ws); err != nil {
			t.Fatal(err)
		}
		return s
	}

	code := newSess("code-11111111")
	work := newSess("work-22222222")

	// 在 code 工作区放一个文件
	if res := code.HandlePut(ctx, "main.go", 0, sha("package main"),
		strings.NewReader("package main")); res.Reason != "" {
		t.Fatalf("put失败: %+v", res)
	}

	// work 工作区**不应该看到它**
	m, err := work.HandleManifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m {
		if e.Path == "main.go" {
			t.Fatal("跨工作区污染：work 看到了 code 的文件")
		}
	}
	if len(m) != 0 {
		t.Errorf("新工作区的清单应为空，got %+v", m)
	}

	// code 自己仍然看得到
	m2, err := code.HandleManifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2) != 1 || m2[0].Path != "main.go" {
		t.Fatalf("自己的工作区应看到自己的文件: %+v", m2)
	}

	// 文件在磁盘上也是分开的
	if _, err := os.Stat(filepath.Join(root, "code-11111111", "main.go")); err != nil {
		t.Errorf("文件应落在工作区子目录里: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "work-22222222", "main.go")); !os.IsNotExist(err) {
		t.Error("另一个工作区的目录里不该有这个文件")
	}
}

// 同名文件在两个工作区里各自独立，互不覆盖。
func TestSameNameDifferentWorkspaces(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := newMemRevStore()
	mk := func(ws string) *SyncSession {
		s := &SyncSession{ProjectID: "p1", Mode: "rw", Store: store, Root: root,
			MaxBytes: 1 << 20, AllowQuota: func(int64, int64, int64) error { return nil }}
		if err := s.SetWorkspace(ws); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := mk("aaa-11111111"), mk("bbb-22222222")

	a.HandlePut(ctx, "README.md", 0, sha("from-a"), strings.NewReader("from-a"))
	b.HandlePut(ctx, "README.md", 0, sha("from-b"), strings.NewReader("from-b"))

	cases := []struct {
		s    *SyncSession
		want string
	}{{a, "from-a"}, {b, "from-b"}}
	for _, tc := range cases {
		_, rc, err := tc.s.HandleGet(ctx, "README.md")
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 32)
		n, _ := rc.Read(buf)
		rc.Close()
		if got := string(buf[:n]); got != tc.want {
			t.Errorf("工作区%s读到了%q，want %q", tc.s.WS, got, tc.want)
		}
	}
}

func TestDisplayNameStripsHash(t *testing.T) {
	cases := map[string]string{
		"og-vault-94137d17":     "og-vault",
		"test01-f32221cc":       "test01",
		"pm-test-ec0ef851":      "pm-test",
		"ws-1a2b3c4d":           "ws-1a2b3c4d", // 只剩哈希：还原出 "ws" 毫无意义
		"no-hash-here":          "no-hash-here",
		"abc-12345678-aabbccdd": "abc-12345678",
	}
	for in, want := range cases {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

// **重名必须保留哈希**：这块界面是用来选删除对象的，两行长得一模一样
// 就等于让人蒙着眼睛删。
func TestDisplayNamesKeepsHashOnCollision(t *testing.T) {
	keys := []string{
		"code-11111111", // ~/a/code
		"code-22222222", // ~/b/code —— 同名不同路径
		"vault-33333333",
	}
	got := DisplayNames(keys)
	if got["code-11111111"] != "code-11111111" || got["code-22222222"] != "code-22222222" {
		t.Errorf("撞名时必须保留哈希，got %v", got)
	}
	if got["vault-33333333"] != "vault" {
		t.Errorf("不撞名的应省掉哈希，got %q", got["vault-33333333"])
	}
}

// 显示名与真实键必须一一对应——删除用的是键，显示错了会删错对象。
func TestDisplayNamesCoversEveryKey(t *testing.T) {
	keys := []string{"a-11111111", "b-22222222", "a-33333333"}
	got := DisplayNames(keys)
	if len(got) != len(keys) {
		t.Fatalf("每个键都要有显示名，got %v", got)
	}
	for _, k := range keys {
		if got[k] == "" {
			t.Errorf("%q 没有显示名", k)
		}
	}
}
