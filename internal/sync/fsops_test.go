package sync

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 路径解析安全（spec §8、P1-2）：这些测试在全部平台跑同一套断言——
// Linux下走openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)实现，
// 非Linux走EvalSymlinks测试替代实现；两者对非竞态逃逸的判定必须一致。
// Linux实现的真实验证以CI（ubuntu）与容器内运行为准。

// escapeRoot构造 root 与 root外的目标目录，在root内放一个指向外部的目录符号链接
// 与一个指向外部文件的文件符号链接。
func escapeRoot(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "workspace")
	outside = filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Skipf("平台不支持symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "filelink.txt")); err != nil {
		t.Skip("平台不支持symlink")
	}
	return root, outside
}

// 经目录符号链接写入必须被拒绝：否则上传会落到root之外。
func TestWriteTempRejectsSymlinkParent(t *testing.T) {
	root, outside := escapeRoot(t)
	d := NewDirStore(root)
	_, _, _, err := d.WriteTemp("dirlink/evil.txt", strings.NewReader("x"), 1<<20)
	if err == nil {
		t.Fatal("经目录符号链接的WriteTemp必须失败")
	}
	entries, _ := os.ReadDir(outside)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cclaude.tmp.") {
			t.Errorf("临时文件泄漏到root之外: %s", e.Name())
		}
	}
}

// 读取叶子文件符号链接必须被拒绝：否则下载会把root之外的内容带给客户端。
func TestOpenRejectsLeafSymlink(t *testing.T) {
	root, _ := escapeRoot(t)
	d := NewDirStore(root)
	rc, err := d.Open("filelink.txt")
	if err == nil {
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		t.Fatalf("经文件符号链接的Open必须失败，实际读到%d字节（%q）", len(b), string(b))
	}
}

// 经目录符号链接读取同样拒绝。
func TestOpenRejectsSymlinkParent(t *testing.T) {
	root, _ := escapeRoot(t)
	d := NewDirStore(root)
	if rc, err := d.Open("dirlink/secret.txt"); err == nil {
		rc.Close()
		t.Fatal("经目录符号链接的Open必须失败")
	}
}

// 经目录符号链接删除必须被拒绝：否则可删掉root之外的文件。
func TestDeleteRejectsSymlinkParent(t *testing.T) {
	root, outside := escapeRoot(t)
	d := NewDirStore(root)
	if err := d.Delete("dirlink/secret.txt", 1); err == nil {
		if _, statErr := os.Stat(filepath.Join(outside, "secret.txt")); os.IsNotExist(statErr) {
			t.Fatal("root之外的文件被删除——路径逃逸")
		}
		t.Fatal("经目录符号链接的Delete必须失败")
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatalf("外部文件不应被动过: %v", err)
	}
}

// Promote的目标父目录是符号链接时拒绝。
func TestPromoteRejectsSymlinkParent(t *testing.T) {
	root, _ := escapeRoot(t)
	d := NewDirStore(root)
	if err := d.Promote("dirlink/out.txt", ".cclaude.tmp.deadbeef", 1); err == nil {
		t.Fatal("经目录符号链接的Promote必须失败")
	}
}

// 正常路径不受影响：写入-提升-读取-删除全链路可用（含自动建父目录）。
func TestNormalRoundTripStillWorks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	d := NewDirStore(root)
	tmpID, sha, n, err := d.WriteTemp("a/b/hello.txt", strings.NewReader("hello"), 1<<20)
	if err != nil {
		t.Fatalf("WriteTemp: %v", err)
	}
	if n != 5 || sha == "" {
		t.Fatalf("size=%d sha=%q", n, sha)
	}
	if err := d.Promote("a/b/hello.txt", tmpID, 1); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	rc, err := d.Open("a/b/hello.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(b, []byte("hello")) {
		t.Fatalf("内容不一致: %q", b)
	}
	if err := d.Delete("a/b/hello.txt", 2); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 删除不存在的文件容忍（幂等）。
	if err := d.Delete("a/b/hello.txt", 3); err != nil {
		t.Fatalf("重复Delete应容忍: %v", err)
	}
}

// 越界相对路径仍然在入口被拒（SafeRelPath是第一道闸，openat2是第二道）。
func TestOpsRejectUnsafeRelPaths(t *testing.T) {
	d := NewDirStore(t.TempDir())
	for _, p := range []string{"../x", "/abs", "a/../../x", "a\x00b"} {
		if _, _, _, err := d.WriteTemp(p, strings.NewReader("x"), 10); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("WriteTemp(%q)应返回ErrUnsafePath，got %v", p, err)
		}
		if _, err := d.Open(p); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Open(%q)应返回ErrUnsafePath，got %v", p, err)
		}
		if err := d.Delete(p, 1); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Delete(%q)应返回ErrUnsafePath，got %v", p, err)
		}
	}
}

// Manifest与ScanDir只入账普通文件：符号链接一律跳过——
// 否则云端清单会把root之外的内容（大小与哈希，进而经下载泄内容）带出去。
func TestManifestSkipsSymlinks(t *testing.T) {
	root, _ := escapeRoot(t)
	d := NewDirStore(root)
	if err := os.WriteFile(filepath.Join(root, "normal.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := d.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	if len(entries) != 1 || entries[0].Path != "normal.txt" {
		t.Errorf("Manifest应只含normal.txt，got %v", paths)
	}
}

func TestScanDirSkipsSymlinks(t *testing.T) {
	root, _ := escapeRoot(t)
	if err := os.WriteFile(filepath.Join(root, "normal.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := ScanDir(root)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "normal.txt" {
		var paths []string
		for _, e := range entries {
			paths = append(paths, e.Path)
		}
		t.Errorf("ScanDir应只含normal.txt，got %v", paths)
	}
}
