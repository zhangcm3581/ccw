package project

import (
	"context"
	"errors"

	"ccw/internal/auth"
)

type Project struct {
	ID            string
	AccountID     string
	Slug          string
	ContainerName string
	DiskLimit     int64
	FiveHourLimit int64
	SevenDayLimit int64
}

var ErrInvalidCDK = errors.New("invalid cdk")

type Resolver interface {
	ResolveCDK(ctx context.Context, plain string) (Project, error)
}

// Entry：以public-id为键的CDK记录（与数据库行同构）。
type Entry struct {
	SecretHash string
	Project    Project
}

// MemoryResolver：单元测试与后续HTTP测试共用；生产实现在store包。
// 与生产实现一致：先按public-id做O(1)查找，再验证secret哈希。
type MemoryResolver struct{ byPublicID map[string]Entry }

func NewMemoryResolver(byPublicID map[string]Entry) *MemoryResolver {
	return &MemoryResolver{byPublicID: byPublicID}
}

func (r *MemoryResolver) ResolveCDK(_ context.Context, plain string) (Project, error) {
	pub, secret, err := auth.SplitCDK(plain)
	if err != nil {
		return Project{}, ErrInvalidCDK
	}
	e, ok := r.byPublicID[pub]
	if !ok || !auth.VerifySecret(secret, e.SecretHash) {
		return Project{}, ErrInvalidCDK
	}
	return e.Project, nil
}
