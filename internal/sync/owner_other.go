//go:build !unix

package sync

import "io/fs"

// 非unix平台没有属主概念，一律当作已正确。
func ownedBy(fs.FileInfo, int) bool { return true }
