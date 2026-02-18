package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	DefaultConfigDir  = ".cst"
	DefaultConfigName = "config.json"
)

// Config holds CST user preferences stored in ~/.cst/config.json.
type Config struct {
	// DangerouslySkipPermissions adds --dangerously-skip-permissions to claude resume commands.
	DangerouslySkipPermissions bool `json:"dangerously_skip_permissions,omitempty"`

	// ExtraArgs are additional arguments always passed to the claude CLI on resume.
	ExtraArgs []string `json:"extra_args,omitempty"`
}

// DefaultConfigPath returns the path to ~/.cst/config.json.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, DefaultConfigDir, DefaultConfigName)
}

// Load reads the config from the given path. Returns a zero Config if the file doesn't exist.
func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// Save writes the config to the given path, creating the directory if needed.
func Save(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

// ClaudeArgs returns the full list of extra arguments to pass to claude on resume.
func (c Config) ClaudeArgs() []string {
	var args []string
	if c.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, c.ExtraArgs...)
	return args
}
