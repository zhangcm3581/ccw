package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccw/internal/deploy"
)

func runRC(t *testing.T, args []string, dbSlugs func() ([]string, error)) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := runRenderCompose(args, &out, &errBuf, dbSlugs)
	return code, out.String(), errBuf.String()
}

func noDB() ([]string, error) { return nil, errors.New("test: 不应触碰数据库") }

// 渲染不需要数据库（渲染计划§3.2/§3.3）：非--check路径绝不能调用dbSlugs。
func TestRenderComposeToStdoutWithoutDB(t *testing.T) {
	code, out, _ := runRC(t, []string{"--projects", "project-b,project-a"}, noDB)
	if code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
	want, err := deploy.RenderCompose(deploy.ComposeInput{Projects: []string{"project-a", "project-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if out != want {
		t.Error("stdout输出应与RenderCompose结果一致（且与--projects书写顺序无关）")
	}
}

func TestRenderComposeRequiresProjects(t *testing.T) {
	code, _, stderr := runRC(t, nil, noDB)
	if code == 0 {
		t.Fatal("缺--projects应失败")
	}
	if !strings.Contains(stderr, "--projects") {
		t.Errorf("错误信息应提示--projects，got: %s", stderr)
	}
}

func TestRenderComposeWritesOutFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yaml")
	code, out, _ := runRC(t, []string{"--projects", "project-a", "--out", path}, noDB)
	if code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
	if out != "" {
		t.Error("--out模式不应再写stdout")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "project-a-workspace:/srv/ccw/project-a") {
		t.Error("输出文件内容不完整")
	}
}

// --disk-gib默认15、超过15拒绝（设计§7.6）。该值不影响渲染输出——
// 磁盘配额是逻辑配额、存数据库（init-project负责），这里只做上限强制。
func TestRenderComposeDiskGiBCap(t *testing.T) {
	code, _, stderr := runRC(t, []string{"--projects", "project-a", "--disk-gib", "16"}, noDB)
	if code == 0 {
		t.Fatal("--disk-gib 16应被拒绝（验收A35同款规则）")
	}
	if !strings.Contains(stderr, "7.6") {
		t.Errorf("错误信息应说明上限来源（设计§7.6），got: %s", stderr)
	}
	if code, _, _ = runRC(t, []string{"--projects", "project-a", "--disk-gib", "15"}, noDB); code != 0 {
		t.Error("--disk-gib 15应通过")
	}
	if code, _, _ = runRC(t, []string{"--projects", "project-a", "--disk-gib", "0"}, noDB); code == 0 {
		t.Error("--disk-gib 0应被拒绝")
	}
}

func TestRenderComposeRejectsInvalidSlug(t *testing.T) {
	code, _, stderr := runRC(t, []string{"--projects", "project-a,BAD"}, noDB)
	if code == 0 {
		t.Fatal("非法slug应被拒绝")
	}
	if !strings.Contains(stderr, "BAD") {
		t.Errorf("错误信息应指明非法slug，got: %s", stderr)
	}
}

// --check：与数据库一致时退出0；漂移时退出非0并指明差异（验收B8）。
func TestRenderComposeCheckMode(t *testing.T) {
	dbOK := func() ([]string, error) { return []string{"project-a", "project-b"}, nil }
	code, out, _ := runRC(t, []string{"--check", "--projects", "project-a,project-b"}, dbOK)
	if code != 0 {
		t.Fatalf("集合一致时--check应退出0，got %d（%s）", code, out)
	}
	if strings.Contains(out, "services:") {
		t.Error("--check不应输出YAML")
	}

	dbDrift := func() ([]string, error) { return []string{"project-a", "project-c"}, nil }
	code, out, stderr := runRC(t, []string{"--check", "--projects", "project-a,project-b"}, dbDrift)
	if code == 0 {
		t.Fatal("漂移时--check应退出非0")
	}
	combined := out + stderr
	if !strings.Contains(combined, "project-c") || !strings.Contains(combined, "project-b") {
		t.Errorf("--check输出应指明漂移的slug（两侧都要），got: %s", combined)
	}
}

func TestRenderComposeCheckDBError(t *testing.T) {
	dbErr := func() ([]string, error) { return nil, errors.New("db down") }
	code, _, _ := runRC(t, []string{"--check", "--projects", "project-a"}, dbErr)
	if code == 0 {
		t.Fatal("数据库不可达时--check应失败，不得当成'无漂移'")
	}
}
