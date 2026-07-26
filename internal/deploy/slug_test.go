package deploy

import (
	"strings"
	"testing"
)

// slug校验是安全边界：slug会被拼进容器名、卷名、宿主机路径（渲染计划§6）。
// 风格对齐internal/sync/paths.go——独立单测、逐类拒绝用例。

func TestValidateSlugAccepts(t *testing.T) {
	valid := []string{
		"ab",
		"a1",
		"project-a",
		"x2-y3-z4",
		"0steel",
		strings.Repeat("a", 32), // 上限32
	}
	for _, s := range valid {
		if err := ValidateSlug(s); err != nil {
			t.Errorf("ValidateSlug(%q) = %v, want nil", s, err)
		}
	}
}

func TestValidateSlugRejects(t *testing.T) {
	cases := []struct {
		name, slug string
	}{
		{"空串", ""},
		{"单字符（计划§6：总长2–32）", "a"},
		{"超长33", strings.Repeat("a", 33)},
		{"大写", "Project-A"},
		{"下划线", "a_b"},
		{"点", "a.b"},
		{"路径穿越", ".."},
		{"斜杠", "a/b"},
		{"反斜杠", `a\b`},
		{"空格", "a b"},
		{"非ASCII", "项目a"},
		{"连字符开头", "-ab"},
		{"连字符结尾", "ab-"},
		{"NUL", "a\x00b"},
	}
	for _, c := range cases {
		if err := ValidateSlug(c.slug); err == nil {
			t.Errorf("%s: ValidateSlug(%q) = nil, want error", c.name, c.slug)
		}
	}
}

// 保留名与compose内既有service冲突时必须拒绝（渲染计划§6）。
func TestValidateSlugRejectsReservedNames(t *testing.T) {
	for _, s := range []string{"postgres", "control-api", "worker-agent", "caddy"} {
		err := ValidateSlug(s)
		if err == nil {
			t.Fatalf("ValidateSlug(%q) = nil, want reserved-name error", s)
		}
		if !strings.Contains(err.Error(), s) {
			t.Errorf("保留名错误信息应指明是哪一个slug（B6），got: %v", err)
		}
	}
}

// 错误信息必须指明具体哪一个slug非法（验收B6）。
func TestValidateSlugsNamesOffender(t *testing.T) {
	err := ValidateSlugs([]string{"good-one", "BAD"})
	if err == nil {
		t.Fatal("want error for slug list containing BAD")
	}
	if !strings.Contains(err.Error(), "BAD") {
		t.Errorf("错误信息应含非法slug %q，got: %v", "BAD", err)
	}
}

func TestValidateSlugsRejectsDuplicates(t *testing.T) {
	err := ValidateSlugs([]string{"alpha", "beta", "alpha"})
	if err == nil {
		t.Fatal("want error for duplicate slug")
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Errorf("重复slug的错误信息应指明是哪一个，got: %v", err)
	}
}

// 项目数硬上限3（设计§7.6产品规则）：第4个即拒绝整次渲染，不做截断，
// 且错误信息须说明上限来源。
func TestValidateSlugsEnforcesMaxProjects(t *testing.T) {
	if err := ValidateSlugs([]string{"p1", "p2", "p3"}); err != nil {
		t.Fatalf("3个项目应通过，got: %v", err)
	}
	err := ValidateSlugs([]string{"p1", "p2", "p3", "p4"})
	if err == nil {
		t.Fatal("第4个项目必须被拒绝（设计§7.6）")
	}
	if !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "7.6") {
		t.Errorf("上限错误信息应说明数量上限与来源（设计§7.6），got: %v", err)
	}
}

func TestValidateSlugsRejectsEmptyList(t *testing.T) {
	if err := ValidateSlugs(nil); err == nil {
		t.Fatal("空项目列表应被拒绝——渲染出没有任何项目的compose没有意义")
	}
}
