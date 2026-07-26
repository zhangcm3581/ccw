//go:build !linux

package sync

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 非Linux平台的文件操作实现：MkdirAll+EvalSymlinks做逃逸检查。
//
// spec §8明文规定：这**只允许作为非Linux平台的测试替代**，不得作为生产安全边界——
// 检查与使用之间存在TOCTOU窗口（能正确拒绝非竞态逃逸，竞态下可被绕过）。
// Linux生产走fsops_linux.go的openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)实现，
// 两者对非竞态用例的判定保持一致（fsops_test.go在全部平台跑同一套断言）。

// resolve把相对路径映射到root下，并拒绝经符号链接父目录的越界。
func (d *DirStore) resolve(rel string) (string, error) {
	rel, err := SafeRelPath(rel)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(d.root, filepath.FromSlash(rel))
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(d.root)
	if err != nil {
		return "", err
	}
	if realParent != realRoot && !strings.HasPrefix(realParent+string(os.PathSeparator), realRoot+string(os.PathSeparator)) {
		return "", ErrUnsafePath
	}
	return filepath.Join(realParent, filepath.Base(abs)), nil
}

func (d *DirStore) openTempWriter(rel, tmpID string) (*os.File, func(removeTmp bool), error) {
	abs, err := d.resolve(rel)
	if err != nil {
		return nil, nil, err
	}
	tmpPath := filepath.Join(filepath.Dir(abs), tmpID)
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return f, func(remove bool) {
		if remove {
			os.Remove(tmpPath)
		}
	}, nil
}

func (d *DirStore) promoteTemp(rel, tmpID string) error {
	abs, err := d.resolve(rel)
	if err != nil {
		return err
	}
	return os.Rename(filepath.Join(filepath.Dir(abs), tmpID), abs)
}

func (d *DirStore) openRead(rel string) (io.ReadCloser, error) {
	abs, err := d.resolve(rel)
	if err != nil {
		return nil, err
	}
	// 叶子必须是普通文件：文件符号链接会把root之外的内容带给下载方。
	// （Lstat与Open之间仍有窗口——测试替代实现的已知限制，见文件头。）
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrUnsafePath
	}
	return os.Open(abs)
}

func (d *DirStore) removeFile(rel string) error {
	abs, err := d.resolve(rel)
	if err != nil {
		return err
	}
	return os.Remove(abs)
}
