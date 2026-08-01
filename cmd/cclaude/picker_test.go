package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 用假终端把选择器整条跑通：造项目 → 按方向键 → 回车 → 拿到选中的项目。
func fakeIO(keys string) (pickerIO, *bytes.Buffer) {
	var out bytes.Buffer
	return pickerIO{
		in:      strings.NewReader(keys),
		out:     &out,
		isTTY:   true,
		makeRaw: func() (func(), error) { return func() {}, nil },
	}, &out
}

const (
	kDown = "\x1b[B"
	kUp   = "\x1b[A"
	kEnt  = "\r"
)

func TestPickerSelectsProject(t *testing.T) {
	root := t.TempDir()
	cfg := t.TempDir()
	for _, n := range []string{"alpha", "beta", "gamma"} {
		os.MkdirAll(filepath.Join(root, n), 0o755)
	}

	// 无 cwdHint：第一行就是 alpha。下两次到 gamma。
	io_, out := fakeIO(kDown + kDown + kEnt)
	act, err := runPicker(io_, root, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if act.Quit || filepath.Base(act.Project.Path) != "gamma" {
		t.Errorf("应选中 gamma，got %+v", act.Project)
	}
	// 三个项目都要出现在画面上
	for _, n := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(out.String(), n) {
			t.Errorf("画面缺少项目 %q", n)
		}
	}
}

// 顶部不能越界：在第一行继续按上不该跑到 -1。
func TestPickerClampsAtTop(t *testing.T) {
	root := t.TempDir()
	cfg := t.TempDir()
	os.MkdirAll(filepath.Join(root, "only"), 0o755)

	io_, _ := fakeIO(kUp + kUp + kUp + kEnt)
	act, err := runPicker(io_, root, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(act.Project.Path) != "only" {
		t.Errorf("越界后应仍停在第一项，got %+v", act.Project)
	}
}

// Esc 与 q 都要能退出，而且**不能返回一个项目**——否则会同步一个用户
// 并没有选的目录。
func TestPickerQuitReturnsNothing(t *testing.T) {
	root := t.TempDir()
	cfg := t.TempDir()
	os.MkdirAll(filepath.Join(root, "alpha"), 0o755)

	for _, keys := range []string{"q", "\x1b"} {
		io_, _ := fakeIO(keys)
		act, err := runPicker(io_, root, cfg, "")
		if err != nil {
			t.Fatal(err)
		}
		if !act.Quit || act.Project.Path != "" {
			t.Errorf("按 %q 应退出且不带项目，got %+v", keys, act)
		}
	}
}

// 「登记当前目录」在顶部且默认选中：在一个还没登记的工程里直接回车即用。
func TestPickerRegistersCurrentDir(t *testing.T) {
	root := t.TempDir()
	cfg := t.TempDir()
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(root, "alpha"), 0o755)

	io_, out := fakeIO(kEnt)
	act, err := runPicker(io_, root, cfg, proj)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(act.Project.Path) != filepath.Clean(proj) {
		t.Errorf("回车应登记当前目录，got %+v", act.Project)
	}
	if !strings.Contains(out.String(), "登记当前目录") {
		t.Error("顶部应有「登记当前目录」一行")
	}
	// 真的写进了登记表
	if got := loadRegistered(cfg); len(got) != 1 || filepath.Clean(got[0].Path) != filepath.Clean(proj) {
		t.Errorf("登记表内容不对：%+v", got)
	}
}

// 就地登记的外部目录要显示真实路径（与截图里 SyntheticProject 那行一致），
// 同步目录里的则不显示——路径对它们是冗余信息。
func TestPickerShowsPathOnlyForExternal(t *testing.T) {
	root := t.TempDir()
	cfg := t.TempDir()
	os.MkdirAll(filepath.Join(root, "inside"), 0o755)
	ext := t.TempDir()
	if err := registerProject(cfg, ext); err != nil {
		t.Fatal(err)
	}

	rows := buildRows(listProjects(root, cfg), "")
	var insideRow, extRow row
	for _, r := range rows {
		switch r.label {
		case "inside":
			insideRow = r
		case filepath.Base(ext):
			extRow = r
		}
	}
	if insideRow.hint != "" {
		t.Errorf("同步目录内的项目不该显示路径，got %q", insideRow.hint)
	}
	if !strings.Contains(extRow.hint, ext) {
		t.Errorf("外部项目应显示真实路径，got %q", extRow.hint)
	}
}

// 登记过但本地被删的项目仍要列出来，并标明选中即从云端拉回。
// 直接从列表里消失的话，用户会以为云端数据也没了。
func TestPickerKeepsMissingProjects(t *testing.T) {
	root := t.TempDir()
	cfg := t.TempDir()
	gone := filepath.Join(t.TempDir(), "deleted-project")
	os.MkdirAll(gone, 0o755)
	if err := registerProject(cfg, gone); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(gone)

	rows := buildRows(listProjects(root, cfg), "")
	if len(rows) != 1 {
		t.Fatalf("应仍列出已删除的项目，got %d 行", len(rows))
	}
	if !strings.Contains(rows[0].hint, "云端") {
		t.Errorf("应提示可从云端拉回，got %q", rows[0].hint)
	}
}

// d 只从列表里移除，绝不删文件。
func TestForgetProjectKeepsFiles(t *testing.T) {
	cfg := t.TempDir()
	proj := t.TempDir()
	f := filepath.Join(proj, "keep.txt")
	os.WriteFile(f, []byte("x"), 0o644)
	registerProject(cfg, proj)

	if err := forgetProject(cfg, proj); err != nil {
		t.Fatal(err)
	}
	if len(loadRegistered(cfg)) != 0 {
		t.Error("应已从登记表移除")
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("文件绝不能被删：%v", err)
	}
}

// 同一个目录不该被登记两次（Windows 上大小写不同也算同一个）。
func TestRegisterProjectIsIdempotent(t *testing.T) {
	cfg := t.TempDir()
	proj := t.TempDir()
	registerProject(cfg, proj)
	registerProject(cfg, proj)
	registerProject(cfg, filepath.Clean(proj)+string(filepath.Separator))
	if got := loadRegistered(cfg); len(got) != 1 {
		t.Errorf("应只有一条，got %d：%+v", len(got), got)
	}
}

// 同步目录里的子文件夹自动成为项目，不需要登记。
func TestSyncRootFoldersAreProjectsWithoutRegistration(t *testing.T) {
	root := t.TempDir()
	cfg := t.TempDir()
	os.MkdirAll(filepath.Join(root, "dropped-in"), 0o755)
	os.WriteFile(filepath.Join(root, "README.txt"), []byte("x"), 0o644) // 散落文件不算项目
	os.MkdirAll(filepath.Join(root, ".hidden"), 0o755)                  // 隐藏目录不算

	ps := listProjects(root, cfg)
	if len(ps) != 1 || ps[0].Name != "dropped-in" {
		t.Errorf("只有 dropped-in 该是项目，got %+v", ps)
	}
}

// 重绘必须擦干净——**这是本轮 code review 找到的三个 bug 的共同根因**。
//
// 原来三处各自记账"打了 N 行、往回移 N 行"，而在行数会变的时候一定错：
// 按 d 移出一项后新一帧少一行，旧的最后一行没人擦；按 n/t 之后输入提示
// 又多打几行，循环末尾仍按旧行数往回移，光标停到比起点更高的位置，
// 下一帧直接盖掉 shell 提示符。
//
// 现在只记"上一帧实际写了多少行"，重绘时移回去再 ESC[J 擦到屏幕末尾。
// 这条测试直接验那个记账：写多少行，就该往回移多少行。
func TestScreenRedrawAccounting(t *testing.T) {
	var buf bytes.Buffer
	sc := newScreen(&buf)

	// 第一帧：3 行，且**不该有任何上移**（前面没有帧）
	sc.begin()
	sc.line("a")
	sc.line("b")
	sc.line("c")
	if strings.Contains(buf.String(), "[3A") {
		t.Error("第一帧不该往回移")
	}

	// 第二帧：应先上移 3 行并擦到末尾
	buf.Reset()
	sc.begin()
	if !strings.Contains(buf.String(), esc+"[3A") {
		t.Errorf("应上移 3 行，got %q", buf.String())
	}
	if !strings.Contains(buf.String(), esc+"[J") {
		t.Error("必须擦到屏幕末尾，否则行数变少时旧行会留下")
	}
	sc.line("only-one") // 这一帧只有 1 行

	// 第三帧：按 1 行回退，而不是 3
	buf.Reset()
	sc.begin()
	if !strings.Contains(buf.String(), esc+"[1A") {
		t.Errorf("应按上一帧的真实行数(1)回退，got %q", buf.String())
	}

	// external：外部又打了 2 行，下一次必须一并回退
	sc.line("x")
	sc.external(2)
	buf.Reset()
	sc.begin()
	if !strings.Contains(buf.String(), esc+"[3A") {
		t.Errorf("应回退 1+2=3 行，got %q", buf.String())
	}
}

// 列表变短之后不能留下旧行。按 d 移出一项就是这个场景。
func TestPickerShrinkingListLeavesNoStaleRow(t *testing.T) {
	root := t.TempDir()
	cfg := t.TempDir()
	ext := t.TempDir()
	if err := registerProject(cfg, ext); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "stays"), 0o755)

	// 光标移到外部项目那行 → d 移出 → q 退出
	io_, out := fakeIO(kDown + "d" + "q")
	if _, err := runPicker(io_, root, cfg, ""); err != nil {
		t.Fatal(err)
	}
	if len(loadRegistered(cfg)) != 0 {
		t.Fatal("d 应把它移出登记表")
	}
	s := out.String()
	// 移出后那一帧必须先回退 2 行（旧帧 4 头 + 2 项 = 6）再擦到末尾
	if !strings.Contains(s, esc+"[J") {
		t.Error("重绘必须擦到屏幕末尾，否则少掉的那行会留在屏幕上")
	}
	// 退出时也要擦干净，别把界面留在 shell 提示符上面
	if !strings.HasSuffix(strings.TrimRight(s, "\x1b[?25h"), esc+"[J") {
		t.Errorf("退出前最后一个动作应是擦除，got 结尾 %q", tail(s, 24))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
