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
