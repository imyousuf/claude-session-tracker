package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

// TestPushEventReachesDaemon spins up a real Daemon and verifies that PushEvent
// over the per-user socket lands an event.
func TestPushEventReachesDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("HOME", dir)

	fw := &fakeWezterm{response: []byte("[]")}
	restore := wezterm.SetRunner(fw)
	defer restore()

	d, err := New(Options{
		SocketPath:     SocketPath(),
		WezStorePath:   filepath.Join(dir, "wezterm.db"),
		CoalesceWindow: 50 * time.Millisecond,
		IdleTimeout:    5 * time.Second,
		Logger:         log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Serve(ctx) }()

	// Wait for socket.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(SocketPath()); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	err = PushEvent(Event{
		Type: EventPreexec, MuxSocket: "/run/mux", PaneID: 1,
		Command: "claude", CWD: "/tmp", Timestamp: 100,
	})
	if err != nil {
		t.Fatalf("PushEvent: %v", err)
	}

	// Verify it was applied.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		s, ok, _ := d.w.LookupPaneRuntime("/run/mux", 1)
		if ok && s.CurrentCommand == "claude" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("event not applied within 500ms")
}

// TestPushEventAutoSpawnsWhenDaemonDown verifies that when the socket isn't
// reachable, PushEvent tries to auto-spawn a daemon. We inject a fake spawn
// function that records the call and returns success without actually exec'ing.
func TestPushEventAutoSpawnsWhenDaemonDown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("HOME", dir)

	// Inject a fake spawn that just records the call. PushEvent will still
	// dial twice and fail both times, then fall back to inline.
	var spawned atomic.Int32
	prev := AutoSpawnDaemonFunc
	AutoSpawnDaemonFunc = func() error {
		spawned.Add(1)
		return nil
	}
	defer func() { AutoSpawnDaemonFunc = prev }()

	// Stub wezterm so inlineFallback's snapshot path works.
	fw := &fakeWezterm{response: []byte("[]")}
	restore := wezterm.SetRunner(fw)
	defer restore()

	// Make dial fast so the test isn't slow.
	prevDial := DialTimeout
	prevWait := SpawnRetryWait
	DialTimeout = 50 * time.Millisecond
	SpawnRetryWait = 20 * time.Millisecond
	defer func() {
		DialTimeout = prevDial
		SpawnRetryWait = prevWait
	}()

	// Use the snapshot DB path the inline fallback opens.
	t.Setenv("HOME", dir) // ~/.cst/wezterm.db will be under temp dir.

	err := PushEvent(Event{Type: EventSnapshotRequest})
	if err != nil {
		t.Fatalf("PushEvent inline fallback: %v", err)
	}
	if spawned.Load() != 1 {
		t.Errorf("expected 1 spawn attempt, got %d", spawned.Load())
	}

	// Inline fallback should have created wezterm.db and written meta.
	dbPath := filepath.Join(dir, ".cst", "wezterm.db")
	w, err := store.OpenWezReadOnly(dbPath)
	if err != nil {
		t.Fatalf("inline fallback didn't create wezterm.db: %v", err)
	}
	defer func() { _ = w.Close() }()
	_, ok, _ := w.GetSnapshotMeta()
	if !ok {
		t.Error("inline fallback didn't write snapshot_meta")
	}
}

func TestIsDaemonReachable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	if IsDaemonReachable() {
		t.Error("should report unreachable when no daemon")
	}
}

func TestInlineFallbackPreexec(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	err := inlineFallback(Event{
		Type: EventPreexec, MuxSocket: "/run/mux", PaneID: 3,
		Command: "cmd", CWD: "/tmp", Timestamp: 1,
	}, nil)
	if err != nil {
		t.Fatalf("inlineFallback: %v", err)
	}

	dbPath := filepath.Join(dir, ".cst", "wezterm.db")
	w, err := store.OpenWezReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = w.Close() }()
	s, ok, _ := w.LookupPaneRuntime("/run/mux", 3)
	if !ok || s.CurrentCommand != "cmd" {
		t.Errorf("preexec not applied via inline: %+v ok=%v", s, ok)
	}
}
