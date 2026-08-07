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

	// ReplayCommands is the allow-list of command names whose last invocation
	// `cst restore` will re-launch in each pane that was running them. The
	// captured full command line (including args) is replayed verbatim.
	// Linked Claude and Codex sessions are special cases: restore uses
	// `claude --resume <id>` or `codex resume <id>` instead of the literal
	// capture. Commands without lifecycle session links are replayed literally.
	//
	// OOTB defaults: "claude", "codex", "sosuke", "tomoe". Users can add more with
	// `cst config replay-add <name>` and remove with `replay-remove <name>`.
	ReplayCommands []string `json:"replay_commands,omitempty"`
}

// Defaults returns the OOTB Config — used to seed a fresh ~/.cst/config.json
// and to merge missing values into an existing config.
func Defaults() Config {
	return Config{
		ReplayCommands: []string{"claude", "codex", "sosuke", "tomoe"},
	}
}

// WithDefaults returns a copy of c with any unset list-style fields filled in
// from Defaults(). Specifically, if ReplayCommands is nil it gets the defaults;
// if it's an empty slice (user explicitly set [] to opt out) it stays empty.
func (c Config) WithDefaults() Config {
	if c.ReplayCommands == nil {
		c.ReplayCommands = Defaults().ReplayCommands
	}
	return c
}

// AddReplayCommand appends name to ReplayCommands if not already present.
// Returns true if added, false if it was already in the list.
func (c *Config) AddReplayCommand(name string) bool {
	for _, n := range c.ReplayCommands {
		if n == name {
			return false
		}
	}
	c.ReplayCommands = append(c.ReplayCommands, name)
	return true
}

// RemoveReplayCommand drops name from ReplayCommands. Returns true if present.
// To distinguish "user explicitly opted out of OOTB defaults" from "user has
// never touched the list" downstream, this method writes an empty slice (not nil)
// when the last entry is removed.
func (c *Config) RemoveReplayCommand(name string) bool {
	out := make([]string, 0, len(c.ReplayCommands))
	removed := false
	for _, n := range c.ReplayCommands {
		if n == name {
			removed = true
			continue
		}
		out = append(out, n)
	}
	if removed {
		c.ReplayCommands = out
	}
	return removed
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
