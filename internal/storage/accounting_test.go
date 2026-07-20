package storage

import (
	"context"
	"testing"

	"ccw/internal/sync"
)

func TestLogicalBytesExact(t *testing.T) {
	ctx := context.Background()
	idx := NewMemoryIndex()
	idx.Upsert(ctx, "pa", sync.FileEntry{Path: "a.bin", Size: 221})
	used, _ := idx.DiskUsed(ctx, "pa")
	if used != 221 {
		t.Fatalf("want 221, got %d", used)
	}
	idx.Upsert(ctx, "pa", sync.FileEntry{Path: "b.bin", Size: 31})
	used, _ = idx.DiskUsed(ctx, "pa")
	if used != 252 {
		t.Fatalf("want 252, got %d", used)
	}
	idx.Upsert(ctx, "pa", sync.FileEntry{Path: "a.bin", Size: 221, Deleted: true})
	used, _ = idx.DiskUsed(ctx, "pa")
	if used != 31 {
		t.Fatalf("deleted must not count, got %d", used)
	}
}

func TestGate(t *testing.T) {
	g := Gate{Limit: 100}
	if err := g.Allow(90, 0, 20); err != ErrDiskFull {
		t.Fatalf("grow over limit must fail, got %v", err)
	}
	if err := g.Allow(90, 50, 20); err != nil {
		t.Fatalf("shrink must pass, got %v", err)
	}
	if err := g.Allow(100, 30, 0); err != nil {
		t.Fatalf("delete must pass even at limit, got %v", err)
	}
}

func TestProjectsIsolated(t *testing.T) {
	ctx := context.Background()
	idx := NewMemoryIndex()
	idx.Upsert(ctx, "pa", sync.FileEntry{Path: "a", Size: 10})
	used, _ := idx.DiskUsed(ctx, "pb")
	if used != 0 {
		t.Fatalf("pb must be 0, got %d", used)
	}
}
