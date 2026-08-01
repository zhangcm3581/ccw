package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeManifest struct{ entries []FileEntry }

func (f *fakeManifest) Current(context.Context, string, string) (int64, int64, bool, error) {
	return 0, 0, false, nil
}
func (f *fakeManifest) Manifest(context.Context, string) ([]FileEntry, error)   { return f.entries, nil }
func (f *fakeManifest) Commit(context.Context, string, FileEntry, string) error { return nil }
func (f *fakeManifest) TotalSize(context.Context, string) (int64, error)        { return 0, nil }

func TestListWorkspacesGroupsAndSorts(t *testing.T) {
	st := &fakeManifest{entries: []FileEntry{
		{Path: "code-1a2b3c4d/a.txt", Size: 100},
		{Path: "code-1a2b3c4d/sub/b.txt", Size: 200},
		{Path: "work-9f8e7d6c/c.txt", Size: 50},
		{Path: "work-9f8e7d6c/gone.txt", Size: 999, Deleted: true}, // 墓碑不占盘
		{Path: "no-prefix-file.txt", Size: 12345},                  // 无工作区前缀：不归任何副本
	}}
	got, err := ListWorkspaces(context.Background(), st, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应有2个副本，got %+v", got)
	}
	// 大的排前面
	if got[0].WS != "code-1a2b3c4d" || got[0].Bytes != 300 || got[0].Files != 2 {
		t.Errorf("第一项应是 code（300B/2文件），got %+v", got[0])
	}
	if got[1].WS != "work-9f8e7d6c" || got[1].Bytes != 50 || got[1].Files != 1 {
		t.Errorf("墓碑不该计入大小与文件数，got %+v", got[1])
	}
}

type fakePurge struct{ called []string }

func (f *fakePurge) PurgeWorkspace(_ context.Context, _, ws string) (int64, error) {
	f.called = append(f.called, ws)
	return 42, nil
}

// **最要紧的一条**：ws 会被拼进文件系统路径，放行 ".." 就是任意目录删除。
func TestPurgeWorkspaceRejectsUnsafeKeys(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "keep-me")
	os.MkdirAll(victim, 0o755)

	for _, bad := range []string{"..", "../..", "a/../..", "/etc", "", "UPPER", "has space", "x/y"} {
		fp := &fakePurge{}
		if _, err := PurgeWorkspace(context.Background(), fp, "p1", root, bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
		if len(fp.called) != 0 {
			t.Errorf("%q 不该到达存储层", bad)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("非法键绝不能删掉任何东西：%v", err)
	}
}

func TestPurgeWorkspaceRemovesDirAndRows(t *testing.T) {
	root := t.TempDir()
	ws := "code-1a2b3c4d"
	dir := filepath.Join(root, ws)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "a.txt"), []byte("x"), 0o644)
	other := filepath.Join(root, "work-9f8e7d6c")
	os.MkdirAll(other, 0o755)

	fp := &fakePurge{}
	freed, err := PurgeWorkspace(context.Background(), fp, "p1", root, ws)
	if err != nil {
		t.Fatal(err)
	}
	if freed != 42 {
		t.Errorf("应返回存储层报的释放量，got %d", freed)
	}
	if len(fp.called) != 1 || fp.called[0] != ws {
		t.Errorf("应清索引行，got %v", fp.called)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("磁盘目录应被删除")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("别的副本不能受影响")
	}
}
