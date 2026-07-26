package deploy

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// 黄金文件锁定1/2/3个项目的渲染输出，防止无意的模板回归（渲染计划R3）。
// 更新：go test ./internal/deploy -run TestGolden -update
// 注意项目数上限为3（设计§7.6），因此没有更多项目数的黄金文件——
// 渲染计划§7原文的compose-10.golden.yaml写于上限定案之前，已不适用。
var update = flag.Bool("update", false, "重写黄金文件")

func TestGolden(t *testing.T) {
	cases := []struct {
		file  string
		slugs []string
	}{
		{"compose-1.golden.yaml", []string{"project-a"}},
		{"compose-2.golden.yaml", []string{"project-a", "project-b"}},
		{"compose-3.golden.yaml", []string{"project-a", "project-b", "project-c"}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			got, err := RenderCompose(ComposeInput{Projects: c.slugs})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", c.file)
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读黄金文件失败（先跑 -update 生成）: %v", err)
			}
			if got != string(want) {
				t.Errorf("渲染输出与%s不一致；若是有意改动模板，跑 -update 并人工审查diff", c.file)
			}
		})
	}
}
