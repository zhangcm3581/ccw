package sync

import (
	"fmt"
	"time"
)

type FileEntry struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Revision int64  `json:"revision"` // server_revision，只由服务端分配
	Deleted  bool   `json:"deleted"`
}

type LocalState string

const (
	StateClean    LocalState = "clean"
	StateModified LocalState = "modified"
	StateDeleted  LocalState = "deleted"
)

// LocalEntry保存三方判断的基线（审查§2.4）：只有同时知道
// "上次确认的服务端版本"和"当前本地内容"，才能区分旧副本与新修改。
type LocalEntry struct {
	Path          string     `json:"path"`
	Size          int64      `json:"size"`
	BaseRevision  int64      `json:"base_revision"`
	BaseSHA256    string     `json:"base_sha256"`
	CurrentSHA256 string     `json:"current_sha256"`
	State         LocalState `json:"state"`
}

type Conflict struct{ Path, LocalSHA, RemoteSHA string }

type Plan struct {
	Upload         []LocalEntry
	Download       []FileEntry
	Conflicts      []Conflict
	DeleteToRemote []LocalEntry
	DeleteToLocal  []FileEntry
}

// Diff三方规则（服务端CAS是最终裁决，这里只产生候选集）：
//
//	clean    + 服务端已变 → 下载（本地旧副本永远不是"赢家"）；
//	modified + 服务端未变 → CAS上传；
//	modified + 服务端已变 → 冲突副本；
//	deleted  + 服务端未变 → CAS删除；
//	deleted  + 服务端已变 → 冲突（保留服务端版本）。
func Diff(local []LocalEntry, remote []FileEntry) Plan {
	ri := make(map[string]FileEntry, len(remote))
	for _, r := range remote {
		ri[r.Path] = r
	}
	seen := make(map[string]bool, len(local))
	var p Plan
	for _, l := range local {
		seen[l.Path] = true
		r, ok := ri[l.Path]
		serverAdvanced := ok && (r.Revision != l.BaseRevision || r.Deleted)
		switch l.State {
		case StateClean:
			if serverAdvanced {
				if r.Deleted {
					p.DeleteToLocal = append(p.DeleteToLocal, r)
				} else {
					p.Download = append(p.Download, r)
				}
			}
		case StateModified:
			switch {
			case !ok || !serverAdvanced:
				p.Upload = append(p.Upload, l)
			case r.SHA256 == l.CurrentSHA256:
				// 内容碰巧一致：只需更新基线，无需传输
			default:
				p.Conflicts = append(p.Conflicts, Conflict{Path: l.Path, LocalSHA: l.CurrentSHA256, RemoteSHA: r.SHA256})
			}
		case StateDeleted:
			if !ok || !serverAdvanced {
				p.DeleteToRemote = append(p.DeleteToRemote, l)
			} else if !r.Deleted {
				p.Conflicts = append(p.Conflicts, Conflict{Path: l.Path, LocalSHA: "", RemoteSHA: r.SHA256})
			}
		}
	}
	for path, r := range ri {
		if !seen[path] && !r.Deleted {
			p.Download = append(p.Download, r)
		}
	}
	return p
}

// BuildLocal由当前目录扫描结果与上次保存的基线推导每条路径的State。
func BuildLocal(scanned []FileEntry, base []LocalEntry) []LocalEntry {
	bi := make(map[string]LocalEntry, len(base))
	for _, b := range base {
		bi[b.Path] = b
	}
	seen := make(map[string]bool, len(scanned))
	var out []LocalEntry
	for _, s := range scanned {
		seen[s.Path] = true
		b, ok := bi[s.Path]
		e := LocalEntry{Path: s.Path, Size: s.Size, CurrentSHA256: s.SHA256}
		if ok {
			e.BaseRevision, e.BaseSHA256 = b.BaseRevision, b.BaseSHA256
		}
		if ok && s.SHA256 == b.BaseSHA256 {
			e.State = StateClean
		} else {
			e.State = StateModified // 新文件的BaseRevision=0也走CAS上传
		}
		out = append(out, e)
	}
	for _, b := range base {
		if !seen[b.Path] && b.State != StateDeleted {
			out = append(out, LocalEntry{Path: b.Path, BaseRevision: b.BaseRevision,
				BaseSHA256: b.BaseSHA256, State: StateDeleted})
		}
	}
	return out
}

func ConflictName(path, device string, at time.Time) string {
	return fmt.Sprintf("%s.conflict-%s-%s", path, device, at.UTC().Format("20060102T150405Z"))
}
