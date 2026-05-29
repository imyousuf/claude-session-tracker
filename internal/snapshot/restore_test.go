package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

// fakeSpawner records every Spawner call for assertion.
type fakeSpawner struct {
	calls           []spawnCall
	nextPaneID      int64
	windowsByPaneID map[int64]int64
}

type spawnCall struct {
	Kind     string // "new-window" | "tab" | "split" | "lookup"
	WindowID int64
	CWD      string
	Command  []string
	PaneID   int64 // for "lookup" and "split" (the pane being split off)
}

func newFakeSpawner() *fakeSpawner {
	return &fakeSpawner{
		nextPaneID:      100,
		windowsByPaneID: map[int64]int64{},
	}
}

func (f *fakeSpawner) SpawnNewWindow(cwd string, cmd []string) (int64, error) {
	pid := f.nextPaneID
	f.nextPaneID++
	wid := int64(1000 + len(f.calls))
	f.windowsByPaneID[pid] = wid
	f.calls = append(f.calls, spawnCall{Kind: "new-window", CWD: cwd, Command: cmd, PaneID: pid})
	return pid, nil
}

func (f *fakeSpawner) SpawnTabInWindow(windowID int64, cwd string, cmd []string) (int64, error) {
	pid := f.nextPaneID
	f.nextPaneID++
	f.calls = append(f.calls, spawnCall{Kind: "tab", WindowID: windowID, CWD: cwd, Command: cmd, PaneID: pid})
	return pid, nil
}

func (f *fakeSpawner) SplitPane(paneID int64, cwd string, cmd []string) (int64, error) {
	pid := f.nextPaneID
	f.nextPaneID++
	f.calls = append(f.calls, spawnCall{Kind: "split", CWD: cwd, Command: cmd, PaneID: paneID})
	return pid, nil
}

func (f *fakeSpawner) LookupWindowForPane(paneID int64) (int64, error) {
	f.calls = append(f.calls, spawnCall{Kind: "lookup", PaneID: paneID})
	wid, ok := f.windowsByPaneID[paneID]
	if !ok {
		return 0, fmt.Errorf("unknown pane %d", paneID)
	}
	return wid, nil
}

// seedSnapshot prepares a wezterm.db with one snapshot for restore tests.
func seedSnapshot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	wezPath := filepath.Join(dir, "wezterm.db")
	w, err := store.OpenWez(wezPath)
	if err != nil {
		t.Fatalf("OpenWez: %v", err)
	}

	// Seed: 2 windows.
	// Window 0: 1 tab with 1 pane (claude, session "abc")
	// Window 1: 2 tabs: tab a has 1 pane (tomoe), tab b has 2 panes (one bash, one vim)
	panes := []wezterm.RawPane{
		{WindowID: 0, TabID: 1, PaneID: 1, Workspace: "default",
			CWD: "file://host/proj/foo/", Size: wezterm.Size{Cols: 100, Rows: 30}},
		{WindowID: 1, TabID: 10, PaneID: 10, Workspace: "default",
			CWD: "file://host/proj/audio/", Size: wezterm.Size{Cols: 100, Rows: 30}},
		{WindowID: 1, TabID: 11, PaneID: 11, Workspace: "default",
			CWD: "file://host/proj/edit/", Size: wezterm.Size{Cols: 100, Rows: 30}},
		{WindowID: 1, TabID: 11, PaneID: 12, Workspace: "default",
			CWD: "file://host/proj/edit/", Size: wezterm.Size{Cols: 50, Rows: 30}},
	}

	// Seed pane_runtime_state for panes 1, 10, 12.
	mux := "/run/mux"
	_ = w.ApplyPreexec(mux, 1, "claude", "/proj/foo", 1)
	_ = w.BindClaudeSession(mux, 1, "abc", "/proj/foo")
	_ = w.ApplyPreexec(mux, 10, "tomoe start", "/proj/audio", 1)
	_ = w.ApplyPreexec(mux, 12, "vim foo.go", "/proj/edit", 1)
	// pane 11 has no runtime state — should get plain shell.

	rawJSON := []byte(`fake-raw`)
	if _, err := Sync(w, rawJSON, panes, mux, time.UnixMilli(1000)); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = w.Close()
	return wezPath
}

func TestRestoreDryRun(t *testing.T) {
	wezPath := seedSnapshot(t)

	var buf bytes.Buffer
	res, err := Restore(context.Background(), RestoreOptions{
		WezDBPath: wezPath,
		DryRun:    true,
		Out:       &buf,
		Replay:    []string{"claude", "tomoe"},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.WindowsSpawned != 2 {
		t.Errorf("windows = %d, want 2", res.WindowsSpawned)
	}
	if res.TabsSpawned != 1 {
		t.Errorf("tabs = %d, want 1 (window 1's second tab)", res.TabsSpawned)
	}
	if res.PanesSplit != 1 {
		t.Errorf("splits = %d, want 1 (the second pane of the two-pane tab)", res.PanesSplit)
	}

	// Output should mention claude --resume abc and tomoe start.
	out := buf.String()
	if !contains(out, "claude --resume abc") {
		t.Errorf("dry-run output missing claude --resume:\n%s", out)
	}
	if !contains(out, "exec tomoe start") {
		t.Errorf("dry-run output missing tomoe start:\n%s", out)
	}
}

func TestRestoreExecutesPlannedSpawns(t *testing.T) {
	wezPath := seedSnapshot(t)
	f := newFakeSpawner()

	res, err := Restore(context.Background(), RestoreOptions{
		WezDBPath: wezPath,
		Spawner:   f,
		Replay:    []string{"claude", "tomoe"},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.WindowsSpawned != 2 || res.TabsSpawned != 1 || res.PanesSplit != 1 {
		t.Errorf("res = %+v", res)
	}

	// First call should be SpawnNewWindow for window 0's pane (claude).
	if f.calls[0].Kind != "new-window" {
		t.Fatalf("first call kind = %q", f.calls[0].Kind)
	}
	if !reflect.DeepEqual(f.calls[0].Command, []string{"claude", "--resume", "abc"}) {
		t.Errorf("first command = %v", f.calls[0].Command)
	}
}

func TestRestoreSkipFirst(t *testing.T) {
	wezPath := seedSnapshot(t)
	f := newFakeSpawner()

	res, err := Restore(context.Background(), RestoreOptions{
		WezDBPath: wezPath,
		SkipFirst: true,
		Spawner:   f,
		Replay:    []string{"claude", "tomoe"},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.PanesSkipped != 1 {
		t.Errorf("PanesSkipped = %d", res.PanesSkipped)
	}
	// With SkipFirst, window 0 was supposed to have 1 pane (skipped),
	// then window 1's first pane spawns a new window.
	if res.WindowsSpawned != 1 {
		t.Errorf("windows = %d, want 1", res.WindowsSpawned)
	}

	// First Spawner call should be SpawnNewWindow for window 1's first pane.
	if f.calls[0].Kind != "new-window" {
		t.Fatalf("first call kind = %q", f.calls[0].Kind)
	}
	if !contains(f.calls[0].Command[2], "tomoe start") {
		t.Errorf("first command = %v, want tomoe start replay", f.calls[0].Command)
	}
}

func TestRestoreUnknownCommandGetsPlainShell(t *testing.T) {
	wezPath := seedSnapshot(t)
	f := newFakeSpawner()

	// Replay registry intentionally excludes "vim".
	_, err := Restore(context.Background(), RestoreOptions{
		WezDBPath: wezPath,
		Spawner:   f,
		Replay:    []string{"claude", "tomoe"},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Pane 12 (vim) is the second pane of window 1's two-pane tab, so it is
	// restored as a split off the tab's lead pane. vim isn't in the registry,
	// so it should get a plain shell (empty command).
	var vimCall *spawnCall
	for i := range f.calls {
		if f.calls[i].Kind == "split" && f.calls[i].CWD == "/proj/edit" {
			vimCall = &f.calls[i]
		}
	}
	if vimCall == nil {
		t.Fatal("no split for vim pane found")
	}
	if len(vimCall.Command) != 0 {
		t.Errorf("vim pane should get plain shell, got command = %v", vimCall.Command)
	}
}

func TestRestoreMissingDBIsNotAnError(t *testing.T) {
	var buf bytes.Buffer
	res, err := Restore(context.Background(), RestoreOptions{
		WezDBPath: "/nonexistent/wezterm.db",
		Out:       &buf,
		Replay:    []string{"claude"}, // avoid loading the real user config
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.WindowsSpawned != 0 {
		t.Errorf("should have spawned nothing, got %d", res.WindowsSpawned)
	}
}

func TestRestoreSpawnIfEmptyOnMissingDB(t *testing.T) {
	f := newFakeSpawner()
	var buf bytes.Buffer
	res, err := Restore(context.Background(), RestoreOptions{
		WezDBPath:    "/nonexistent/wezterm.db",
		SpawnIfEmpty: true,
		Spawner:      f,
		Out:          &buf,
		Replay:       []string{"claude"},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.WindowsSpawned != 1 {
		t.Errorf("SpawnIfEmpty should open one fallback window, got %d", res.WindowsSpawned)
	}
	if len(f.calls) != 1 || f.calls[0].Kind != "new-window" {
		t.Errorf("expected a single new-window fallback call, got %+v", f.calls)
	}
}

func TestRestoreSpawnIfEmptyNoopWhenLayoutExists(t *testing.T) {
	wezPath := seedSnapshot(t)
	f := newFakeSpawner()
	res, err := Restore(context.Background(), RestoreOptions{
		WezDBPath:    wezPath,
		SpawnIfEmpty: true,
		Spawner:      f,
		Replay:       []string{"claude", "tomoe"},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// Two real windows restored; the fallback must NOT add a third.
	if res.WindowsSpawned != 2 {
		t.Errorf("windows = %d, want 2 (no spurious fallback window)", res.WindowsSpawned)
	}
}

func TestRestoreWorkspaceFilter(t *testing.T) {
	wezPath := seedSnapshot(t)
	f := newFakeSpawner()

	// Only restore "other" workspace — fixture has none, expect zero spawns.
	res, err := Restore(context.Background(), RestoreOptions{
		WezDBPath: wezPath,
		Spawner:   f,
		Workspace: "other",
		Replay:    []string{"claude", "tomoe"},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.WindowsSpawned != 0 || res.TabsSpawned != 0 {
		t.Errorf("workspace filter not applied: %+v", res)
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
