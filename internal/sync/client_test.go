package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanDirMatchesManifestRules(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "main.go"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(root, ".cclaude"), 0o755)
	os.WriteFile(filepath.Join(root, ".cclaude", "index.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o644)
	entries, err := ScanDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "main.go" {
		t.Fatalf(".cclaude and .env must be excluded: %+v", entries)
	}
}

func TestLocalIndexRoundTrip(t *testing.T) {
	root := t.TempDir()
	idx := LocalIndex{Root: root}
	in := []LocalEntry{{Path: "a.go", Size: 3, BaseRevision: 2, BaseSHA256: "abc", CurrentSHA256: "abc", State: StateClean}}
	if err := idx.Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := idx.Load()
	if err != nil || len(out) != 1 || out[0] != in[0] {
		t.Fatalf("round trip failed: %+v %v", out, err)
	}
}

func TestUnchangedFileNotReuploaded(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("abc"), 0o644)
	scanned, _ := ScanDir(root)
	sha := scanned[0].SHA256
	// 基线=服务端rev3同内容 → BuildLocal判定clean → Diff零传输
	base := []LocalEntry{{Path: "a.go", BaseRevision: 3, BaseSHA256: sha, State: StateClean}}
	local := BuildLocal(scanned, base)
	remote := []FileEntry{{Path: "a.go", SHA256: sha, Revision: 3, Size: 3}}
	p := Diff(local, remote)
	if len(p.Upload)+len(p.Download)+len(p.Conflicts) != 0 {
		t.Fatalf("unchanged files must not transfer: %+v", p)
	}
}
