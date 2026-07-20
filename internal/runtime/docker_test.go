package runtime

import (
	"context"
	"strings"
	"testing"

	"ccw/internal/project"
)

type fakeDocker struct {
	volumes    []string
	containers []ContainerSpec
}

func (f *fakeDocker) EnsureVolume(_ context.Context, name string) error {
	f.volumes = append(f.volumes, name)
	return nil
}

func (f *fakeDocker) EnsureContainer(_ context.Context, s ContainerSpec) error {
	f.containers = append(f.containers, s)
	return nil
}

func (f *fakeDocker) RemoveContainer(_ context.Context, _ string) error { return nil }

var pa = project.Project{ID: "11111111-1111-1111-1111-111111111111", Slug: "project-a", ContainerName: "ccw-project-a"}
var pb = project.Project{ID: "22222222-2222-2222-2222-222222222222", Slug: "project-b", ContainerName: "ccw-project-b"}

func TestMountsNeverCrossProjects(t *testing.T) {
	f := &fakeDocker{}
	if err := EnsureProjectRuntime(context.Background(), f, pa, "img"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProjectRuntime(context.Background(), f, pb, "img"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.containers {
		for _, m := range c.Mounts {
			if c.Name == pa.ContainerName && strings.Contains(m.Volume, "project-b") {
				t.Fatalf("container A mounts B volume: %+v", m)
			}
			if c.Name == pb.ContainerName && strings.Contains(m.Volume, "project-a") {
				t.Fatalf("container B mounts A volume: %+v", m)
			}
		}
		if len(c.Mounts) != 3 {
			t.Fatalf("must mount exactly 3 volumes, got %d", len(c.Mounts))
		}
	}
}

func TestVolumeNamesStableAcrossRebuild(t *testing.T) {
	w1, c1, s1 := VolumeNames(pa)
	w2, c2, s2 := VolumeNames(pa)
	if w1 != w2 || c1 != c2 || s1 != s2 {
		t.Fatal("volume names must be deterministic")
	}
	if w1 != "project-a-workspace" || c1 != "project-a-claude" || s1 != "project-a-sync" {
		t.Fatalf("unexpected names: %s %s %s", w1, c1, s1)
	}
}

func TestSecurityDefaults(t *testing.T) {
	f := &fakeDocker{}
	_ = EnsureProjectRuntime(context.Background(), f, pa, "img")
	c := f.containers[0]
	if c.User == "" || c.User == "root" || c.User == "0" {
		t.Fatalf("container must run as non-root, got %q", c.User)
	}
	if c.Limits.MemoryBytes == 0 || c.Limits.PidsLimit == 0 || c.Limits.NanoCPUs == 0 {
		t.Fatalf("resource limits must be set: %+v", c.Limits)
	}
	// PID 1必须是sleep infinity而不是tmux（无TTY下tmux前台会立即退出，审计§4.1）
	if got := strings.Join(c.Cmd, " "); got != "sleep infinity" {
		t.Fatalf("pid1 must be sleep infinity, got %q", got)
	}
}
