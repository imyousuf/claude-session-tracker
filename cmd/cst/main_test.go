package main

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/imyousuf/claude-session-tracker/internal/config"
	"github.com/imyousuf/claude-session-tracker/internal/snapshot"
	"github.com/imyousuf/claude-session-tracker/internal/store"
)

func TestRestoreExitError(t *testing.T) {
	// Clean run → nil.
	if err := restoreExitError(snapshot.RestoreResult{WindowsSpawned: 2}, nil); err != nil {
		t.Errorf("clean run: got %v, want nil", err)
	}

	// Hard error passes through.
	hard := fmt.Errorf("open db: boom")
	if err := restoreExitError(snapshot.RestoreResult{}, hard); err != hard {
		t.Errorf("hard error: got %v, want %v", err, hard)
	}

	// Per-pane errors → non-nil so the process exits non-zero.
	res := snapshot.RestoreResult{
		WindowsSpawned: 1,
		Errors:         []error{fmt.Errorf("split failed"), fmt.Errorf("send-text failed")},
	}
	if err := restoreExitError(res, nil); err == nil {
		t.Error("per-pane errors: got nil, want non-nil")
	}
}

func TestBuildResumeCommandByProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		cfg      config.Config
		wantBin  string
		wantArgs []string
	}{
		{
			name:     "claude",
			provider: store.ProviderClaude,
			cfg:      config.Config{DangerouslySkipPermissions: true},
			wantBin:  "claude",
			wantArgs: []string{"claude", "--resume", "session-1", "--dangerously-skip-permissions", "--verbose"},
		},
		{
			name:     "codex",
			provider: store.ProviderCodex,
			cfg:      config.Config{DangerouslySkipPermissions: true},
			wantBin:  "codex",
			wantArgs: []string{"codex", "resume", "session-1", "--verbose"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin, args, err := buildResumeCommand("session-1", tc.provider, tc.cfg, []string{"--verbose"})
			if err != nil {
				t.Fatal(err)
			}
			if bin != tc.wantBin || !reflect.DeepEqual(args, tc.wantArgs) {
				t.Fatalf("got %q %v, want %q %v", bin, args, tc.wantBin, tc.wantArgs)
			}
		})
	}
}
