//go:build !linux && !darwin

package main

import "errors"

// 非unix平台没有statfs：status会省略disk段（生产节点只有Linux，设计§9.2）。
func diskStat(path string) (total, free uint64, err error) {
	return 0, 0, errors.New("diskstat: unsupported platform")
}
