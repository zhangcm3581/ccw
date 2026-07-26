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

// 路径安全边界（审查§2.5、spec §8）：全部文件操作经fsops_{linux,other}.go的
// 平台实现进行。Linux生产用openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)+父目录fd，
// 内核原子解析、无TOCTOU窗口；非Linux用EvalSymlinks实现，仅作测试替代
// （spec §8明文允许的唯一用途）。两者的非竞态判定一致，由fsops_test.go统一断言。

// WriteTemp：随机命名+O_EXCL独占创建；LimitReader读"上限+1"字节判定超限；
// 任何失败路径都删除临时文件（审查§2.5）。
func (d *DirStore) WriteTemp(rel string, r io.Reader, maxBytes int64) (string, string, int64, error) {
	rnd := make([]byte, 8)
	if _, err := rand.Read(rnd); err != nil {
		return "", "", 0, err
	}
	tmpID := fmt.Sprintf(".cclaude.tmp.%s", hex.EncodeToString(rnd))
	f, cleanup, err := d.openTempWriter(rel, tmpID)
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxBytes+1))
	f.Close()
	if err != nil || n > maxBytes {
		cleanup(true)
		if err == nil {
			err = ErrTooLarge
		}
		return "", "", 0, err
	}
	cleanup(false)
	return tmpID, hex.EncodeToString(h.Sum(nil)), n, nil
}

// Root返回workspace根目录（供云端reconcile扫描）。
func (d *DirStore) Root() string { return d.root }

// Open打开已落盘的文件供读取（get 操作用），路径经安全校验；
// 叶子符号链接一律拒绝（否则下载会把root之外的内容带给客户端）。
func (d *DirStore) Open(rel string) (io.ReadCloser, error) {
	return d.openRead(rel)
}

func (d *DirStore) Promote(rel, tmpID string, rev int64) error {
	return d.promoteTemp(rel, tmpID)
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
	if err := d.removeFile(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
		// 只入账普通文件：符号链接会把root之外的内容（大小、哈希，进而经下载泄内容）
		// 带进清单；socket/设备文件同样无意义。与ScanDir的规则一致。
		if !e.Type().IsRegular() {
			return nil
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
