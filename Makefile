# ccw构建入口。发布流水线见console-fleet-design §3.2与scripts/build-release.sh。

.PHONY: build test release

build: ## 编译全部二进制
	go build ./...

test: ## 全部单测（需要真实PG的store测试无CCW_TEST_DATABASE_URL时自动skip）
	go test ./...

# 本地开发用（需要本机装 Go）。
# **正式发布不走这里**——按 DEPLOY.md 的 B4，产物一律在 Console 主机上用
# scripts/build-release-docker.sh 构建，产物直接落到发布目录，没有传输环节。
#
# 用法：make release VERSION=v0.1.0 [DIST_DIR=dist] [TARGETS="darwin/arm64 windows/amd64"]
release:
	@test -n "$(VERSION)" || { echo "用法: make release VERSION=v0.1.0"; exit 2; }
	VERSION="$(VERSION)" DIST_DIR="$(DIST_DIR)" TARGETS="$(TARGETS)" ./scripts/build-release.sh
