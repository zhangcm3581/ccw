package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirStoreAtomicWriteAndManifest(t *testing.T) {
	root := t.TempDir()
	s := NewDirStore(root)
	tmpID, sha, size, err := s.WriteTemp("src/main.go", strings.NewReader("package main\n"), 1<<20)
	if err != nil || size != 13 {
		t.Fatalf("write: sha=%s size=%d err=%v", sha, size, err)
	}
	// promote前目录里只有tmp文件，不可见于清单
	m, _ := s.Manifest()
	if len(m) != 0 {
		t.Fatalf("tmp file must not appear in manifest: %+v", m)
	}
	if err := s.Promote("src/main.go", tmpID, 1); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Manifest()
	if len(m) != 1 || m[0].Path != "src/main.go" || m[0].SHA256 != sha {
		t.Fatalf("manifest wrong: %+v", m)
	}
}

func TestDirStoreTooLarge(t *testing.T) {
	// 真实字节上限用"上限+1"判定，超限必须失败且tmp被清理
	root := t.TempDir()
	s := NewDirStore(root)
	if _, _, _, err := s.WriteTemp("big.bin", strings.NewReader("0123456789"), 9); err != ErrTooLarge {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("failed write must leave no tmp files: %v", entries)
	}
}

func TestDirStoreRejectsEscape(t *testing.T) {
	root := t.TempDir()
	s := NewDirStore(root)
	if _, _, _, err := s.WriteTemp("../evil", strings.NewReader("x"), 8); err == nil {
		t.Fatal("path escape must be rejected")
	}
	// 符号链接逃逸：workspace内建一个指向外部的链接目录
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(root, "link"))
	if _, _, _, err := s.WriteTemp("link/evil.txt", strings.NewReader("x"), 8); err == nil {
		t.Fatal("symlink escape must be rejected")
	}
}

func TestDirStoreDeleteTombstone(t *testing.T) {
	root := t.TempDir()
	s := NewDirStore(root)
	tmpID, _, _, _ := s.WriteTemp("a.txt", strings.NewReader("hello"), 8)
	s.Promote("a.txt", tmpID, 1)
	if err := s.Delete("a.txt", 2); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Manifest()
	if len(m) != 0 {
		t.Fatalf("deleted file must leave manifest: %+v", m)
	}
}
