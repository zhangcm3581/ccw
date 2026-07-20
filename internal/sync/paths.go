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

var excludedPrefixes = []string{".env", ".cclaude/", ".ssh/", ".aws/", ".claude/",
	".config/gcloud/", ".azure/", ".kube/", ".npmrc", ".pypirc", ".netrc", ".git-credentials"}

func DefaultExcluded(p string) bool {
	for _, pre := range excludedPrefixes {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}
