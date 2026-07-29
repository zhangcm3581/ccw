package sync

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeTransport struct {
	mode      string
	manifest  []FileEntry
	files     map[string]string // 远端 path -> content
	puts      []LocalEntry
	deletes   []LocalEntry
	rejectPut map[string]string
	nextRev   int64
}

func (f *fakeTransport) Hello(string, string) (string, error) { return f.mode, nil }
func (f *fakeTransport) Manifest() ([]FileEntry, error) {
	return f.manifest, nil
}
func (f *fakeTransport) Put(entry LocalEntry, content io.Reader) (int64, string, error) {
	if r := f.rejectPut[entry.Path]; r != "" {
		return 0, r, nil
	}
	b, _ := io.ReadAll(content)
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[entry.Path] = string(b)
	f.puts = append(f.puts, entry)
	f.nextRev++
	return f.nextRev, "", nil
}
func (f *fakeTransport) Get(path string) (FileEntry, io.ReadCloser, error) {
	c, ok := f.files[path]
	if !ok {
		return FileEntry{}, nil, errors.New("not found")
	}
	return FileEntry{Path: path, SHA256: sha256hex(c), Revision: 7, Size: int64(len(c))},
		io.NopCloser(strings.NewReader(c)), nil
}
func (f *fakeTransport) Delete(entry LocalEntry) (int64, string, error) {
	f.deletes = append(f.deletes, entry)
	f.nextRev++
	return f.nextRev, "", nil
}

func TestSyncUploadNewFileUpdatesBaseline(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "new.go"), []byte("hello"), 0o644)
	ft := &fakeTransport{mode: "rw"}
	c := &SyncClient{Root: root, Device: "laptop"}
	newBase, err := c.SyncOnce(context.Background(), ft)
	if err != nil {
		t.Fatal(err)
	}
	if len(ft.puts) != 1 || ft.puts[0].Path != "new.go" {
		t.Fatalf("must upload new.go, got %+v", ft.puts)
	}
	var b *LocalEntry
	for i := range newBase {
		if newBase[i].Path == "new.go" {
			b = &newBase[i]
		}
	}
	if b == nil || b.BaseRevision != 1 || b.State != StateClean {
		t.Fatalf("baseline must record acked revision as clean, got %+v", b)
	}
}

func TestSyncDownloadWritesFile(t *testing.T) {
	root := t.TempDir()
	ft := &fakeTransport{
		mode:     "rw",
		files:    map[string]string{"srv.go": "remote-content"},
		manifest: []FileEntry{{Path: "srv.go", Revision: 3, SHA256: sha256hex("remote-content"), Size: 14}},
	}
	c := &SyncClient{Root: root}
	if _, err := c.SyncOnce(context.Background(), ft); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "srv.go"))
	if err != nil || string(got) != "remote-content" {
		t.Fatalf("download must write file: %q %v", got, err)
	}
}

func TestCleanupModeSkipsUpload(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "new.go"), []byte("x"), 0o644)
	ft := &fakeTransport{mode: "cleanup"}
	c := &SyncClient{Root: root}
	if _, err := c.SyncOnce(context.Background(), ft); err != nil {
		t.Fatal(err)
	}
	if len(ft.puts) != 0 {
		t.Fatalf("cleanup mode must not upload, got %+v", ft.puts)
	}
}

func TestSyncConflictSavesRemoteCopy(t *testing.T) {
	root := t.TempDir()
	// 本地 a.go 相对基线已修改
	os.WriteFile(filepath.Join(root, "a.go"), []byte("local-modified"), 0o644)
	idx := LocalIndex{Root: root}
	idx.Save([]LocalEntry{{Path: "a.go", BaseRevision: 2, BaseSHA256: sha256hex("original"), State: StateClean}})
	// 服务端 a.go 也已前进到 rev4，内容不同
	ft := &fakeTransport{
		mode:     "rw",
		files:    map[string]string{"a.go": "remote-version"},
		manifest: []FileEntry{{Path: "a.go", Revision: 4, SHA256: sha256hex("remote-version")}},
	}
	c := &SyncClient{Root: root}
	if _, err := c.SyncOnce(context.Background(), ft); err != nil {
		t.Fatal(err)
	}
	// 本地原文件保留
	got, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(got) != "local-modified" {
		t.Fatalf("local file must be preserved on conflict, got %q", got)
	}
	// 远端版本另存为 conflict 副本
	entries, _ := os.ReadDir(root)
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "a.go.conflict-remote-") {
			found = true
			b, _ := os.ReadFile(filepath.Join(root, e.Name()))
			if string(b) != "remote-version" {
				t.Fatalf("conflict copy content wrong: %q", b)
			}
		}
	}
	if !found {
		t.Fatalf("conflict must save remote copy, dir=%v", entries)
	}
	// 上传不能发生（冲突不静默覆盖）
	if len(ft.puts) != 0 {
		t.Fatalf("conflict must not upload, got %+v", ft.puts)
	}
}
