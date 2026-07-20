package config

import (
	"encoding/hex"
	"fmt"
)

type Config struct {
	DatabaseURL     string
	TokenSigningKey []byte
	WorkspaceRoot   string
	ListenAddr      string
	AgentListenAddr string
	AdminSocketPath string
	ClaudeImage     string
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
	return c, nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
