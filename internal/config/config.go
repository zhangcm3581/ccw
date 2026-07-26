package config

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"ccw/internal/usage"
)

type Config struct {
	DatabaseURL     string
	TokenSigningKey []byte
	WorkspaceRoot   string
	ListenAddr      string
	AgentListenAddr string
	AdminSocketPath string
	ClaudeImage     string

	// 用量采集配置：只有worker-agent需要，因此不在Load里强制，
	// 由worker-agent启动时调RequireUsage二次校验（见该方法的说明）。
	UsageRoot    string        // 会话JSONL的挂载根，采集目录为 <UsageRoot>/<slug>
	UsageWeights usage.Weights // 四种token折算成内部额度单位的权重
}

func Load(getenv func(string) string) (Config, error) {
	c := Config{
		DatabaseURL:     getenv("CCW_DATABASE_URL"),
		WorkspaceRoot:   getenv("CCW_WORKSPACE_ROOT"),
		ListenAddr:      or(getenv("CCW_LISTEN_ADDR"), "127.0.0.1:8080"),
		AgentListenAddr: or(getenv("CCW_AGENT_LISTEN_ADDR"), "127.0.0.1:8081"),
		AdminSocketPath: or(getenv("CCW_ADMIN_SOCKET"), "/run/ccw/admin.sock"),
		ClaudeImage:     or(getenv("CCW_CLAUDE_IMAGE"), "ccw-claude:latest"),
	}
	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: CCW_DATABASE_URL is required")
	}
	if c.WorkspaceRoot == "" {
		return Config{}, fmt.Errorf("config: CCW_WORKSPACE_ROOT is required")
	}
	keyHex := getenv("CCW_TOKEN_KEY")
	if keyHex == "" {
		return Config{}, fmt.Errorf("config: CCW_TOKEN_KEY is required (hex, >=32 bytes); no default is provided")
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) < 32 {
		return Config{}, fmt.Errorf("config: CCW_TOKEN_KEY must be hex-encoded and at least 32 bytes")
	}
	c.TokenSigningKey = key

	c.UsageRoot = getenv("CCW_USAGE_ROOT")
	if raw := getenv("CCW_USAGE_WEIGHTS"); raw != "" {
		w, werr := parseWeights(raw)
		if werr != nil {
			return Config{}, werr
		}
		c.UsageWeights = w
	}
	return c, nil
}

// parseWeights解析"input,output,cache_read,cache_write"。
//
// 严格解析、不做宽松容错：把"5x"当成0会让一整类token静默不计量，
// 而这种错误在运行时唯一的表现就是"用量偏低"——没人会察觉。
func parseWeights(raw string) (usage.Weights, error) {
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return usage.Weights{}, fmt.Errorf(
			"config: CCW_USAGE_WEIGHTS must be 4 comma-separated integers (input,output,cache_read,cache_write), got %d", len(parts))
	}
	var v [4]int64
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return usage.Weights{}, fmt.Errorf("config: CCW_USAGE_WEIGHTS field %d is not an integer: %q", i+1, p)
		}
		if n < 0 {
			return usage.Weights{}, fmt.Errorf("config: CCW_USAGE_WEIGHTS field %d must be >= 0, got %d", i+1, n)
		}
		v[i] = n
	}
	return usage.Weights{Input: v[0], Output: v[1], CacheRead: v[2], CacheWrite: v[3]}, nil
}

// RequireUsage是worker-agent专用的二次校验：采集配置缺失即拒绝启动。
//
// 为什么不放进Load：control-api与cclaude也调Load，它们不采集用量，
// 强制这两个变量会让它们无谓地失败。
//
// 为什么必须硬失败：带着空UsageRoot启动会扫一个不存在的目录，带着零权重启动会
// 把所有用量都算成0——两种情况下采集器都"在正常运行"，日志无异常，
// usage_events却永远为空。这正是接线前的现象，必须在启动时就挡住。
func (c Config) RequireUsage() error {
	if c.UsageRoot == "" {
		return fmt.Errorf("config: CCW_USAGE_ROOT is required for worker-agent; no default is provided")
	}
	w := c.UsageWeights
	if w.Input == 0 && w.Output == 0 && w.CacheRead == 0 && w.CacheWrite == 0 {
		return fmt.Errorf("config: CCW_USAGE_WEIGHTS is required and must not be all zero " +
			"(all-zero weights record every event as 0 units, which silently disables the quota gate)")
	}
	return nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
