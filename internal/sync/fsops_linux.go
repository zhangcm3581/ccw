//go:build linux

package sync

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux生产实现（spec §8的"必须"、解P1-2/D4）：
// openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)在内核里**原子**解析root之下的整段
// 相对路径——任何分量是符号链接、或解析越出root，一次系统调用内直接失败，
// 不存在"先检查后使用"的TOCTOU窗口。最终分量的创建/改名/删除全部经父目录fd
// 进行（openat2/renameat/unlinkat）：持有fd期间祖先目录被替换也不影响已固定的解析。
//
// 要求内核≥5.6（openat2引入版本）；发行版白名单Ubuntu 22.04/24.04、Debian 12
// （设计§9.2）全部满足。内核不支持时**硬失败**（ENOSYS原样上抛）而非退回
// EvalSymlinks——安全边界不允许静默降级。

const resolveBeneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS

// openat2Retry处理EINTR与EAGAIN——openat2在检测到并发rename时可能返回EAGAIN，
// man页建议重试。重试有界：真被rename风暴攻击时报错，不空转。
func openat2Retry(dirfd int, p string, how *unix.OpenHow) (int, error) {
	for i := 0; i < 32; i++ {
		fd, err := unix.Openat2(dirfd, p, how)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		return fd, err
	}
	return -1, fmt.Errorf("sync: openat2重试耗尽（疑似并发rename干扰）: %w", unix.EAGAIN)
}

// mapPathErr把内核的越界判定翻译成ErrUnsafePath（EXDEV=RESOLVE_BENEATH判定越出root、
// ELOOP=RESOLVE_NO_SYMLINKS遇到符号链接）；其余errno包成PathError，
// 保留errors.Is(err, fs.ErrNotExist)等标准判定。
func mapPathErr(op, p string, err error) error {
	if err == unix.EXDEV || err == unix.ELOOP {
		return fmt.Errorf("%w（%s %q被内核拒绝：越界或符号链接）", ErrUnsafePath, op, p)
	}
	if errno, ok := err.(unix.Errno); ok {
		return &os.PathError{Op: op, Path: p, Err: errno}
	}
	return err
}

// openRootFD打开root目录fd。root来自部署配置（WorkspaceRoot+slug），是受信路径；
// 不受信的只有rel（来自客户端），全部经openat2相对解析。
func (d *DirStore) openRootFD(create bool) (int, error) {
	if create {
		if err := os.MkdirAll(d.root, 0o755); err != nil {
			return -1, err
		}
	}
	fd, err := unix.Open(d.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, mapPathErr("open-root", d.root, err)
	}
	return fd, nil
}

// openParentFD返回rel父目录的fd与最终分量名；create时逐分量mkdirat补建，
// 全程fd相对、不重解析已走过的前缀。调用方负责unix.Close返回的fd。
func (d *DirStore) openParentFD(rel string, create bool) (int, string, error) {
	rel, err := SafeRelPath(rel)
	if err != nil {
		return -1, "", err
	}
	dir, base := path.Dir(rel), path.Base(rel)
	rootfd, err := d.openRootFD(create)
	if err != nil {
		return -1, "", err
	}
	if dir == "." {
		return rootfd, base, nil
	}
	how := &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: resolveBeneath}
	// 快路径：整段父路径一次openat2（内核原子解析全部中间分量）。
	fd, ferr := openat2Retry(rootfd, dir, how)
	if ferr == nil {
		unix.Close(rootfd)
		return fd, base, nil
	}
	if !create || !errors.Is(ferr, unix.ENOENT) {
		unix.Close(rootfd)
		return -1, "", mapPathErr("open-parent", dir, ferr)
	}
	// 慢路径：父目录尚不存在，逐分量mkdirat+openat2补建。
	cur := rootfd
	for _, comp := range strings.Split(dir, "/") {
		fd, err := openat2Retry(cur, comp, how)
		if errors.Is(err, unix.ENOENT) {
			unix.Mkdirat(cur, comp, 0o755) // EEXIST竞态无害：随后的openat2才是裁决
			fd, err = openat2Retry(cur, comp, how)
		}
		unix.Close(cur)
		if err != nil {
			return -1, "", mapPathErr("open-parent", comp, err)
		}
		cur = fd
	}
	return cur, base, nil
}

func (d *DirStore) openTempWriter(rel, tmpID string) (*os.File, func(removeTmp bool), error) {
	dirfd, _, err := d.openParentFD(rel, true)
	if err != nil {
		return nil, nil, err
	}
	how := &unix.OpenHow{
		Flags:   unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Mode:    0o600,
		Resolve: resolveBeneath,
	}
	fd, err := openat2Retry(dirfd, tmpID, how)
	if err != nil {
		unix.Close(dirfd)
		return nil, nil, mapPathErr("create-temp", tmpID, err)
	}
	f := os.NewFile(uintptr(fd), tmpID)
	cleanup := func(remove bool) {
		if remove {
			unix.Unlinkat(dirfd, tmpID, 0)
		}
		unix.Close(dirfd)
	}
	return f, cleanup, nil
}

func (d *DirStore) promoteTemp(rel, tmpID string) error {
	dirfd, base, err := d.openParentFD(rel, true)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	// renameat不解引用两端的尾部符号链接；父目录已由openat2锁定在root之内。
	if err := unix.Renameat(dirfd, tmpID, dirfd, base); err != nil {
		return mapPathErr("promote", rel, err)
	}
	return nil
}

func (d *DirStore) openRead(rel string) (io.ReadCloser, error) {
	rel, err := SafeRelPath(rel)
	if err != nil {
		return nil, err
	}
	rootfd, err := d.openRootFD(false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootfd)
	how := &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: resolveBeneath}
	fd, err := openat2Retry(rootfd, rel, how)
	if err != nil {
		return nil, mapPathErr("open", rel, err)
	}
	return os.NewFile(uintptr(fd), rel), nil
}

func (d *DirStore) removeFile(rel string) error {
	dirfd, base, err := d.openParentFD(rel, false)
	if err != nil {
		return err // 父目录不存在→PathError(ENOENT)→Delete按"已不存在"容忍
	}
	defer unix.Close(dirfd)
	if err := unix.Unlinkat(dirfd, base, 0); err != nil {
		return mapPathErr("delete", rel, err)
	}
	return nil
}
