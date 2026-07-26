package deploy

import (
	"reflect"
	"testing"
)

// --check的核心：发现「数据库里有但compose里没有」的漂移（验收B8）。
// 那种漂移的受害者是拿到能认证但连不上容器的CDK的客户（渲染计划§6）。
func TestCheckDrift(t *testing.T) {
	cases := []struct {
		name         string
		rendered, db []string
		wantMissing  []string // 库里有、compose没有（危险：客户连不上容器）
		wantExtra    []string // compose有、库里没有（无主容器）
	}{
		{"一致", []string{"a1", "b2"}, []string{"b2", "a1"}, nil, nil},
		{"库里多", []string{"a1"}, []string{"a1", "c3"}, []string{"c3"}, nil},
		{"compose多", []string{"a1", "b2"}, []string{"a1"}, nil, []string{"b2"}},
		{"两边都漂", []string{"a1", "b2"}, []string{"a1", "c3"}, []string{"c3"}, []string{"b2"}},
		{"空库", []string{"a1"}, nil, nil, []string{"a1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			missing, extra := CheckDrift(c.rendered, c.db)
			if !reflect.DeepEqual(missing, c.wantMissing) {
				t.Errorf("missing=%v, want %v", missing, c.wantMissing)
			}
			if !reflect.DeepEqual(extra, c.wantExtra) {
				t.Errorf("extra=%v, want %v", extra, c.wantExtra)
			}
		})
	}
}
