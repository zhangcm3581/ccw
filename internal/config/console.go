package config

import "fmt"

// ConsoleConfig是ccw-console的配置（console-fleet-design §11.4、§2.4）。
//
// 与节点侧Config分开加载：Console是独立主机上的独立进程，两边需要的变量
// 几乎不相交；共用一个Load会迫使每个进程都配上自己用不到的变量。
// 本阶段只收录已实施功能（C1/C20/C18）用到的变量；C2/C3落地时再扩
// CCW_SECRET_KEY、CCW_ADMIN_ALLOWLIST等——不提前强制未使用的变量。
type ConsoleConfig struct {
	DatabaseURL string // Console独立库（不是节点库）
	DistDir     string // 客户端产物目录（下载分发的本体）
	ListenAddr  string // 默认127.0.0.1:8090：只监听回环，公网入口只有Caddy
}

// LoadConsole沿用「缺失即硬失败、无默认值」（CLAUDE.md）；
// 唯一例外是监听地址——回环默认值本身就是spec要求的安全默认。
func LoadConsole(getenv func(string) string) (ConsoleConfig, error) {
	c := ConsoleConfig{
		DatabaseURL: getenv("CCW_CONSOLE_DATABASE_URL"),
		DistDir:     getenv("CCW_DIST_DIR"),
		ListenAddr:  or(getenv("CCW_CONSOLE_LISTEN_ADDR"), "127.0.0.1:8090"),
	}
	if c.DatabaseURL == "" {
		return ConsoleConfig{}, fmt.Errorf("config: CCW_CONSOLE_DATABASE_URL is required; no default is provided")
	}
	if c.DistDir == "" {
		return ConsoleConfig{}, fmt.Errorf("config: CCW_DIST_DIR is required; no default is provided")
	}
	return c, nil
}
