package sync

import (
	"testing"
	"time"
)

func srv(path, sha string, rev int64, del bool) FileEntry {
	return FileEntry{Path: path, SHA256: sha, Revision: rev, Deleted: del, Size: 10}
}

func loc(path string, baseRev int64, baseSHA, curSHA string, st LocalState) LocalEntry {
	return LocalEntry{Path: path, BaseRevision: baseRev, BaseSHA256: baseSHA, CurrentSHA256: curSHA, State: st, Size: 10}
}

func TestDiffCleanFollowsServer(t *testing.T) {
	// 本地未修改（current==base），服务端已前进 → 下载，绝不上传
	local := []LocalEntry{loc("a.go", 1, "s1", "s1", StateClean)}
	remote := []FileEntry{srv("a.go", "s9", 5, false)}
	p := Diff(local, remote)
	if len(p.Download) != 1 || p.Download[0].SHA256 != "s9" || len(p.Upload)+len(p.Conflicts) != 0 {
		t.Fatalf("clean local must follow server: %+v", p)
	}
}

func TestDiffModifiedOnCurrentBaseUploads(t *testing.T) {
	// 本地已修改，服务端仍在同一基线 → CAS上传
	local := []LocalEntry{loc("a.go", 3, "s1", "s2", StateModified), loc("new.go", 0, "", "n1", StateModified)}
	remote := []FileEntry{srv("a.go", "s1", 3, false)}
	p := Diff(local, remote)
	if len(p.Upload) != 2 || len(p.Conflicts) != 0 {
		t.Fatalf("want 2 uploads no conflicts, got %+v", p)
	}
}

func TestDiffBothModifiedIsConflict(t *testing.T) {
	// 本地基于rev2修改，服务端已到rev4且内容不同 → 冲突，禁止任何静默传输
	local := []LocalEntry{loc("a.go", 2, "s1", "local-sha", StateModified)}
	remote := []FileEntry{srv("a.go", "remote-sha", 4, false)}
	p := Diff(local, remote)
	if len(p.Conflicts) != 1 || p.Conflicts[0].Path != "a.go" {
		t.Fatalf("want conflict on a.go, got %+v", p)
	}
	if len(p.Upload)+len(p.Download) != 0 {
		t.Fatal("conflict must not silently transfer")
	}
}

func TestDiffStaleLocalIsNotAWin(t *testing.T) {
	// 关键回归（审查§2.4）：本地是未修改的旧版本，不能因为"看起来不同"而上传覆盖服务端
	local := []LocalEntry{loc("a.go", 2, "s-old", "s-old", StateClean)}
	remote := []FileEntry{srv("a.go", "s-new", 7, false)}
	p := Diff(local, remote)
	if len(p.Upload) != 0 || len(p.Download) != 1 {
		t.Fatalf("stale clean copy must download, never upload: %+v", p)
	}
}

func TestDiffDelete(t *testing.T) {
	local := []LocalEntry{loc("gone.go", 3, "s", "", StateDeleted)}
	remote := []FileEntry{srv("gone.go", "s", 3, false)}
	p := Diff(local, remote)
	if len(p.DeleteToRemote) != 1 {
		t.Fatalf("deletion on current base must propagate: %+v", p)
	}
	// 删除遇到服务端新版本 → 冲突（保留服务端）
	remote2 := []FileEntry{srv("gone.go", "s-new", 5, false)}
	p2 := Diff(local, remote2)
	if len(p2.DeleteToRemote) != 0 || len(p2.Conflicts) != 1 {
		t.Fatalf("delete vs newer server must conflict: %+v", p2)
	}
}

func TestBuildLocalDerivesState(t *testing.T) {
	base := []LocalEntry{
		loc("keep.go", 4, "k1", "k1", StateClean),
		loc("edit.go", 4, "e1", "e1", StateClean),
		loc("del.go", 4, "d1", "d1", StateClean),
	}
	// 扫描：keep 未变、edit 内容变了、del 消失、fresh 新增
	scanned := []FileEntry{
		{Path: "keep.go", SHA256: "k1", Size: 1},
		{Path: "edit.go", SHA256: "e2", Size: 1},
		{Path: "fresh.go", SHA256: "f1", Size: 1},
	}
	got := map[string]LocalState{}
	for _, e := range BuildLocal(scanned, base) {
		got[e.Path] = e.State
	}
	if got["keep.go"] != StateClean || got["edit.go"] != StateModified ||
		got["fresh.go"] != StateModified || got["del.go"] != StateDeleted {
		t.Fatalf("state derivation wrong: %+v", got)
	}
}

func TestConflictName(t *testing.T) {
	at := time.Date(2026, 7, 19, 8, 30, 0, 0, time.UTC)
	got := ConflictName("src/a.go", "laptop", at)
	want := "src/a.go.conflict-laptop-20260719T083000Z"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
