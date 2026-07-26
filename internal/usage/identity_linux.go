//go:build linux

package usage

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity取dev:inode——生产（Linux容器）的实现。
//
// 为什么不能用路径：Claude的会话JSONL按session id命名，同一路径原则上不重复出现，
// 但容器重建、卷恢复、或客户手动删文件后新会话复用同名路径都会导致"同路径不同文件"。
// 此时若沿用旧游标，新文件开头到旧偏移之间的事件会被整段跳过——用量偏低且无任何迹象。
// （反过来，新文件比旧偏移短时Scan有截断检测会从头重扫，那个方向本来就是安全的。）
//
// Stat_t取不到时退回路径：宁可退化成旧行为，也不要让整轮采集失败。
func fileIdentity(path string, fi os.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
	}
	return path
}
