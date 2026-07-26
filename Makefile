# ccw构建入口。发布流水线见console-fleet-design §3.2与scripts/build-release.sh。

.PHONY: build test release

build: ## 编译全部二进制
	go build ./...

test: ## 全部单测（需要真实PG的store测试无CCW_TEST_DATABASE_URL时自动skip）
	go test ./...

# 用法：make release VERSION=v0.1.0 [DIST_DIR=dist]
# 交叉编译六目标客户端并生成SHA256SUMS；随后在Console主机上登记：
#   ccw-console register-release --version $(VERSION) --publish
release:
	@test -n "$(VERSION)" || { echo "用法: make release VERSION=v0.1.0"; exit 2; }
	VERSION=$(VERSION) DIST_DIR=$(DIST_DIR) ./scripts/build-release.sh
