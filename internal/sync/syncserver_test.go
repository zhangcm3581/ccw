package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memRevStore：内存版RevisionStore，供服务端同步会话单元测试（生产用PG）。
type memRevStore struct {
	files map[string]FileEntry
}

func newMemRevStore() *memRevStore { return &memRevStore{files: map[string]FileEntry{}} }

func (m *memRevStore) Current(_ context.Context, _, path string) (int64, int64, bool, error) {
	e, ok := m.files[path]
	if !ok {
		return 0, 0, false, nil
	}
	return e.Revision, e.Size, true, nil
}

func (m *memRevStore) Commit(_ context.Context, _ string, e FileEntry, _ string) error {
	m.files[e.Path] = e
	return nil
}

func (m *memRevStore) Manifest(_ context.Context, _ string) ([]FileEntry, error) {
	out := make([]FileEntry, 0, len(m.files))
	for _, e := range m.files {
		out = append(out, e)
	}
	return out, nil
}

func (m *memRevStore) TotalSize(_ context.Context, _ string) (int64, error) {
	var sum int64
	for _, e := range m.files {
		if !e.Deleted {
			sum += e.Size
		}
	}
	return sum, nil
}

// 配额回调：worker注入storage.Gate.Allow；测试用固定上限。
func gateAllow(limit int64) func(used, oldSize, newSize int64) error {
	return func(used, oldSize, newSize int64) error {
		if newSize <= oldSize {
			return nil
		}
		if used-oldSize+newSize > limit {
			return ErrDiskFull
		}
		return nil
	}
}

func newSession(t *testing.T, mode string, limit int64) (*SyncSession, *memRevStore) {
	t.Helper()
	store := newMemRevStore()
	s := &SyncSession{
		ProjectID:  "pa",
		Device:     "laptop",
		Mode:       mode,
		Store:      store,
		Dir:        NewDirStore(t.TempDir()),
		MaxBytes:   1 << 20,
		AllowQuota: gateAllow(limit),
	}
	return s, store
}

func sha(content string) string {
	// 复用WriteTemp的哈希：直接写一次拿sha
	return sha256hex(content)
}

func TestPutNewFileGetsRevision1(t *testing.T) {
	s, store := newSession(t, "rw", 1<<30)
	r := s.HandlePut(context.Background(), "src/a.go", 0, sha("hello"), strings.NewReader("hello"))
	if !r.OK || r.Revision != 1 {
		t.Fatalf("new file must ack revision 1, got %+v", r)
	}
	if store.files["src/a.go"].SHA256 != sha("hello") {
		t.Fatalf("file not committed: %+v", store.files)
	}
}

func TestPutConflictOnStaleBaseRevision(t *testing.T) {
	s, store := newSession(t, "rw", 1<<30)
	store.files["a.go"] = FileEntry{Path: "a.go", Size: 3, SHA256: "old", Revision: 5}
	// 客户端以为基线是 rev 2，但服务端已到 5 → conflict
	r := s.HandlePut(context.Background(), "a.go", 2, sha("new"), strings.NewReader("new"))
	if r.OK || r.Reason != "conflict" {
		t.Fatalf("stale base_revision must conflict, got %+v", r)
	}
}

func TestPutShaMismatch(t *testing.T) {
	s, _ := newSession(t, "rw", 1<<30)
	r := s.HandlePut(context.Background(), "a.go", 0, "wrong-sha", strings.NewReader("hello"))
	if r.OK || r.Reason != "sha_mismatch" {
		t.Fatalf("declared sha mismatch must reject, got %+v", r)
	}
}

func TestPutTooLarge(t *testing.T) {
	s, _ := newSession(t, "rw", 1<<30)
	s.MaxBytes = 4
	r := s.HandlePut(context.Background(), "big", 0, sha("0123456789"), strings.NewReader("0123456789"))
	if r.OK || r.Reason != "too_large" {
		t.Fatalf("over MaxBytes must reject, got %+v", r)
	}
}

func TestPutDiskFull(t *testing.T) {
	s, _ := newSession(t, "rw", 5) // 配额上限5字节
	r := s.HandlePut(context.Background(), "a", 0, sha("0123456789"), strings.NewReader("0123456789"))
	if r.OK || r.Reason != "disk_full" {
		t.Fatalf("over quota must reject disk_full, got %+v", r)
	}
}

func TestCleanupModeRejectsGrowAllowsShrink(t *testing.T) {
	s, store := newSession(t, "cleanup", 1<<30)
	store.files["a"] = FileEntry{Path: "a", Size: 10, SHA256: "x", Revision: 1}
	// cleanup 模式：写入更大内容（扩大）→ readonly_mode
	grow := s.HandlePut(context.Background(), "a", 1, sha("0123456789ABC"), strings.NewReader("0123456789ABC"))
	if grow.OK || grow.Reason != "readonly_mode" {
		t.Fatalf("cleanup must reject grow, got %+v", grow)
	}
	// cleanup 模式：写入更小内容（缩小）→ 允许
	shrink := s.HandlePut(context.Background(), "a", 1, sha("hi"), strings.NewReader("hi"))
	if !shrink.OK {
		t.Fatalf("cleanup must allow shrink, got %+v", shrink)
	}
}

func TestDeleteWritesTombstone(t *testing.T) {
	s, store := newSession(t, "rw", 1<<30)
	store.files["a"] = FileEntry{Path: "a", Size: 3, SHA256: "x", Revision: 2}
	r := s.HandleDelete(context.Background(), "a", 2)
	if !r.OK || r.Revision != 3 {
		t.Fatalf("delete must ack next revision, got %+v", r)
	}
	if !store.files["a"].Deleted {
		t.Fatalf("delete must mark tombstone: %+v", store.files["a"])
	}
}

func TestDeleteConflict(t *testing.T) {
	s, store := newSession(t, "rw", 1<<30)
	store.files["a"] = FileEntry{Path: "a", Size: 3, SHA256: "x", Revision: 9}
	r := s.HandleDelete(context.Background(), "a", 2) // 基线过时
	if r.OK || r.Reason != "conflict" {
		t.Fatalf("stale delete must conflict, got %+v", r)
	}
}

func TestManifestFromStore(t *testing.T) {
	s, store := newSession(t, "rw", 1<<30)
	store.files["a"] = FileEntry{Path: "a", Revision: 1}
	store.files["b"] = FileEntry{Path: "b", Revision: 1, Deleted: true}
	m, err := s.HandleManifest(context.Background())
	if err != nil || len(m) != 2 {
		t.Fatalf("manifest must return all entries incl tombstones, got %d %v", len(m), err)
	}
}

func TestManifestReconcilesCloudEdit(t *testing.T) {
	s, store := newSession(t, "rw", 1<<30)
	// Claude 在云端 workspace 新建了一个文件，file_index 里还没有
	os.WriteFile(filepath.Join(s.Dir.Root(), "cloud.go"), []byte("by claude"), 0o644)
	m, err := s.HandleManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if store.files["cloud.go"].SHA256 != sha256hex("by claude") {
		t.Fatalf("cloud edit must be reconciled into index: %+v", store.files)
	}
	found := false
	for _, e := range m {
		if e.Path == "cloud.go" {
			found = true
		}
	}
	if !found {
		t.Fatal("manifest must include cloud edit")
	}
}

func TestManifestReconcilesCloudDelete(t *testing.T) {
	s, store := newSession(t, "rw", 1<<30)
	store.files["gone.go"] = FileEntry{Path: "gone.go", Size: 5, SHA256: "x", Revision: 2}
	// workspace 里没有 gone.go（Claude 在云端删了）
	if _, err := s.HandleManifest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.files["gone.go"].Deleted {
		t.Fatalf("cloud delete must write tombstone, got %+v", store.files["gone.go"])
	}
}

func TestReconcileSkipsUnchanged(t *testing.T) {
	s, store := newSession(t, "rw", 1<<30)
	// 客户端刚 put 的文件：workspace 有它，file_index sha 一致
	os.WriteFile(filepath.Join(s.Dir.Root(), "a.go"), []byte("same"), 0o644)
	store.files["a.go"] = FileEntry{Path: "a.go", Size: 4, SHA256: sha256hex("same"), Revision: 9}
	s.HandleManifest(context.Background())
	// revision 不应被 reconcile 抬高（sha 一致 → 跳过）
	if store.files["a.go"].Revision != 9 {
		t.Fatalf("unchanged file must not be re-revisioned, got rev %d", store.files["a.go"].Revision)
	}
}
