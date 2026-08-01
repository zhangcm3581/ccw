package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 「登记当前目录」绝不能在会造成灾难的地方出现。
//
// 在盘符根、家目录、或同步目录自身里敲 cclaude 时，把整棵树当成一个项目
// 会把几十 GB 无关文件推上云端——而用户只是回车了一下。
func TestCurrentDirHintRefusesDangerousDirs(t *testing.T) {
	root := t.TempDir()
	home, _ := os.UserHomeDir()

	if got := currentDirHint(root, root); got != "" {
		t.Errorf("同步目录自身不该提示登记，got %q", got)
	}
	if got := currentDirHint(root, filepath.Join(root, "sub")); got != "" {
		t.Errorf("同步目录内部不该提示登记（子文件夹本身就是项目），got %q", got)
	}
	if home != "" {
		if got := currentDirHint(root, home); got != "" {
			t.Errorf("家目录不该提示登记，got %q", got)
		}
	}
	fsRoot := "/"
	if filepath.Separator == '\\' {
		fsRoot = `C:\`
	}
	if got := currentDirHint(root, fsRoot); got != "" {
		t.Errorf("根目录不该提示登记，got %q", got)
	}

	// 普通工程目录应该提示
	proj := t.TempDir()
	if got := currentDirHint(root, proj); got == "" {
		t.Errorf("普通目录应提示登记，got 空")
	}
}

// --dir 必须跳过选择器：脚本与 CI 不该被交互界面挡住。
func TestResolveWorkDirHonorsFlag(t *testing.T) {
	cfg := t.TempDir()
	want := t.TempDir()
	got, err := resolveWorkDir(cfg, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// 指向文件而不是目录时要明确报错，而不是默默同步它的父目录
	f := filepath.Join(want, "x.txt")
	os.WriteFile(f, []byte("x"), 0o644)
	if _, err := resolveWorkDir(cfg, f, nil); err == nil {
		t.Error("--dir 指向文件应报错")
	}
}

// 当前目录已属于某个项目时直接用它，不弹选择器。
// 在工程的**子目录**里跑也要落到同一个项目根——否则 src/ 与工程根会变成
// 两个互不相干的云端工作区。
func TestResolveWorkDirUsesEnclosingProject(t *testing.T) {
	root := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("CCLAUDE_SYNC_DIR", root)

	proj := filepath.Join(root, "code")
	sub := filepath.Join(proj, "src", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	got, err := resolveWorkDir(cfg, "", nil)
	if err != nil {
		t.Fatalf("不该弹选择器：%v", err)
	}
	if filepath.Clean(got) != filepath.Clean(proj) {
		t.Errorf("子目录应落到项目根 %q，got %q", proj, got)
	}
}

// 同步目录被删掉之后必须自己长回来。
func TestSyncRootRecreatedAfterDeletion(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "同步目录")
	t.Setenv("CCLAUDE_SYNC_DIR", root)

	if _, err := ensureSyncRoot(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	got, err := ensureSyncRoot()
	if err != nil {
		t.Fatalf("删除后应能重建：%v", err)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Errorf("重建后应存在：%v", err)
	}
}

func TestSyncRootHonorsEnvOverride(t *testing.T) {
	want := t.TempDir()
	t.Setenv("CCLAUDE_SYNC_DIR", want)
	if got := syncRoot(); filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// 默认路径必须落在桌面下，且用约定的名字（安装脚本要建同一个）
	os.Unsetenv("CCLAUDE_SYNC_DIR")
	if d := syncRoot(); !strings.HasSuffix(d, SyncDirName) {
		t.Errorf("默认同步目录名应为 %q，got %q", SyncDirName, d)
	}
}

// 安装脚本建的目录名必须与客户端用的一致。
// 不一致的表现是"桌面上有个文件夹，但客户端在另一个地方找项目"——
// 而且两边都不报错。
func TestInstallScriptsUseSameSyncDirName(t *testing.T) {
	for _, p := range []string{
		"../../internal/console/templates/install.sh",
		"../../internal/console/templates/install.ps1",
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), SyncDirName) {
			t.Errorf("%s 里的同步目录名与 SyncDirName(%q) 不一致", p, SyncDirName)
		}
	}
}
