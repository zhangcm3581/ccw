package main

import "embed"

// nodefiles是推送到节点的编排文件（Caddyfile与三个Dockerfile）。
//
// **它们是deploy/下同名文件的副本**：go:embed不能引用包目录之外的路径。
// 副本由scripts/sync-nodefiles.sh同步，CI校验两边一致——
// 不一致会让Console部署出来的节点与手动部署的节点行为不同，且很难被发现。
//
//go:embed nodefiles
var nodeFilesFS embed.FS
