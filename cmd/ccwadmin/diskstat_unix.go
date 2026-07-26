//go:build linux || darwin

package main

import "golang.org/x/sys/unix"

// diskStat返回path所在文件系统的总量与可用量（字节）。
// 生产上ccwadmin跑在control-api容器里，"/"即容器根文件系统——宿主机侧就是
// Docker data-root所在盘，这正是N4磁盘失控防线要盯的水位（设计§12.1）。
func diskStat(path string) (total, free uint64, err error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return 0, 0, err
	}
	// Bavail是非特权用户可用量，比Bfree更接近"还能写多少"。
	// 字段类型跨平台有差异（linux为int64、darwin为uint64），统一经uint64转换。
	return uint64(fs.Blocks) * uint64(fs.Bsize), uint64(fs.Bavail) * uint64(fs.Bsize), nil
}
