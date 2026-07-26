package config

import (
	"strings"
	"testing"
)

// Console配置（console-fleet-design §11.4）：沿用「缺失即硬失败、无默认值」，
// 但只强制ccw-console实际用到的变量——把别的进程不需要的变量塞进Load会让它们
// 无谓失败（与RequireUsage的分寸一致）。

func consoleEnv(over map[string]string) func(string) string {
	base := map[string]string{
		"CCW_CONSOLE_DATABASE_URL": "postgres://x/console",
		"CCW_DIST_DIR":             "/srv/ccw-console/dist",
	}
	for k, v := range over {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestLoadConsoleDefaults(t *testing.T) {
	c, err := LoadConsole(consoleEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != "postgres://x/console" || c.DistDir != "/srv/ccw-console/dist" {
		t.Errorf("字段错误: %+v", c)
	}
	if c.ListenAddr != "127.0.0.1:8090" {
		t.Errorf("默认监听应为127.0.0.1:8090（只监听回环，spec规则），got %s", c.ListenAddr)
	}
}

func TestLoadConsoleHardFails(t *testing.T) {
	for _, missing := range []string{"CCW_CONSOLE_DATABASE_URL", "CCW_DIST_DIR"} {
		_, err := LoadConsole(consoleEnv(map[string]string{missing: ""}))
		if err == nil {
			t.Errorf("缺%s应硬失败", missing)
		} else if !strings.Contains(err.Error(), missing) {
			t.Errorf("错误信息应指明缺的是%s，got: %v", missing, err)
		}
	}
}

func TestLoadConsoleListenOverride(t *testing.T) {
	c, err := LoadConsole(consoleEnv(map[string]string{"CCW_CONSOLE_LISTEN_ADDR": "0.0.0.0:9000"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != "0.0.0.0:9000" {
		t.Errorf("覆盖失败: %s", c.ListenAddr)
	}
}
