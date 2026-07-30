package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	stdsync "sync"
)

// 修复历史遗留的属主（2026-07-30）。
//
// 在 chown 上线之前同步上去的文件是 root:root，容器里的 claude(1001) 读不了、
// 也改不了。只修新写入的话，那些文件会永久卡住，而用户唯一的出路是删掉重传
// ——他自己都删不掉（父目录也归 root）。
//
// **每个进程每个目录只走一次**：客户端每 2 秒重连一次，每次都遍历一遍
// 整棵树是不能接受的开销。worker-agent 重启（每次部署都会）时再修一次，
// 而那时该修的早就修完了，遍历的是一棵已经正确的树。
var ownerRepaired stdsync.Map

// repairOwnerOnce把dir下属主不对的条目改回uid:gid。
//
// 失败**不阻断同步**：修不了（比如没有 CAP_CHOWN）时也要让同步继续跑，
// 只是容器里读不了——那是原来就有的状态，不该因为修不成而连同步一起断掉。
func repairOwnerOnce(dir string, uid, gid int) {
	if _, done := ownerRepaired.LoadOrStore(dir, true); done {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个条目坏了不影响其余
		}
		info, ierr := e.Info()
		if ierr != nil {
			return nil
		}
		if ownedBy(info, uid) {
			return nil
		}
		_ = os.Lchown(p, uid, gid)
		return nil
	})
}
