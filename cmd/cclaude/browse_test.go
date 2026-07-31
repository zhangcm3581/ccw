package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 导航的边界比画界面难写对，单独测。
func TestBrowseNavigation(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{"alpha", "beta", "beta/inner", ".hidden"} {
		os.MkdirAll(filepath.Join(base, d), 0o755)
	}
	os.WriteFile(filepath.Join(base, "file.txt"), []byte("x"), 0o644)

	st := newBrowseState(base)
	// 第一项恒为 ".."，随后只有目录、按名字排序；隐藏目录与文件都不列
	if len(st.entries) != 3 {
		t.Fatalf("应为 ..、alpha、beta 三项，got %d：%+v", len(st.entries), st.entries)
	}
	if !st.entries[0].isUp || st.entries[1].name != "alpha" || st.entries[2].name != "beta" {
		t.Errorf("顺序不对：%+v", st.entries)
	}

	// 进入 beta 能看到 inner
	st.sel = 2
	st = st.enter()
	if filepath.Base(st.dir) != "beta" {
		t.Fatalf("应进入 beta，got %s", st.dir)
	}
	var names []string
	for _, e := range st.entries {
		names = append(names, e.name)
	}
	if !contains(names, "inner") {
		t.Errorf("beta 下应有 inner，got %v", names)
	}

	// 上一级回到 base
	st = st.up()
	if resolvePath(st.dir) != resolvePath(base) {
		t.Errorf("上一级应回到 %s，got %s", base, st.dir)
	}

	// 「选定当前目录」返回的就是当前所在目录
	if resolvePath(st.current()) != resolvePath(base) {
		t.Errorf("current() 应为 %s，got %s", base, st.current())
	}
}

// 读不动的目录不能让浏览器卡死或退出——要能看到原因并返回上一级。
func TestBrowseUnreadableDirDoesNotTrap(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("需要非 root 的 unix 权限语义")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "locked")
	os.MkdirAll(locked, 0o000)
	defer os.Chmod(locked, 0o755)

	st := newBrowseState(locked)
	if len(st.entries) == 0 {
		t.Fatal("至少要有一条 .. 让人能退出去")
	}
	if !st.entries[0].isUp {
		t.Error("第一项必须是上一级")
	}
	// 错误行不可进入（回车原地不动，而不是崩或跳到奇怪的地方）
	st.sel = len(st.entries) - 1
	if st.entries[st.sel].isErr {
		if got := st.enter(); got.dir != st.dir {
			t.Errorf("错误行不该能进入：%s → %s", st.dir, got.dir)
		}
	}
}

// 到达文件系统根之后再上一级不能死循环。
func TestBrowseUpAtRootTerminates(t *testing.T) {
	root := "/"
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	st := newBrowseState(root)
	for i := 0; i < 5; i++ {
		st = st.up()
	}
	if st.dir == "" {
		t.Error("反复上一级不该产生空目录")
	}
}

// 走目录树选中一个目录：↓ 到 alpha → Enter 进入 → s 选定。
func TestBrowseForDirSelects(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "alpha")
	os.MkdirAll(target, 0o755)

	var out bytes.Buffer
	io_ := pickerIO{out: &out, isTTY: true}
	rd := bufio.NewReader(strings.NewReader(kDown + kEnt + "s"))
	got := browseForDir(io_, rd, base)
	if resolvePath(got) != resolvePath(target) {
		t.Errorf("应选中 %s，got %s", target, got)
	}
	if !strings.Contains(out.String(), "选择目录") {
		t.Error("界面应有标题")
	}
}

// Esc 取消必须返回空——否则会登记一个用户并没有选的目录。
func TestBrowseForDirCancel(t *testing.T) {
	base := t.TempDir()
	var out bytes.Buffer
	io_ := pickerIO{out: &out, isTTY: true}
	rd := bufio.NewReader(strings.NewReader("\x1b"))
	if got := browseForDir(io_, rd, base); got != "" {
		t.Errorf("Esc 应取消，got %q", got)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
