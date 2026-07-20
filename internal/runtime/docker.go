package runtime

import (
	"context"

	"ccw/internal/project"
)

type Mount struct{ Volume, Target string }

type Limits struct {
	NanoCPUs    int64
	MemoryBytes int64
	PidsLimit   int64
}

type ContainerSpec struct {
	Name   string
	Image  string
	Mounts []Mount
	User   string
	Cmd    []string
	Limits Limits
}

type DockerAPI interface {
	EnsureVolume(ctx context.Context, name string) error
	EnsureContainer(ctx context.Context, spec ContainerSpec) error
	RemoveContainer(ctx context.Context, name string) error
}

// VolumeNames：确定性命名，容器删除重建后仍指向同一批持久卷。
func VolumeNames(p project.Project) (workspace, claudeHome, sync string) {
	return p.Slug + "-workspace", p.Slug + "-claude", p.Slug + "-sync"
}

// EnsureProjectRuntime为项目准备三个持久卷与一个固定名称容器。
// 只挂载本项目的卷——这是A/B隔离的物理边界。
func EnsureProjectRuntime(ctx context.Context, api DockerAPI, p project.Project, image string) error {
	w, c, s := VolumeNames(p)
	for _, v := range []string{w, c, s} {
		if err := api.EnsureVolume(ctx, v); err != nil {
			return err
		}
	}
	return api.EnsureContainer(ctx, ContainerSpec{
		Name:  p.ContainerName,
		Image: image,
		User:  "claude",
		Mounts: []Mount{
			{Volume: w, Target: "/workspace"},
			{Volume: c, Target: "/home/claude/.claude"},
			{Volume: s, Target: "/var/lib/cclaude-sync"},
		},
		// PID 1不能是tmux（容器启动时无TTY，前台tmux会立即退出）；
		// tmux会话由worker-agent在容器运行后经docker exec准备（Task 5）。
		Cmd: []string{"sleep", "infinity"},
		Limits: Limits{
			NanoCPUs:    2_000_000_000, // 2 CPU
			MemoryBytes: 4 << 30,       // 4 GiB
			PidsLimit:   512,
		},
	})
}
