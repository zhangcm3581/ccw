package deploy

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 本文件逐条断言渲染计划§5的模板契约I1–I7，外加用量接线计划§9要求补充的I8。
// 这些不变量是渲染器的正确性定义；I3/I4/I8是产品硬约束的技术表达。

// render渲染并解析为通用YAML树，失败即中止测试。
func render(t *testing.T, slugs ...string) (string, map[string]any) {
	t.Helper()
	out, err := RenderCompose(ComposeInput{Projects: slugs})
	if err != nil {
		t.Fatalf("RenderCompose(%v): %v", slugs, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("渲染结果不是合法YAML: %v\n%s", err, out)
	}
	return out, doc
}

func section(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := doc[key].(map[string]any)
	if !ok {
		t.Fatalf("缺少%q段或类型不对: %T", key, doc[key])
	}
	return v
}

func service(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	svcs := section(t, doc, "services")
	v, ok := svcs[name].(map[string]any)
	if !ok {
		t.Fatalf("缺少service %q", name)
	}
	return v
}

func volumesOf(t *testing.T, svc map[string]any) []string {
	t.Helper()
	raw, ok := svc["volumes"].([]any)
	if !ok {
		t.Fatalf("service缺volumes段")
	}
	var out []string
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

// I1：每个项目在worker-agent.volumes里有且仅有一条<slug>-workspace:/srv/ccw/<slug>。
// 漏掉的后果是同步静默写进worker容器自身文件系统（渲染计划§2.3——本计划最主要的动机）。
func TestInvariantI1WorkerWorkspaceMounts(t *testing.T) {
	_, doc := render(t, "beta", "alpha")
	vols := volumesOf(t, service(t, doc, "worker-agent"))
	for _, slug := range []string{"alpha", "beta"} {
		want := fmt.Sprintf("%s-workspace:/srv/ccw/%s", slug, slug)
		if n := count(vols, want); n != 1 {
			t.Errorf("I1: worker-agent应恰好有1条%q，实际%d条；volumes=%v", want, n, vols)
		}
	}
}

// I8：每个项目的claude-projects卷必须以只读方式挂进worker-agent的
// /srv/ccw/usage/<slug>（用量接线计划§9）。漏挂的表现是采集器安静地扫空目录、
// usage_events永远为空——与接线前的现象完全一样。
func TestInvariantI8WorkerUsageMountsReadOnly(t *testing.T) {
	_, doc := render(t, "alpha", "beta")
	vols := volumesOf(t, service(t, doc, "worker-agent"))
	for _, slug := range []string{"alpha", "beta"} {
		want := fmt.Sprintf("%s-claude-projects:/srv/ccw/usage/%s:ro", slug, slug)
		if n := count(vols, want); n != 1 {
			t.Errorf("I8: worker-agent应恰好有1条只读挂载%q，实际%d条；volumes=%v", want, n, vols)
		}
	}
	// 挂载根必须与CCW_USAGE_ROOT一致，否则采集器扫的不是挂进来的目录。
	env, ok := service(t, doc, "worker-agent")["environment"].(map[string]any)
	if !ok {
		t.Fatal("worker-agent缺environment段")
	}
	if got := env["CCW_USAGE_ROOT"]; got != "/srv/ccw/usage" {
		t.Errorf("CCW_USAGE_ROOT=%v，与usage挂载根/srv/ccw/usage不一致", got)
	}
}

// I2：每个项目容器恰好4项挂载：workspace、claude-shared、claude-projects、sync。
func TestInvariantI2ProjectMounts(t *testing.T) {
	_, doc := render(t, "alpha", "beta")
	for _, slug := range []string{"alpha", "beta"} {
		vols := volumesOf(t, service(t, doc, slug))
		want := []string{
			slug + "-workspace:/workspace",
			"claude-shared:/home/claude",
			slug + "-claude-projects:/home/claude/.claude/projects",
			slug + "-sync:/var/lib/cclaude-sync",
		}
		if len(vols) != len(want) {
			t.Errorf("I2: 项目%s应恰好%d项挂载，实际%d项: %v", slug, len(want), len(vols), vols)
		}
		for _, w := range want {
			if count(vols, w) != 1 {
				t.Errorf("I2: 项目%s缺挂载%q；volumes=%v", slug, w, vols)
			}
		}
	}
}

// I3：所有项目共用同一个claude-shared:/home/claude（验收B7）。
// 这是「一台VPS只授权一次」产品硬约束的技术表达（设计§7.3）。
func TestInvariantI3SharedClaudeHome(t *testing.T) {
	_, doc := render(t, "alpha", "beta", "gamma")
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		vols := volumesOf(t, service(t, doc, slug))
		if count(vols, "claude-shared:/home/claude") != 1 {
			t.Errorf("I3/B7: 项目%s必须挂载共享卷claude-shared:/home/claude；volumes=%v", slug, vols)
		}
	}
}

// I4：<slug>-claude-projects挂载点恒为/home/claude/.claude/projects。
// 挂错层级会遮蔽.credentials.json，导致每个项目各自要求登录（违反授权一次）。
func TestInvariantI4ClaudeProjectsMountpoint(t *testing.T) {
	_, doc := render(t, "alpha", "beta")
	for _, slug := range []string{"alpha", "beta"} {
		for _, v := range volumesOf(t, service(t, doc, slug)) {
			if strings.HasPrefix(v, slug+"-claude-projects:") &&
				v != slug+"-claude-projects:/home/claude/.claude/projects" {
				t.Errorf("I4: 项目%s的claude-projects挂载点错误: %q", slug, v)
			}
		}
	}
}

// I5：容器名恒为ccw-<slug>，与数据库container_name约定一致，
// 否则worker-agent的docker exec找不到容器。
func TestInvariantI5ContainerNames(t *testing.T) {
	_, doc := render(t, "alpha", "beta")
	for _, slug := range []string{"alpha", "beta"} {
		if got := service(t, doc, slug)["container_name"]; got != "ccw-"+slug {
			t.Errorf("I5: 项目%s的container_name=%v，want ccw-%s", slug, got, slug)
		}
	}
}

// I6：volumes段声明的卷与全部service引用的卷完全一致，无多余、无遗漏。
func TestInvariantI6VolumeDeclarations(t *testing.T) {
	_, doc := render(t, "alpha", "beta")
	declared := map[string]bool{}
	for name := range section(t, doc, "volumes") {
		declared[name] = true
	}
	referenced := map[string]bool{}
	for name := range section(t, doc, "services") {
		svc := service(t, doc, name)
		if _, has := svc["volumes"]; !has {
			continue
		}
		for _, v := range volumesOf(t, svc) {
			src := strings.SplitN(v, ":", 2)[0]
			if strings.HasPrefix(src, "/") || strings.HasPrefix(src, "./") {
				continue // bind mount（docker.sock、Caddyfile）不需要声明
			}
			referenced[src] = true
		}
	}
	for name := range referenced {
		if !declared[name] {
			t.Errorf("I6: 卷%q被引用但未在volumes段声明（compose会启动失败）", name)
		}
	}
	for name := range declared {
		if !referenced[name] {
			t.Errorf("I6: 卷%q声明了但没人引用（孤儿卷）", name)
		}
	}
}

// I7：同一输入字节级稳定（B3），项目顺序打乱后输出不变（B4）。
// 这是--check与push-artifacts的sha256比对能工作的前提。
func TestInvariantI7Deterministic(t *testing.T) {
	a1, _ := render(t, "alpha", "beta", "gamma")
	a2, _ := render(t, "alpha", "beta", "gamma")
	if a1 != a2 {
		t.Error("I7/B3: 同一输入两次渲染输出不同")
	}
	b, _ := render(t, "gamma", "alpha", "beta")
	if a1 != b {
		t.Error("I7/B4: 打乱项目顺序后输出改变（应按slug字典序归一）")
	}
	if !strings.HasSuffix(a1, "\n") || strings.HasSuffix(a1, "\n\n") {
		t.Error("I7: 输出必须以恰好一个\\n结尾")
	}
	if strings.Contains(a1, "\t") {
		t.Error("I7: 输出不得含TAB（缩进固定两空格）")
	}
}

// 镜像构建职责：字典序第一个项目带build块（节点本地构建镜像），
// 其余项目复用镜像并depends_on第一个——与现有手写compose.yaml的语义一致。
func TestFirstProjectBuildsImage(t *testing.T) {
	_, doc := render(t, "beta", "alpha", "gamma")
	first := service(t, doc, "alpha")
	if _, has := first["build"]; !has {
		t.Error("字典序第一个项目（alpha）应带build块")
	}
	for _, slug := range []string{"beta", "gamma"} {
		svc := service(t, doc, slug)
		if _, has := svc["build"]; has {
			t.Errorf("项目%s不应带build块（镜像由第一个项目构建）", slug)
		}
		deps, ok := svc["depends_on"].([]any)
		if !ok || len(deps) != 1 || deps[0] != "alpha" {
			t.Errorf("项目%s应depends_on: [alpha]，got %v", slug, svc["depends_on"])
		}
	}
}

// 基础设施service必须齐全，且渲染不随项目数改变它们的关键配置。
func TestBaseServicesPresent(t *testing.T) {
	_, doc := render(t, "alpha")
	for _, name := range []string{"postgres", "control-api", "worker-agent", "caddy"} {
		service(t, doc, name)
	}
	// caddy是唯一映射宿主机端口的service（公网只有443/80入口）。
	for name := range section(t, doc, "services") {
		_, hasPorts := service(t, doc, name)["ports"]
		if hasPorts && name != "caddy" {
			t.Errorf("service %s不得映射宿主机端口（对外只暴露Caddy）", name)
		}
	}
}

// 非法输入必须拒绝整次渲染（渲染计划§6），错误信息指明具体slug。
func TestRenderRejectsInvalidInput(t *testing.T) {
	cases := [][]string{
		{},
		{"BAD"},
		{"postgres"},
		{"alpha", "alpha"},
		{"p1", "p2", "p3", "p4"},
	}
	for _, slugs := range cases {
		if _, err := RenderCompose(ComposeInput{Projects: slugs}); err == nil {
			t.Errorf("RenderCompose(%v)应失败", slugs)
		}
	}
}

// --claude-image覆盖项目容器镜像；默认ccw-claude:latest（与现有手写文件一致）。
func TestClaudeImageOverride(t *testing.T) {
	_, doc := render(t, "alpha", "beta")
	if got := service(t, doc, "alpha")["image"]; got != "ccw-claude:latest" {
		t.Errorf("默认镜像应为ccw-claude:latest，got %v", got)
	}
	out, err := RenderCompose(ComposeInput{Projects: []string{"alpha", "beta"}, ClaudeImage: "ccw-claude:v1"})
	if err != nil {
		t.Fatal(err)
	}
	var doc2 map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc2); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"alpha", "beta"} {
		if got := service(t, doc2, slug)["image"]; got != "ccw-claude:v1" {
			t.Errorf("项目%s镜像应为ccw-claude:v1，got %v", slug, got)
		}
	}
}

func count(list []string, want string) int {
	n := 0
	for _, v := range list {
		if v == want {
			n++
		}
	}
	return n
}
