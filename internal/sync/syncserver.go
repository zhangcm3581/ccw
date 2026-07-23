package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
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
	ProjectID  string
	Device     string
	Mode       string // "rw" | "cleanup"
	Store      RevisionStore
	Dir        *DirStore
	MaxBytes   int64
	AllowQuota func(used, oldSize, newSize int64) error
}

// HandlePut：CAS基线检查→写临时文件（字节上限）→SHA校验→cleanup/配额门禁→原子替换→提交新revision。
// 任何失败都Discard临时文件，绝不部分落盘。
func (s *SyncSession) HandlePut(ctx context.Context, path string, baseRev int64, declaredSHA string, content io.Reader) PutResult {
	rel, err := SafeRelPath(path)
	if err != nil {
		return PutResult{Reason: "unsafe_path"}
	}
	curRev, oldSize, exists, err := s.Store.Current(ctx, s.ProjectID, rel)
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
	e := FileEntry{Path: rel, Size: size, SHA256: gotSHA, Revision: newRev}
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
	curRev, _, exists, err := s.Store.Current(ctx, s.ProjectID, rel)
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
	e := FileEntry{Path: rel, Revision: newRev, Deleted: true}
	if err := s.Store.Commit(ctx, s.ProjectID, e, s.Device); err != nil {
		return PutResult{Reason: "internal"}
	}
	return PutResult{OK: true, Revision: newRev}
}

// HandleManifest：权威清单来自file_index（含未过保留期tombstone），不是磁盘扫描。
func (s *SyncSession) HandleManifest(ctx context.Context) ([]FileEntry, error) {
	return s.Store.Manifest(ctx, s.ProjectID)
}
