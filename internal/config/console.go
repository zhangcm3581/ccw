package config

import (
	"fmt"

	"ccw/internal/secretbox"
)

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

	// SecretKey是信封加密主密钥（32字节，来自CCW_SECRET_KEY的hex）。
	// 托管SSH私钥、AWS凭据、TOTP secret都用它加密落库；**丢失即全部不可解**
	// （设计§14），部署runbook必须写明备份要求。
	SecretKey []byte
	// AdminAllowlist是管理后台的IP白名单原文（Caddy同款语法）。
	// 应用层独立解析与校验，不只依赖反代（设计§8.3的双重校验）。
	AdminAllowlist string
	// AdminInsecureCookie=true时会话cookie不带Secure属性，仅供本地HTTP调试。
	AdminInsecureCookie bool
	// LogDir存放流水线运行日志（每run一个文件，0600）。空值＝只在内存里保留，
	// Console重启后历史日志丢失。
	LogDir string
	// NodeSrcPath是节点源码包（tar.gz）的路径。纳管时推送到节点，
	// **节点靠它构建自己的镜像**——缺了它compose-up必然失败，
	// 因此文件不存在时机队管理不启用（宁可不开，也不要跑到第8步才炸）。
	NodeSrcPath string
}

// LoadConsole沿用「缺失即硬失败、无默认值」（CLAUDE.md）；
// 唯一例外是监听地址——回环默认值本身就是spec要求的安全默认。
//
// CCW_SECRET_KEY与CCW_ADMIN_ALLOWLIST**当前是可选**：管理后台尚在分批实施，
// 只跑公开站点的部署不该被迫配置它们。二者缺失时Console不注册任何/admin路由
// （没有认证就不上管理页面）。等后台功能完整后再改为必填。
func LoadConsole(getenv func(string) string) (ConsoleConfig, error) {
	c := ConsoleConfig{
		DatabaseURL:         getenv("CCW_CONSOLE_DATABASE_URL"),
		DistDir:             getenv("CCW_DIST_DIR"),
		ListenAddr:          or(getenv("CCW_CONSOLE_LISTEN_ADDR"), "127.0.0.1:8090"),
		AdminAllowlist:      getenv("CCW_ADMIN_ALLOWLIST"),
		AdminInsecureCookie: getenv("CCW_ADMIN_INSECURE_COOKIE") == "1",
		LogDir:              or(getenv("CCW_LOG_DIR"), "/var/lib/ccw-console/logs"),
		NodeSrcPath:         or(getenv("CCW_NODE_SRC"), "/node-src.tar.gz"),
	}
	if c.DatabaseURL == "" {
		return ConsoleConfig{}, fmt.Errorf("config: CCW_CONSOLE_DATABASE_URL is required; no default is provided")
	}
	if c.DistDir == "" {
		return ConsoleConfig{}, fmt.Errorf("config: CCW_DIST_DIR is required; no default is provided")
	}
	// 提供了就必须合法：写错的密钥比没配更危险（会以为后台开着，实际起不来）。
	if raw := getenv("CCW_SECRET_KEY"); raw != "" {
		key, err := secretbox.ParseKeyHex(raw)
		if err != nil {
			return ConsoleConfig{}, err
		}
		c.SecretKey = key
	}
	return c, nil
}

// AdminEnabled：只有同时具备主密钥与白名单时才启用管理后台。
//
// **白名单是必需的**（不是"可选加固"）：管理后台权限等同全机队root，
// 允许在无网络层限制的情况下暴露登录页，等于把唯一的门槛压在密码+TOTP上。
func (c ConsoleConfig) AdminEnabled() bool {
	return len(c.SecretKey) > 0 && c.AdminAllowlist != ""
}
