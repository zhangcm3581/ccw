package main

import "runtime"

// isCaseInsensitiveFS报告默认文件系统是否大小写不敏感。
//
// 按平台判断而不是实际探测：探测要在目标目录里建临时文件，而这个函数会在
// 比较路径时被频繁调用，副作用不成比例。macOS 的 APFS 可以格式化成大小写敏感，
// 那种配置下把 C:\Code 与 C:\code 当成同一个只会多合并一条登记记录——
// 后果远小于反过来（同一个目录被登记两次、显示成两个项目）。
func isCaseInsensitiveFS() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}
