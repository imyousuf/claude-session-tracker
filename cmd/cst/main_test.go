package main

import (
	"fmt"
	"testing"

	"github.com/imyousuf/claude-session-tracker/internal/snapshot"
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
