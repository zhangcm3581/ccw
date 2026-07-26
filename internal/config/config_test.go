package config

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func valid() map[string]string {
	return map[string]string{
		"CCW_DATABASE_URL":   "postgres://ccw:pw@localhost:5432/ccw",
		"CCW_TOKEN_KEY":      "8ba7167acf1c9ee1cbfbcbf0b2c7e51ecdf8b1d0a9b3c2d1e0f1a2b3c4d5e6f7",
		"CCW_WORKSPACE_ROOT": "/srv/ccw",
	}
}

func TestLoadValid(t *testing.T) {
	c, err := Load(env(valid()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.TokenSigningKey) != 32 {
		t.Fatalf("want 32-byte key, got %d", len(c.TokenSigningKey))
	}
	// 默认只监听回环地址（审查§3.1）：绑定所有网卡的":8080"形式是错误答案
	if c.ListenAddr != "127.0.0.1:8080" || c.AgentListenAddr != "127.0.0.1:8081" {
		t.Fatalf("defaults must bind loopback only: %+v", c)
	}
}

func TestLoadMissingEach(t *testing.T) {
	for _, k := range []string{"CCW_DATABASE_URL", "CCW_TOKEN_KEY", "CCW_WORKSPACE_ROOT"} {
		m := valid()
		delete(m, k)
		if _, err := Load(env(m)); err == nil {
			t.Fatalf("missing %s: want error, got nil", k)
		}
	}
}

func TestLoadShortKeyRejected(t *testing.T) {
	m := valid()
	m["CCW_TOKEN_KEY"] = "abcd"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("short key must be rejected")
	}
}

// 用量配置只有worker-agent需要（control-api与cclaude不采集），因此不放进Load的
// 通用必填项，而是由worker-agent启动时调RequireUsage二次校验。
// 但格式错误必须在Load阶段就被拒——把"5x"当成0会让整类token静默不计量。
func TestLoadUsageWeights(t *testing.T) {
	m := valid()
	m["CCW_USAGE_WEIGHTS"] = "1,5,1,2"
	m["CCW_USAGE_ROOT"] = "/srv/ccw/usage"
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	w := c.UsageWeights
	if w.Input != 1 || w.Output != 5 || w.CacheRead != 1 || w.CacheWrite != 2 {
		t.Fatalf("weights parsed wrong: %+v", w)
	}
	if c.UsageRoot != "/srv/ccw/usage" {
		t.Fatalf("usage root: %q", c.UsageRoot)
	}
	if err := c.RequireUsage(); err != nil {
		t.Fatalf("RequireUsage should pass: %v", err)
	}
}

func TestLoadUsageWeightsMalformed(t *testing.T) {
	for _, bad := range []string{
		"1,5,1",      // 少一个字段
		"1,5,1,2,3",  // 多一个字段
		"1,5,1,x",    // 非数字
		"1,5,1,-1",   // 负权重会让用量随token增加而下降
		"",           // 空字符串在设置了变量的情况下也是错的
		" 1,5, 1, 2", // 不做宽松解析：格式必须严格
	} {
		m := valid()
		m["CCW_USAGE_WEIGHTS"] = bad
		if bad == "" {
			continue // 未设置与设为空串在getenv下不可区分，交给RequireUsage处理
		}
		if _, err := Load(env(m)); err == nil {
			t.Errorf("CCW_USAGE_WEIGHTS=%q 应被拒绝", bad)
		}
	}
}

// 未配置时Load不报错（control-api用得着这条），但RequireUsage必须报错——
// 否则worker-agent会带着零权重跑起来，所有用量都算作0，闸门静默失效。
func TestRequireUsageFailsWhenUnset(t *testing.T) {
	c, err := Load(env(valid()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.RequireUsage(); err == nil {
		t.Fatal("缺少CCW_USAGE_ROOT/CCW_USAGE_WEIGHTS时RequireUsage必须报错")
	}
}

// 权重全零等于"不计量"：语法合法但语义上让闸门失效，必须拒绝。
func TestRequireUsageRejectsAllZeroWeights(t *testing.T) {
	m := valid()
	m["CCW_USAGE_ROOT"] = "/srv/ccw/usage"
	m["CCW_USAGE_WEIGHTS"] = "0,0,0,0"
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.RequireUsage(); err == nil {
		t.Fatal("全零权重必须被拒绝：所有用量都会算作0，闸门形同虚设")
	}
}
