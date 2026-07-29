package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SyncTransport抽象一次同步会话的消息往来，便于用假传输单测执行逻辑。
// reject为空表示成功；非空是服务端拒绝原因（conflict|disk_full|...）。
type SyncTransport interface {
	Hello(device, ws string) (mode string, err error)
	Manifest() ([]FileEntry, error)
	Put(entry LocalEntry, content io.Reader) (newRev int64, reject string, err error)
	Get(path string) (entry FileEntry, content io.ReadCloser, err error)
	Delete(entry LocalEntry) (newRev int64, reject string, err error)
}

// SyncClient执行一次三方同步：拉服务端清单→与本地基线Diff→按Plan上传/下载/删除/存冲突副本→更新基线。
type SyncClient struct {
	Root   string
	Device string
	// WS是工作区键，由Root算出（见WorkspaceKey）。云端按它把不同本地目录
	// 分开存放——空值会被服务端拒绝，不再退回到"全项目一个平铺目录"。
	WS     string
	Notify func(string) // 提示用户（冲突等）；可为 nil
}

func (c *SyncClient) note(msg string) {
	if c.Notify != nil {
		c.Notify(msg)
	}
}

// SyncOnce用给定传输执行一轮同步，返回更新后的本地基线（调用方负责持久化到LocalIndex）。
func (c *SyncClient) SyncOnce(ctx context.Context, t SyncTransport) ([]LocalEntry, error) {
	mode, err := t.Hello(c.Device, c.WS)
	if err != nil {
		return nil, err
	}
	remote, err := t.Manifest()
	if err != nil {
		return nil, err
	}
	scanned, err := ScanDir(c.Root)
	if err != nil {
		return nil, err
	}
	base, err := (LocalIndex{Root: c.Root}).Load()
	if err != nil {
		return nil, err
	}
	local := BuildLocal(scanned, base)
	plan := Diff(local, remote)
	return c.execute(ctx, t, plan, mode, local, remote), nil
}

// execute按Plan逐项执行，返回执行后的新基线（path→已确认的revision/sha）。
func (c *SyncClient) execute(ctx context.Context, t SyncTransport, plan Plan, mode string,
	local []LocalEntry, remote []FileEntry) []LocalEntry {

	base := map[string]LocalEntry{}
	for _, l := range local {
		if l.State == StateClean {
			base[l.Path] = clean(l.Path, l.BaseRevision, l.BaseSHA256)
		}
	}
	remoteByPath := make(map[string]FileEntry, len(remote))
	for _, r := range remote {
		remoteByPath[r.Path] = r
	}

	if mode != "cleanup" { // cleanup 模式不上传
		for _, u := range plan.Upload {
			f, err := os.Open(filepath.Join(c.Root, filepath.FromSlash(u.Path)))
			if err != nil {
				continue
			}
			newRev, reject, err := t.Put(u, f)
			f.Close()
			if err != nil {
				continue
			}
			switch reject {
			case "":
				base[u.Path] = clean(u.Path, newRev, u.CurrentSHA256)
			case "conflict":
				c.saveConflict(t, u.Path, remoteByPath[u.Path], base)
			default:
				c.note(fmt.Sprintf("上传 %s 被拒绝：%s", u.Path, reject))
			}
		}
	}

	for _, d := range plan.Download {
		c.download(t, d, base)
	}

	for _, d := range plan.DeleteToRemote {
		_, reject, err := t.Delete(d)
		if err == nil && reject == "" {
			delete(base, d.Path)
		}
	}

	for _, d := range plan.DeleteToLocal {
		os.Remove(filepath.Join(c.Root, filepath.FromSlash(d.Path)))
		delete(base, d.Path)
	}

	for _, cf := range plan.Conflicts {
		c.saveConflict(t, cf.Path, remoteByPath[cf.Path], base)
	}

	out := make([]LocalEntry, 0, len(base))
	for _, e := range base {
		out = append(out, e)
	}
	return out
}

// download获取远端版本写入本地并更新基线。
func (c *SyncClient) download(t SyncTransport, d FileEntry, base map[string]LocalEntry) {
	entry, rc, err := t.Get(d.Path)
	if err != nil {
		return
	}
	defer rc.Close()
	if err := writeLocal(c.Root, d.Path, rc); err != nil {
		return
	}
	base[d.Path] = clean(d.Path, entry.Revision, entry.SHA256)
}

// saveConflict：本地保留用户修改，远端版本另存为 <name>.conflict-remote-<UTC>，
// 基线更新为远端版本（下次本地修改会以远端为基线重新上传，远端旧版已在冲突副本里）。
func (c *SyncClient) saveConflict(t SyncTransport, path string, remote FileEntry, base map[string]LocalEntry) {
	entry, rc, err := t.Get(path)
	if err != nil {
		return
	}
	defer rc.Close()
	cname := ConflictName(path, "remote", time.Now())
	if err := writeLocal(c.Root, cname, rc); err != nil {
		return
	}
	c.note(fmt.Sprintf("冲突：%s 的远端版本已另存为 %s，请手动合并", path, cname))
	base[path] = clean(path, entry.Revision, entry.SHA256)
}

func clean(path string, rev int64, sha string) LocalEntry {
	return LocalEntry{Path: path, BaseRevision: rev, BaseSHA256: sha, CurrentSHA256: sha, State: StateClean}
}

// writeLocal把内容写入 root/rel（先写临时文件再原子重命名）。
func writeLocal(root, rel string, r io.Reader) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	tmp := abs + ".cclaude.dl.tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, abs)
}
