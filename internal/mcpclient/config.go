package mcpclient

import (
	"encoding/json"
	"fmt"
	"os"
)

// ServerConfig mirrors the Claude CLI MCP config shape that claude-agent
// already generates — anyrun reuses the file verbatim.
type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`     // remote servers (http/sse)
	Type    string            `json:"type"`    // "http" | "sse" | "" (stdio when Command set, http when URL set)
	Headers map[string]string `json:"headers"` // static headers for remote servers
}

type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("mcpclient: read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("mcpclient: parse config: %w", err)
	}
	return cfg, nil
}
