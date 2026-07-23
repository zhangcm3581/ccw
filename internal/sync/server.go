package sync

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrTooLarge = errors.New("sync: content exceeds size limit")

type Store interface {
	WriteTemp(path string, r io.Reader, maxBytes int64) (tmpID, sha string, size int64, err error)
	Promote(path, tmpID string, revision int64) error
	Discard(tmpID string)
	Delete(path string, revision int64) error
	Manifest() ([]FileEntry, error)
}

type DirStore struct{ root string }

func NewDirStore(root string) *DirStore { return &DirStore{root: root} }

// resolve把相对路径映射到root下。
//
// 安全边界（审查§2.5）：本函数的EvalSymlinks实现存在检查与写入之间的
// TOCTOU窗口，只适合本机/非Linux测试。Linux生产构建前必须替换为openat2
// （RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS）逐级打开父目录fd的版本——见Task 12
// 集成阶段的硬化步骤。当前版本能正确拒绝非竞态的符号链接逃逸。
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

// WriteTemp：随机命名+O_EXCL独占创建；LimitReader读"上限+1"字节判定超限；
// 任何失败路径都删除临时文件（审查§2.5）。
func (d *DirStore) WriteTemp(rel string, r io.Reader, maxBytes int64) (string, string, int64, error) {
	abs, err := d.resolve(rel)
	if err != nil {
		return "", "", 0, err
	}
	rnd := make([]byte, 8)
	if _, err := rand.Read(rnd); err != nil {
		return "", "", 0, err
	}
	tmpID := fmt.Sprintf(".cclaude.tmp.%s", hex.EncodeToString(rnd))
	tmpPath := filepath.Join(filepath.Dir(abs), tmpID)
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxBytes+1))
	f.Close()
	if err != nil || n > maxBytes {
		os.Remove(tmpPath)
		if err == nil {
			err = ErrTooLarge
		}
		return "", "", 0, err
	}
	return tmpID, hex.EncodeToString(h.Sum(nil)), n, nil
}

// Root返回workspace根目录（供云端reconcile扫描）。
func (d *DirStore) Root() string { return d.root }

// Open打开已落盘的文件供读取（get 操作用），路径经安全校验。
func (d *DirStore) Open(rel string) (io.ReadCloser, error) {
	abs, err := d.resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.Open(abs)
}

func (d *DirStore) Promote(rel, tmpID string, rev int64) error {
	abs, err := d.resolve(rel)
	if err != nil {
		return err
	}
	return os.Rename(filepath.Join(filepath.Dir(abs), tmpID), abs)
}

func (d *DirStore) Discard(tmpID string) {
	// tmpID不含路径分隔符，只可能位于root子树内；遍历删除同名tmp
	filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && filepath.Base(p) == tmpID {
			os.Remove(p)
		}
		return nil
	})
}

func (d *DirStore) Delete(rel string, rev int64) error {
	abs, err := d.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *DirStore) Manifest() ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, d.root+string(os.PathSeparator)))
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".cclaude.tmp.") || DefaultExcluded(rel) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out = append(out, FileEntry{Path: rel, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	return out, err
}
