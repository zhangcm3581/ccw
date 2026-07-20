package storage

import (
	"context"
	"errors"
	stdsync "sync"

	syncpkg "ccw/internal/sync"
)

var ErrDiskFull = errors.New("storage: disk quota exceeded")

type Index interface {
	Upsert(ctx context.Context, projectID string, e syncpkg.FileEntry) error
	DiskUsed(ctx context.Context, projectID string) (int64, error)
}

// MemoryIndex：单元测试与本地使用；生产PGIndex（写file_index表、同事务SUM）在Task 12接入。
type MemoryIndex struct {
	mu stdsync.Mutex
	m  map[string]map[string]syncpkg.FileEntry
}

func NewMemoryIndex() *MemoryIndex {
	return &MemoryIndex{m: map[string]map[string]syncpkg.FileEntry{}}
}

func (i *MemoryIndex) Upsert(_ context.Context, pid string, e syncpkg.FileEntry) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.m[pid] == nil {
		i.m[pid] = map[string]syncpkg.FileEntry{}
	}
	i.m[pid][e.Path] = e
	return nil
}

func (i *MemoryIndex) DiskUsed(_ context.Context, pid string) (int64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var sum int64
	for _, e := range i.m[pid] {
		if !e.Deleted {
			sum += e.Size
		}
	}
	return sum, nil
}

type Gate struct{ Limit int64 }

// Allow：projected = used - oldSize + newSize；只有增量为正且超限才拒绝。
// 删除与缩小（newSize<=oldSize）永远允许，即使已达上限（审查§2.6/§9）。
func (g Gate) Allow(used, oldSize, newSize int64) error {
	if newSize <= oldSize {
		return nil
	}
	if used-oldSize+newSize > g.Limit {
		return ErrDiskFull
	}
	return nil
}
