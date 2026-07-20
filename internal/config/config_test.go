package config

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func valid() map[string]string {
	return map[string]string{
		"CCW_DATABASE_URL":  "postgres://ccw:pw@localhost:5432/ccw",
		"CCW_TOKEN_KEY":     "8ba7167acf1c9ee1cbfbcbf0b2c7e51ecdf8b1d0a9b3c2d1e0f1a2b3c4d5e6f7",
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
