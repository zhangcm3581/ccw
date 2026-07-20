package project

import (
	"context"
	"testing"

	"ccw/internal/auth"
)

func TestCDKBindsExactlyOneProject(t *testing.T) {
	cdkA, pubA, _ := auth.NewCDK()
	cdkB, pubB, _ := auth.NewCDK()
	_, secA, _ := auth.SplitCDK(cdkA)
	_, secB, _ := auth.SplitCDK(cdkB)
	hashA, _ := auth.HashSecret(secA)
	hashB, _ := auth.HashSecret(secB)
	r := NewMemoryResolver(map[string]Entry{
		pubA: {SecretHash: hashA, Project: Project{ID: "pa", Slug: "project-a"}},
		pubB: {SecretHash: hashB, Project: Project{ID: "pb", Slug: "project-b"}},
	})
	p, err := r.ResolveCDK(context.Background(), cdkA)
	if err != nil || p.ID != "pa" {
		t.Fatalf("cdkA must resolve to project A only, got %+v err=%v", p, err)
	}
	// 正确public-id+错误secret也必须失败
	if _, err := r.ResolveCDK(context.Background(), "ccw_"+pubA+".wrongsecret"); err != ErrInvalidCDK {
		t.Fatalf("wrong secret must return ErrInvalidCDK, got %v", err)
	}
	if _, err := r.ResolveCDK(context.Background(), "ccw_unknown.zzz"); err != ErrInvalidCDK {
		t.Fatalf("unknown cdk must return ErrInvalidCDK, got %v", err)
	}
}
