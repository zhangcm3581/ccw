//go:build linux

package usage

import (
	"os"
	"path/filepath"
	"testing"
)

// 同路径删除重建后identity必须变化——否则新文件会沿用旧游标，
// 开头到旧偏移之间的事件被整段跳过，用量偏低且无任何迹象。
// 本用例只在Linux跑（生产形态），本机macOS开发时会被构建标签排除。
func TestFileIdentityChangesOnRecreate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "session.jsonl")

	if err := os.WriteFile(p, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi1, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	id1 := fileIdentity(p, fi1)

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("bb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	id2 := fileIdentity(p, fi2)

	if id1 == id2 {
		t.Errorf("重建后identity不应相同：%q == %q（说明退化成了路径）", id1, id2)
	}
	if id1 == p || id2 == p {
		t.Errorf("Linux下identity应为dev:inode而不是路径：%q / %q", id1, id2)
	}
}

// 同一个文件多次Stat必须得到稳定的identity，否则每轮采集都会当成新文件重扫。
func TestFileIdentityStableForSameFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(p, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi1, _ := os.Stat(p)
	// 追加内容不改变inode。
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("b\n")
	f.Close()
	fi2, _ := os.Stat(p)

	if fileIdentity(p, fi1) != fileIdentity(p, fi2) {
		t.Error("同一文件追加后identity不应变化")
	}
}
