//go:build linux

package sync

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// 同步落盘的文件与目录必须归容器里的 claude(1001)。
//
// 2026-07-30 真机：worker-agent 以 root 写盘，容器以 1001 运行，同步上去的
// 文件是 root:root 0600——Claude 读不了也改不了，`cat` 直接 Permission denied。
//
// **需要 root**（要 chown 到别的 uid），非 root 自动 skip；CI 的容器里是 root。
func TestSyncedFilesOwnedByContainerUID(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("需要 root 才能 chown 到别的 uid（容器里跑：docker run -v … golang）")
	}
	root := t.TempDir()
	d := NewOwnedDirStore(root, ContainerUID, ContainerGID)

	// 带子目录，验证目录也被 chown——目录归 root 0755 的话容器能进能读，
	// 但没法在里面建文件。
	tmpID, _, _, err := d.WriteTemp("sub/deep/111.txt", strings.NewReader("hi\n"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Promote("sub/deep/111.txt", tmpID, 1); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"sub", "sub/deep", "sub/deep/111.txt"} {
		fi, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if int(st.Uid) != ContainerUID || int(st.Gid) != ContainerGID {
			t.Errorf("%s 属主 %d:%d，应为 %d:%d", rel, st.Uid, st.Gid, ContainerUID, ContainerGID)
		}
	}
}

// 修复历史遗留：chown 上线前落盘的 root:root 文件必须被改回来，
// 否则用户既读不了也删不了（父目录也归 root），永久卡住。
func TestRepairOwnerFixesLegacyRootFiles(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("需要 root")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "old", "legacy.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 此刻是 root:root（测试以 root 跑）
	repairOwnerOnce(root, ContainerUID, ContainerGID)

	st := mustStat(t, f)
	if int(st.Uid) != ContainerUID {
		t.Errorf("遗留文件未被修复，属主仍是 %d", st.Uid)
	}
	if st2 := mustStat(t, filepath.Join(root, "old")); int(st2.Uid) != ContainerUID {
		t.Errorf("遗留目录未被修复，属主仍是 %d", st2.Uid)
	}
}

// 每个目录只修一次：客户端每2秒重连，每次遍历整棵树的开销不能接受。
func TestRepairOwnerRunsOncePerDir(t *testing.T) {
	root := t.TempDir()
	repairOwnerOnce(root, ContainerUID, ContainerGID)
	if _, ok := ownerRepaired.Load(root); !ok {
		t.Fatal("应记下已修过")
	}
	// 第二次进来必须直接返回（用一个只有第一次才可能被改到的文件间接验证）
	f := filepath.Join(root, "after.txt")
	if err := os.WriteFile(f, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	repairOwnerOnce(root, ContainerUID, ContainerGID)
	if os.Getuid() == 0 {
		if int(mustStat(t, f).Uid) == ContainerUID {
			t.Error("第二次调用不该再遍历（这个文件本不该被改）")
		}
	}
}

func mustStat(t *testing.T, p string) *syscall.Stat_t {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t)
}
