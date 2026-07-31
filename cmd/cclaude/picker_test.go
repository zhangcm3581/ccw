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
