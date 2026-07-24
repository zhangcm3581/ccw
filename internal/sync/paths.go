package sync

import (
	"errors"
	"path"
	"strings"
)

var ErrUnsafePath = errors.New("sync: unsafe path")

// SafeRelPath归一化为forward-slash相对路径，并拒绝任何越界形态。
// 比单纯path.Clean更严格：拒绝含任何".."段的路径（即使clean后落在根内），
// 因为正常客户端/Claude不会产生这类路径，从严可消除歧义。
func SafeRelPath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return "", ErrUnsafePath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", ErrUnsafePath
		}
	}
	clean := path.Clean(p)
	if clean == "." || strings.HasPrefix(clean, "../") {
		return "", ErrUnsafePath
	}
	return clean, nil
}

var excludedPrefixes = []string{
	// 凭据与敏感配置
	".env", ".cclaude/", ".ssh/", ".aws/", ".claude/", ".config/gcloud/",
	".azure/", ".kube/", ".npmrc", ".pypirc", ".netrc", ".git-credentials",
	// 版本控制与系统垃圾
	".git/", ".DS_Store",
	// 依赖目录（可重新安装，不必同步）
	"node_modules/", "vendor/",
	// 通用构建产物
	"target/", "build/", "dist/", "out/",
	// Java / Gradle
	".gradle/",
	// Python 缓存与虚拟环境
	"__pycache__/", ".venv/", "venv/", ".pytest_cache/", ".mypy_cache/", ".tox/",
}

func DefaultExcluded(p string) bool {
	for _, pre := range excludedPrefixes {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}
