//go:build !linux

package usage

import "os"

// 非Linux平台退化为路径。仅用于本机开发与测试——生产一律是Linux容器，
// 走identity_linux.go的dev:inode实现。
func fileIdentity(path string, _ os.FileInfo) string { return path }
