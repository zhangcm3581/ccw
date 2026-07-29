package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
)

// ErrDiskFull：配额回调可返回它表示超额（SyncSession只判 err!=nil，不依赖具体类型）。
var ErrDiskFull = errors.New("sync: disk quota exceeded")

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// RevisionStore：服务端file_index的权威状态。生产用PG，测试用内存。
type RevisionStore interface {
	Current(ctx context.Context, projectID, path string) (rev int64, size int64, exists bool, err error)
	Manifest(ctx context.Context, projectID string) ([]FileEntry, error)
	Commit(ctx context.Context, projectID string, e FileEntry, device string) error
	TotalSize(ctx context.Context, projectID string) (int64, error)
}

type PutResult struct {
	OK       bool
	Revision int64
	Reason   string // conflict|disk_full|sha_mismatch|unsafe_path|readonly_mode|too_large|internal
}

// SyncSession承载一条同步连接的服务端逻辑，与WebSocket传输层解耦以便单元测试。
// AllowQuota由worker注入（storage.Gate.Allow），返回非nil即视为超额。
type SyncSession struct {
	ProjectID string
	Device    string
	// WS是工作区键（见workspace.go）。**索引路径与磁盘目录都按它分层**，
	// 这是"不同本地目录互不污染"的实现点。空值只在单元测试里出现，
	// 生产路径由ws.go的hello强制要求。
	WS    string
	Mode  string // "rw" | "cleanup"
	Store RevisionStore
	// Root是本项目的workspace根；SetWorkspace会在它下面按工作区建子目录。
	// 直接给Dir而不给Root的用法只保留给单元测试。
	Root       string
	Dir        *DirStore
	MaxBytes   int64
	AllowQuota func(used, oldSize, newSize int64) error
	// Lock：项目级锁，串行化同一项目的写，防并发上传各读旧revision/用量后同时通过（审查§15.2）。
	// 可为 nil（单元测试）；worker 为每个 project 注入同一把锁。
	Lock *stdsync.Mutex
}

// key把工作区内的相对路径转成索引里的全局路径。
// file_index的主键是(project_id, path)，用前缀分层就不必改表结构——
// 不同工作区的同名文件天然是两行。
func (s *SyncSession) key(rel string) string {
	return s.WS + "/" + rel
}

// unkey把索引路径还原成工作区内的相对路径；不属于本工作区的返回false。
func (s *SyncSession) unkey(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, s.WS+"/")
	return rest, ok
}

// SetWorkspace绑定工作区：索引前缀与磁盘目录都随它走。
// Root为空表示调用方直接给了Dir（单元测试），此时只设键、不动目录。
func (s *SyncSession) SetWorkspace(ws string) error {
	if !ValidWorkspace(ws) {
		return errors.New("sync: 非法的工作区键")
	}
	s.WS = ws
	if s.Root != "" {
		dir := filepath.Join(s.Root, ws)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		s.Dir = NewDirStore(dir)
	}
	return nil
}

func (s *SyncSession) lock() {
	if s.Lock != nil {
		s.Lock.Lock()
	}
}

func (s *SyncSession) unlock() {
	if s.Lock != nil {
		s.Lock.Unlock()
	}
}

// HandlePut：CAS基线检查→写临时文件（字节上限）→SHA校验→cleanup/配额门禁→原子替换→提交新revision。
// 任何失败都Discard临时文件，绝不部分落盘。
func (s *SyncSession) HandlePut(ctx context.Context, path string, baseRev int64, declaredSHA string, content io.Reader) PutResult {
	rel, err := SafeRelPath(path)
	if err != nil {
		return PutResult{Reason: "unsafe_path"}
	}
	s.lock() // 从读当前revision到提交，全程串行，防并发TOCTOU
	defer s.unlock()
	curRev, oldSize, exists, err := s.Store.Current(ctx, s.ProjectID, s.key(rel))
	if err != nil {
		return PutResult{Reason: "internal"}
	}
	// CAS：客户端基线必须与服务端当前一致，否则冲突（不静默覆盖）
	if exists && curRev != baseRev {
		return PutResult{Reason: "conflict"}
	}
	if !exists && baseRev != 0 {
		return PutResult{Reason: "conflict"}
	}
	tmpID, gotSHA, size, err := s.Dir.WriteTemp(rel, content, s.MaxBytes)
	if err == ErrTooLarge {
		return PutResult{Reason: "too_large"}
	}
	if err != nil {
		return PutResult{Reason: "internal"}
	}
	if gotSHA != declaredSHA {
		s.Dir.Discard(tmpID)
		return PutResult{Reason: "sha_mismatch"}
	}
	if s.Mode == "cleanup" && size >= oldSize {
		s.Dir.Discard(tmpID)
		return PutResult{Reason: "readonly_mode"}
	}
	used, err := s.Store.TotalSize(ctx, s.ProjectID)
	if err != nil {
		s.Dir.Discard(tmpID)
		return PutResult{Reason: "internal"}
	}
	if s.AllowQuota(used, oldSize, size) != nil {
		s.Dir.Discard(tmpID)
		return PutResult{Reason: "disk_full"}
	}
	newRev := curRev + 1
	if err := s.Dir.Promote(rel, tmpID, newRev); err != nil {
		s.Dir.Discard(tmpID)
		return PutResult{Reason: "internal"}
	}
	e := FileEntry{Path: s.key(rel), Size: size, SHA256: gotSHA, Revision: newRev}
	if err := s.Store.Commit(ctx, s.ProjectID, e, s.Device); err != nil {
		return PutResult{Reason: "internal"}
	}
	return PutResult{OK: true, Revision: newRev}
}

// HandleDelete：CAS后写持久tombstone；文件已不存在则幂等成功。
func (s *SyncSession) HandleDelete(ctx context.Context, path string, baseRev int64) PutResult {
	rel, err := SafeRelPath(path)
	if err != nil {
		return PutResult{Reason: "unsafe_path"}
	}
	s.lock()
	defer s.unlock()
	curRev, _, exists, err := s.Store.Current(ctx, s.ProjectID, s.key(rel))
	if err != nil {
		return PutResult{Reason: "internal"}
	}
	if exists && curRev != baseRev {
		return PutResult{Reason: "conflict"}
	}
	if !exists {
		return PutResult{OK: true, Revision: baseRev} // 幂等
	}
	newRev := curRev + 1
	if err := s.Dir.Delete(rel, newRev); err != nil {
		return PutResult{Reason: "internal"}
	}
	e := FileEntry{Path: s.key(rel), Revision: newRev, Deleted: true}
	if err := s.Store.Commit(ctx, s.ProjectID, e, s.Device); err != nil {
		return PutResult{Reason: "internal"}
	}
	return PutResult{OK: true, Revision: newRev}
}

// HandleManifest：返回清单前先把云端workspace的改动（Claude在容器内改的文件）
// 入账到file_index，这样客户端拉到的清单包含云端最新状态（云端→本地方向）。
func (s *SyncSession) HandleManifest(ctx context.Context) ([]FileEntry, error) {
	s.reconcileCloud(ctx)
	all, err := s.Store.Manifest(ctx, s.ProjectID)
	if err != nil {
		return nil, err
	}
	// **只把本工作区的条目给客户端**。这一句是"不同本地目录互不污染"的
	// 最后一道闸：漏掉它，客户端会把别的工作区的文件当成自己该有的下下来。
	out := make([]FileEntry, 0, len(all))
	for _, e := range all {
		rel, ok := s.unkey(e.Path)
		if !ok {
			continue
		}
		e.Path = rel
		out = append(out, e)
	}
	return out, nil
}

// reconcileCloud扫描workspace，把与file_index不一致的文件入账为新revision，
// 消失的文件写tombstone。客户端刚put的文件sha与索引一致会被跳过，不会重复入账。
// 在项目锁内串行，避免与客户端put竞争。
func (s *SyncSession) reconcileCloud(ctx context.Context) {
	s.lock()
	defer s.unlock()
	scanned, err := ScanDir(s.Dir.Root())
	if err != nil {
		return
	}
	all, err := s.Store.Manifest(ctx, s.ProjectID)
	if err != nil {
		return
	}
	// 只对账本工作区：ScanDir扫的是本工作区目录，拿全项目的索引去比，
	// 别的工作区的文件会因"扫不到"而被全部写成tombstone。
	idx := make(map[string]FileEntry, len(all))
	var remote []FileEntry
	for _, r := range all {
		rel, ok := s.unkey(r.Path)
		if !ok {
			continue
		}
		r.Path = rel
		idx[rel] = r
		remote = append(remote, r)
	}
	seen := make(map[string]bool, len(scanned))
	for _, f := range scanned {
		seen[f.Path] = true
		cur, ok := idx[f.Path]
		if ok && !cur.Deleted && cur.SHA256 == f.SHA256 {
			continue // 未变（含客户端刚put的）
		}
		_ = s.Store.Commit(ctx, s.ProjectID,
			FileEntry{Path: s.key(f.Path), Size: f.Size, SHA256: f.SHA256, Revision: cur.Revision + 1}, "cloud")
	}
	for _, r := range remote {
		if !r.Deleted && !seen[r.Path] {
			_ = s.Store.Commit(ctx, s.ProjectID,
				FileEntry{Path: s.key(r.Path), Revision: r.Revision + 1, Deleted: true}, "cloud")
		}
	}
}

// HandleGet：返回文件的当前元数据与内容流（客户端下载云端版本用）。调用方负责关闭reader。
func (s *SyncSession) HandleGet(ctx context.Context, path string) (FileEntry, io.ReadCloser, error) {
	rel, err := SafeRelPath(path)
	if err != nil {
		return FileEntry{}, nil, err
	}
	rev, size, exists, err := s.Store.Current(ctx, s.ProjectID, s.key(rel))
	if err != nil {
		return FileEntry{}, nil, err
	}
	if !exists {
		return FileEntry{}, nil, errors.New("sync: not found")
	}
	rc, err := s.Dir.Open(rel)
	if err != nil {
		return FileEntry{}, nil, err
	}
	return FileEntry{Path: rel, Size: size, Revision: rev}, rc, nil
}
