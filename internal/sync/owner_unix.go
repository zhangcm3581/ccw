//go:build unix

package sync

import (
	"io/fs"
	"syscall"
)

// ownedBy报告info的属主是否已经是uid。取不到底层stat时**当作已正确**，
// 避免在不支持的平台上做无意义的chown。
func ownedBy(info fs.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return int(st.Uid) == uid
}
