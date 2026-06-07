package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

// fakeSpawner records every Spawner call for assertion. Under skeleton-first
// restore, spawns/splits carry NO command (plain shells); commands arrive later
// as "send-text" calls.
type fakeSpawner struct {
	calls           []spawnCall
	nextPaneID      int64
	windowsByPaneID map[int64]int64
	issued          map[int64]bool // pane ids handed out (for PaneExists)

	// existsAfter, if >0, makes PaneExists return false for the first N checks of
	// each pane id, then true — to exercise the readiness poll.
	existsAfter  int
	existsChecks map[int64]int

	splitErr bool // SplitPaneDir returns an error when true
}

type spawnCall struct {
	Kind      string // "new-window" | "tab" | "split" | "send-text" | "lookup"
	WindowID  int64
	CWD       string
	Command   []string
	Direction wezterm.SplitDirection // for "split"
	Text      string                 // for "send-text"
	PaneID    int64                  // "lookup"/"split" parent, or "send-text" target
}

func newFakeSpawner() *fakeSpawner {
	return &fakeSpawner{
		nextPaneID:      100,
		windowsByPaneID: map[int64]int64{},
		issued:          map[int64]bool{},
		existsChecks:    map[int64]int{},
	}
}

func (f *fakeSpawner) issue() int64 {
	pid := f.nextPaneID
	f.nextPaneID++
	f.issued[pid] = true
	return pid
}

func (f *fakeSpawner) SpawnNewWindow(cwd string, cmd []string) (int64, error) {
	pid := f.issue()
	wid := int64(1000 + len(f.calls))
	f.windowsByPaneID[pid] = wid
	f.calls = append(f.calls, spawnCall{Kind: "new-window", CWD: cwd, Command: cmd, PaneID: pid})
	return pid, nil
}

func (f *fakeSpawner) SpawnTabInWindow(windowID int64, cwd string, cmd []string) (int64, error) {
	pid := f.issue()
	f.calls = append(f.calls, spawnCall{Kind: "tab", WindowID: windowID, CWD: cwd, Command: cmd, PaneID: pid})
	return pid, nil
}

func (f *fakeSpawner) SplitPaneDir(parentPaneID int64, dir wezterm.SplitDirection, cwd string) (int64, error) {
	if f.splitErr {
		return 0, fmt.Errorf("split boom")
	}
	pid := f.issue()
	f.calls = append(f.calls, spawnCall{Kind: "split", CWD: cwd, Direction: dir, PaneID: parentPaneID})
	return pid, nil
}

func (f *fakeSpawner) SendText(paneID int64, text string) error {
	f.calls = append(f.calls, spawnCall{Kind: "send-text", PaneID: paneID, Text: text})
	return nil
}

func (f *fakeSpawner) PaneExists(paneID int64) (bool, error) {
	if f.existsAfter > 0 {
		f.existsChecks[paneID]++
		if f.existsChecks[paneID] <= f.existsAfter {
			return false, nil
		}
	}
	return f.issued[paneID], nil
}

func (f *fakeSpawner) LookupWindowForPane(paneID int64) (int64, error) {
	f.calls = append(f.calls, spawnCall{Kind: "lookup", PaneID: paneID})
	wid, ok := f.windowsByPaneID[paneID]
	if !ok {
		return 0, fmt.Errorf("unknown pane %d", paneID)
	}
	return wid, nil
}

// fast restore options for tests (tiny poll timeout).
func testOpts(o RestoreOptions) RestoreOptions {
	o.PaneReadyTimeout = 200 * time.Millisecond
	return o
}

// seedSnapshot prepares a wezterm.db with one snapshot for restore tests.
//
//	Window 0: 1 tab, 1 pane (claude, session "abc")
//	Window 1: tab a (tomoe), tab b (2 panes: bash lead + vim split)
func seedSnapshot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	wezPath := filepath.Join(dir, "wezterm.db")
	w, err := store.OpenWez(wezPath)
	if err != nil {
		t.Fatalf("OpenWez: %v", err)
	}

	panes := []wezterm.RawPane{
		{WindowID: 0, TabID: 1, PaneID: 1, Workspace: "default",
			CWD: "file://host/proj/foo/", Size: wezterm.Size{Cols: 100, Rows: 30}},
		{WindowID: 1, TabID: 10, PaneID: 10, Workspace: "default",
			CWD: "file://host/proj/audio/", Size: wezterm.Size{Cols: 100, Rows: 30}},
		{WindowID: 1, TabID: 11, PaneID: 11, Workspace: "default",
			LeftCol: 0, TopRow: 0, CWD: "file://host/proj/edit/", Size: wezterm.Size{Cols: 100, Rows: 30}},
		{WindowID: 1, TabID: 11, PaneID: 12, Workspace: "default",
			LeftCol: 101, TopRow: 0, CWD: "file://host/proj/edit/", Size: wezterm.Size{Cols: 100, Rows: 30}},
	}

	mux := "/run/mux"
	_ = w.ApplyPreexec(mux, 1, "claude", "/proj/foo", 1)
	_ = w.BindClaudeSession(mux, 1, "abc", "/proj/foo")
	_ = w.ApplyPreexec(mux, 10, "tomoe start", "/proj/audio", 1)
	_ = w.ApplyPreexec(mux, 12, "vim foo.go", "/proj/edit", 1)
	// pane 11 has no runtime state — plain shell.

	if _, err := Sync(w, []byte(`fake-raw`), panes, mux, time.UnixMilli(1000)); err != nil {
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
		t.Errorf("splits = %d, want 1 (the vim pane)", res.PanesSplit)
	}
	if res.CommandsSent != 2 {
		t.Errorf("commands = %d, want 2 (claude + tomoe)", res.CommandsSent)
	}

	out := buf.String()
	// send-text lines carry the verbatim command (no sh -c "exec" wrapper).
	if !contains(out, "send-text") || !contains(out, "claude --resume abc") {
		t.Errorf("dry-run output missing claude send-text:\n%s", out)
	}
	if !contains(out, "tomoe start") {
		t.Errorf("dry-run output missing tomoe send-text:\n%s", out)
	}
}

func TestRestoreSkeletonThenSendText(t *testing.T) {
	wezPath := seedSnapshot(t)
	f := newFakeSpawner()

	res, err := Restore(context.Background(), testOpts(RestoreOptions{
		WezDBPath: wezPath,
		Spawner:   f,
		Replay:    []string{"claude", "tomoe"},
	}))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.WindowsSpawned != 2 || res.TabsSpawned != 1 || res.PanesSplit != 1 {
		t.Errorf("res = %+v", res)
	}
	if res.CommandsSent != 2 {
		t.Errorf("CommandsSent = %d, want 2", res.CommandsSent)
	}

	// Skeleton phase: every new-window/tab/split must carry NO command.
	lastSkeleton := -1
	firstSendText := -1
	for i, c := range f.calls {
		switch c.Kind {
		case "new-window", "tab", "split":
			if len(c.Command) != 0 {
				t.Errorf("skeleton call %d (%s) has command %v, want none", i, c.Kind, c.Command)
			}
			lastSkeleton = i
		case "send-text":
			if firstSendText < 0 {
				firstSendText = i
			}
		}
	}
	// All skeleton calls precede all send-text calls.
	if firstSendText >= 0 && firstSendText < lastSkeleton {
		t.Errorf("send-text (idx %d) interleaved before skeleton done (idx %d)", firstSendText, lastSkeleton)
	}

	// First call opens a new window (claude pane's tab lead) as a plain shell.
	if f.calls[0].Kind != "new-window" || len(f.calls[0].Command) != 0 {
		t.Fatalf("first call = %+v, want plain new-window", f.calls[0])
	}

	// A send-text must carry the claude resume line and another the tomoe line.
	var sawClaude, sawTomoe bool
	for _, c := range f.calls {
		if c.Kind != "send-text" {
			continue
		}
		if c.Text == "claude --resume abc\r" {
			sawClaude = true
		}
		if c.Text == "tomoe start\r" {
			sawTomoe = true
		}
	}
	if !sawClaude {
		t.Error("missing send-text 'claude --resume abc\\r'")
	}
	if !sawTomoe {
		t.Error("missing send-text 'tomoe start\\r'")
	}
}

// TestRestoreTomoeSplitChain is the headline regression: a 3-pane tab
// (A full-height left, B top-right, C bottom-right) must rebuild as
// lead A → split B right off A → split C bottom off B, with claude/tomoe
// typed in afterwards.
func TestRestoreTomoeSplitChain(t *testing.T) {
	dir := t.TempDir()
	wezPath := filepath.Join(dir, "wezterm.db")
	w, err := store.OpenWez(wezPath)
	if err != nil {
		t.Fatalf("OpenWez: %v", err)
	}
	mux := "/run/mux"
	panes := []wezterm.RawPane{
		{WindowID: 1, TabID: 1, PaneID: 1, Workspace: "default",
			LeftCol: 0, TopRow: 0, Size: wezterm.Size{Cols: 122, Rows: 59}, CWD: "file://h/proj/tomoe/"},
		{WindowID: 1, TabID: 1, PaneID: 2, Workspace: "default",
			LeftCol: 123, TopRow: 0, Size: wezterm.Size{Cols: 124, Rows: 30}, CWD: "file://h/proj/tomoe/"},
		{WindowID: 1, TabID: 1, PaneID: 15, Workspace: "default",
			LeftCol: 123, TopRow: 31, Size: wezterm.Size{Cols: 124, Rows: 28}, CWD: "file://h/proj/tomoe/"},
	}
	_ = w.ApplyPreexec(mux, 1, "tomoe", "/proj/tomoe", 1)        // A: tomoe
	_ = w.ApplyPreexec(mux, 2, "cst", "/proj/tomoe", 1)          // B: claude via cst
	_ = w.BindClaudeSession(mux, 2, "sess-tomoe", "/proj/tomoe") // B linked
	// C (pane 15): no runtime → plain shell.
	if _, err := Sync(w, []byte("tomoe"), panes, mux, time.UnixMilli(1)); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = w.Close()

	f := newFakeSpawner()
	res, err := Restore(context.Background(), testOpts(RestoreOptions{
		WezDBPath: wezPath,
		Spawner:   f,
		Replay:    []string{"claude", "tomoe"},
	}))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.WindowsSpawned != 1 || res.PanesSplit != 2 {
		t.Fatalf("res = %+v, want 1 window / 2 splits", res)
	}

	// Identify skeleton calls in order.
	var newWin, splitB, splitC *spawnCall
	for i := range f.calls {
		c := &f.calls[i]
		switch c.Kind {
		case "new-window":
			newWin = c
		case "split":
			if splitB == nil {
				splitB = c
			} else {
				splitC = c
			}
		}
	}
	if newWin == nil || splitB == nil || splitC == nil {
		t.Fatalf("expected new-window + 2 splits, calls = %+v", f.calls)
	}
	leadID := newWin.PaneID
	// B splits RIGHT off the lead (A). The fake issues ids sequentially, so B's
	// new pane id is the next one after the lead.
	if splitB.PaneID != leadID || splitB.Direction != wezterm.SplitRight {
		t.Errorf("split B = parent %d dir %q, want parent %d dir right", splitB.PaneID, splitB.Direction, leadID)
	}
	bID := leadID + 1 // new-window=lead, first split (B)=lead+1
	// C splits BOTTOM off B (not off the full-height lead A).
	if splitC.PaneID != bID || splitC.Direction != wezterm.SplitBottom {
		t.Errorf("split C = parent %d dir %q, want parent %d (B) dir bottom", splitC.PaneID, splitC.Direction, bID)
	}

	// send-text: tomoe into A (lead), claude --resume into B; none into C.
	texts := map[int64]string{}
	for _, c := range f.calls {
		if c.Kind == "send-text" {
			texts[c.PaneID] = c.Text
		}
	}
	if texts[leadID] != "tomoe\r" {
		t.Errorf("lead send-text = %q, want tomoe", texts[leadID])
	}
	if texts[bID] != "claude --resume sess-tomoe\r" {
		t.Errorf("B send-text = %q, want claude --resume sess-tomoe", texts[bID])
	}
}

func TestRestoreReadinessPollWaitsForPane(t *testing.T) {
	wezPath := seedSnapshot(t)
	f := newFakeSpawner()
	f.existsAfter = 2 // PaneExists false twice per pane, then true

	res, err := Restore(context.Background(), testOpts(RestoreOptions{
		WezDBPath: wezPath,
		Spawner:   f,
		Replay:    []string{"claude", "tomoe"},
	}))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// Completes normally despite the delayed visibility.
	if res.WindowsSpawned != 2 || res.PanesSplit != 1 || res.CommandsSent != 2 {
		t.Errorf("res = %+v", res)
	}
}

func TestRestoreSurfacesSplitError(t *testing.T) {
	wezPath := seedSnapshot(t)
	f := newFakeSpawner()
	f.splitErr = true // every SplitPaneDir fails

	res, err := Restore(context.Background(), testOpts(RestoreOptions{
		WezDBPath: wezPath,
		Spawner:   f,
		Replay:    []string{"claude", "tomoe"},
	}))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected split error to be surfaced in res.Errors")
	}
	// The vim pane's split failed, so no send-text should target a vim pane
	// (vim isn't in the registry anyway → no command). claude+tomoe still sent.
	if res.CommandsSent != 2 {
		t.Errorf("CommandsSent = %d, want 2 (claude+tomoe still sent)", res.CommandsSent)
	}
}

func TestRestoreMissingDBIsNotAnError(t *testing.T) {
	var buf bytes.Buffer
	res, err := Restore(context.Background(), RestoreOptions{
		WezDBPath: "/nonexistent/wezterm.db",
		Out:       &buf,
		Replay:    []string{"claude"},
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
	res, err := Restore(context.Background(), testOpts(RestoreOptions{
		WezDBPath:    wezPath,
		SpawnIfEmpty: true,
		Spawner:      f,
		Replay:       []string{"claude", "tomoe"},
	}))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.WindowsSpawned != 2 {
		t.Errorf("windows = %d, want 2 (no spurious fallback window)", res.WindowsSpawned)
	}
}

func TestRestoreWorkspaceFilter(t *testing.T) {
	wezPath := seedSnapshot(t)
	f := newFakeSpawner()

	res, err := Restore(context.Background(), testOpts(RestoreOptions{
		WezDBPath: wezPath,
		Spawner:   f,
		Workspace: "other",
		Replay:    []string{"claude", "tomoe"},
	}))
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
