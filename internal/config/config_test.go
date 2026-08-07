package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissing(t *testing.T) {
	cfg, err := Load("/nonexistent/config.json")
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if cfg.DangerouslySkipPermissions {
		t.Error("expected false for missing config")
	}
	if len(cfg.ExtraArgs) != 0 {
		t.Error("expected empty extra args for missing config")
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := Config{
		DangerouslySkipPermissions: true,
		ExtraArgs:                  []string{"--verbose", "--model", "opus"},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.DangerouslySkipPermissions {
		t.Error("DangerouslySkipPermissions = false, want true")
	}
	if len(loaded.ExtraArgs) != 3 {
		t.Fatalf("ExtraArgs len = %d, want 3", len(loaded.ExtraArgs))
	}
	if loaded.ExtraArgs[0] != "--verbose" {
		t.Errorf("ExtraArgs[0] = %q, want %q", loaded.ExtraArgs[0], "--verbose")
	}
}

func TestSaveCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "config.json")

	if err := Save(path, Config{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "dir")); err != nil {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestDefaultsHasSupportedAgentCommands(t *testing.T) {
	d := Defaults()
	want := []string{"claude", "codex", "sosuke", "tomoe"}
	if len(d.ReplayCommands) != len(want) {
		t.Fatalf("expected %d OOTB commands, got %d", len(want), len(d.ReplayCommands))
	}
	for i := range want {
		if d.ReplayCommands[i] != want[i] {
			t.Errorf("ReplayCommands[%d] = %q, want %q", i, d.ReplayCommands[i], want[i])
		}
	}
	if t.Failed() {
		t.Errorf("OOTB list = %v", d.ReplayCommands)
	}
}

func TestWithDefaultsFillsNil(t *testing.T) {
	c := Config{} // ReplayCommands is nil
	got := c.WithDefaults()
	if len(got.ReplayCommands) != 4 {
		t.Errorf("WithDefaults didn't fill nil: %v", got.ReplayCommands)
	}
}

func TestWithDefaultsPreservesExplicitEmpty(t *testing.T) {
	c := Config{ReplayCommands: []string{}}
	got := c.WithDefaults()
	if got.ReplayCommands == nil || len(got.ReplayCommands) != 0 {
		t.Errorf("explicit [] was overridden: %v", got.ReplayCommands)
	}
}

func TestWithDefaultsPreservesUserList(t *testing.T) {
	c := Config{ReplayCommands: []string{"foo", "bar"}}
	got := c.WithDefaults()
	if len(got.ReplayCommands) != 2 || got.ReplayCommands[0] != "foo" {
		t.Errorf("user list was overridden: %v", got.ReplayCommands)
	}
}

func TestAddReplayCommand(t *testing.T) {
	c := Defaults()
	if added := c.AddReplayCommand("tomoe"); added {
		t.Error("expected false when adding duplicate")
	}
	if added := c.AddReplayCommand("ssh"); !added {
		t.Error("expected true when adding new")
	}
	if len(c.ReplayCommands) != 5 {
		t.Fatalf("len = %d", len(c.ReplayCommands))
	}
	if c.ReplayCommands[4] != "ssh" {
		t.Errorf("last = %q", c.ReplayCommands[4])
	}
}

func TestRemoveReplayCommand(t *testing.T) {
	c := Defaults()
	if removed := c.RemoveReplayCommand("tomoe"); !removed {
		t.Error("expected true when removing existing")
	}
	if len(c.ReplayCommands) != 3 || c.ReplayCommands[0] != "claude" {
		t.Errorf("after remove: %v", c.ReplayCommands)
	}
	if removed := c.RemoveReplayCommand("nope"); removed {
		t.Error("expected false when removing missing")
	}

	// Remove the remaining entries → list becomes empty (not nil).
	c.RemoveReplayCommand("claude")
	c.RemoveReplayCommand("codex")
	c.RemoveReplayCommand("sosuke")
	if c.ReplayCommands == nil {
		t.Error("ReplayCommands should be empty slice, not nil, after last removal")
	}
	if len(c.ReplayCommands) != 0 {
		t.Errorf("len after last remove = %d", len(c.ReplayCommands))
	}
}

func TestSaveLoadReplayCommandsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := Defaults()
	cfg.AddReplayCommand("ssh")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.ReplayCommands) != 5 {
		t.Fatalf("len = %d", len(loaded.ReplayCommands))
	}
}

func TestClaudeArgs(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "empty config",
			cfg:  Config{},
			want: nil,
		},
		{
			name: "skip permissions only",
			cfg:  Config{DangerouslySkipPermissions: true},
			want: []string{"--dangerously-skip-permissions"},
		},
		{
			name: "extra args only",
			cfg:  Config{ExtraArgs: []string{"--verbose"}},
			want: []string{"--verbose"},
		},
		{
			name: "both",
			cfg: Config{
				DangerouslySkipPermissions: true,
				ExtraArgs:                  []string{"--model", "opus"},
			},
			want: []string{"--dangerously-skip-permissions", "--model", "opus"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.ClaudeArgs()
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
