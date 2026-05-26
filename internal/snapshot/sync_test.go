package snapshot

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

func openTestWez(t *testing.T) *store.WezStore {
	t.Helper()
	dir := t.TempDir()
	w, err := store.OpenWez(filepath.Join(dir, "wezterm.db"))
	if err != nil {
		t.Fatalf("OpenWez: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// sampleTree constructs a minimal in-memory wezterm payload for tests.
// No filesystem dependency, no developer-machine references.
//
// Shape: 2 windows, 4 panes total (matches the original on-disk fixture
// shape so existing assertions stay valid):
//
//	window 0
//	├── tab 1  (pane 1)
//	└── tab 6  (pane 12, pane 13 — same tab, different left_col = split)
//	window 1
//	└── tab 8  (pane 20)
func sampleTree() (raw []byte, panes []wezterm.RawPane) {
	panes = []wezterm.RawPane{
		{
			WindowID: 0, TabID: 1, PaneID: 1, Workspace: "default",
			Size: wezterm.Size{Cols: 254, Rows: 59},
			CWD:  "file://host/home/user/projects/foo/",
			TTYName: "/dev/pts/15", IsActive: true,
		},
		{
			WindowID: 0, TabID: 6, PaneID: 12, Workspace: "default",
			Size: wezterm.Size{Cols: 127, Rows: 59},
			CWD:  "file://host/home/user/projects/bar/",
			Title: "claude", LeftCol: 0, IsActive: true,
			TTYName: "/dev/pts/14",
		},
		{
			WindowID: 0, TabID: 6, PaneID: 13, Workspace: "default",
			Size: wezterm.Size{Cols: 126, Rows: 59},
			CWD:  "file://host/home/user/projects/bar/",
			LeftCol: 128, TTYName: "/dev/pts/19",
		},
		{
			WindowID: 1, TabID: 8, PaneID: 20, Workspace: "default",
			Size: wezterm.Size{Cols: 200, Rows: 50},
			CWD:  "file://host/home/user/audio/",
			Title: "tomoe", IsActive: true,
			TTYName: "/dev/pts/22",
		},
	}
	// `raw` only matters for the content-hash skip logic. A stable byte
	// sequence keyed off the tree shape is enough; doesn't need to be valid
	// JSON because Sync takes `panes` as the source of truth.
	raw = []byte("sample-tree-v1: 4 panes / 2 windows")
	return
}

// loadFixture is the backwards-compatible name used by existing tests.
func loadFixture(t *testing.T) ([]byte, []wezterm.RawPane) {
	t.Helper()
	return sampleTree()
}

func countRows(t *testing.T, w *store.WezStore, table string) int {
	t.Helper()
	var n int
	if err := w.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestSyncWritesTree(t *testing.T) {
	w := openTestWez(t)
	raw, panes := loadFixture(t)

	res, err := Sync(w, raw, panes, "/run/wezterm/mux", time.UnixMilli(1000))
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !res.DidWork {
		t.Fatal("expected DidWork=true on first sync")
	}
	if res.WindowsWritten != 2 {
		t.Errorf("windows = %d, want 2", res.WindowsWritten)
	}
	if res.TabsWritten != 3 { // tabs 1, 6, 8
		t.Errorf("tabs = %d, want 3", res.TabsWritten)
	}
	if res.PanesWritten != 4 {
		t.Errorf("panes = %d, want 4", res.PanesWritten)
	}

	if got := countRows(t, w, "terminal_windows"); got != 2 {
		t.Errorf("window rows = %d", got)
	}
	if got := countRows(t, w, "terminal_tabs"); got != 3 {
		t.Errorf("tab rows = %d", got)
	}
	if got := countRows(t, w, "terminal_panes"); got != 4 {
		t.Errorf("pane rows = %d", got)
	}

	meta, ok, err := w.GetSnapshotMeta()
	if err != nil || !ok {
		t.Fatalf("meta: ok=%v err=%v", ok, err)
	}
	if meta.TakenAt != 1000 {
		t.Errorf("taken_at = %d", meta.TakenAt)
	}
	if meta.ContentHash != res.ContentHashHex {
		t.Errorf("hash mismatch: meta=%q result=%q", meta.ContentHash, res.ContentHashHex)
	}
}

func TestSyncHashSkipsWhenUnchanged(t *testing.T) {
	w := openTestWez(t)
	raw, panes := loadFixture(t)

	_, err := Sync(w, raw, panes, "/run/wezterm/mux", time.UnixMilli(1000))
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Same payload, later timestamp.
	res, err := Sync(w, raw, panes, "/run/wezterm/mux", time.UnixMilli(2000))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.DidWork {
		t.Fatal("expected hash-skip on identical payload")
	}

	// Meta timestamp should be touched.
	meta, _, _ := w.GetSnapshotMeta()
	if meta.TakenAt != 2000 {
		t.Errorf("taken_at not touched: %d", meta.TakenAt)
	}
}

func TestSyncDeletesClosedPanes(t *testing.T) {
	w := openTestWez(t)
	raw, panes := loadFixture(t)

	if _, err := Sync(w, raw, panes, "/run/wezterm/mux", time.UnixMilli(1000)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// Simulate closing window 1 (which has tab 8 with pane 20) AND closing pane 13
	// in tab 6 of window 0. Result: only 3 panes remain (1, 12, 20 was closed... wait).
	// Fixture had: window 0 [tab 1 (pane 1)], window 0 [tab 6 (panes 12, 13)], window 1 [tab 8 (pane 20)]
	// After closing window 1 and pane 13: window 0 [tab 1 (pane 1)], window 0 [tab 6 (pane 12)]
	smaller := []wezterm.RawPane{panes[0], panes[1]}
	smallerRaw := []byte(`[fake-changed]`) // any different bytes; hash differs from original

	res, err := Sync(w, smallerRaw, smaller, "/run/wezterm/mux", time.UnixMilli(2000))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if !res.DidWork {
		t.Fatal("second sync should have done work (hash changed)")
	}
	if res.WindowsWritten != 1 {
		t.Errorf("windows = %d, want 1 (only window 0)", res.WindowsWritten)
	}
	if res.PanesWritten != 2 {
		t.Errorf("panes = %d, want 2", res.PanesWritten)
	}

	// Closed window 1 should have cascaded away.
	if got := countRows(t, w, "terminal_windows"); got != 1 {
		t.Errorf("window rows after close = %d, want 1", got)
	}
	if got := countRows(t, w, "terminal_tabs"); got != 2 { // tabs 1 and 6
		t.Errorf("tab rows after close = %d, want 2", got)
	}
	if got := countRows(t, w, "terminal_panes"); got != 2 {
		t.Errorf("pane rows after close = %d, want 2", got)
	}

	// Verify the closed pane id is actually gone.
	var n int
	_ = w.DB().QueryRow("SELECT COUNT(*) FROM terminal_panes WHERE pane_id IN (13, 20)").Scan(&n)
	if n != 0 {
		t.Errorf("closed panes still present: %d", n)
	}
}

func TestSyncPrunesPaneRuntimeStateForClosedPanes(t *testing.T) {
	w := openTestWez(t)
	mux := "/run/wezterm/mux"

	// Seed pane_runtime_state for 4 panes (matching the fixture).
	for _, pid := range []int64{1, 12, 13, 20} {
		if err := w.ApplyPreexec(mux, pid, "claude", "/tmp", 1); err != nil {
			t.Fatalf("seed pane %d: %v", pid, err)
		}
	}

	raw, panes := loadFixture(t)
	if _, err := Sync(w, raw, panes, mux, time.UnixMilli(1000)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// All 4 runtime rows survive — all panes still live.
	var n int
	_ = w.DB().QueryRow("SELECT COUNT(*) FROM pane_runtime_state WHERE mux_socket = ?", mux).Scan(&n)
	if n != 4 {
		t.Fatalf("runtime rows after first sync = %d, want 4", n)
	}

	// Close panes 13 and 20.
	smaller := []wezterm.RawPane{panes[0], panes[1]}
	res, err := Sync(w, []byte(`changed`), smaller, mux, time.UnixMilli(2000))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.RuntimePruned != 2 {
		t.Errorf("RuntimePruned = %d, want 2", res.RuntimePruned)
	}
	_ = w.DB().QueryRow("SELECT COUNT(*) FROM pane_runtime_state WHERE mux_socket = ?", mux).Scan(&n)
	if n != 2 {
		t.Errorf("runtime rows after close = %d, want 2", n)
	}
}

func TestSyncDoesNotPruneOtherMuxSockets(t *testing.T) {
	w := openTestWez(t)

	// Seed runtime rows for two different mux sockets.
	if err := w.ApplyPreexec("/run/mux-A", 1, "cmd", "/tmp", 1); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := w.ApplyPreexec("/run/mux-B", 99, "cmd", "/tmp", 1); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	// Sync for mux-A with no panes — should prune mux-A's rows but leave mux-B alone.
	res, err := Sync(w, []byte(`[]`), nil, "/run/mux-A", time.UnixMilli(1000))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.RuntimePruned != 1 {
		t.Errorf("pruned = %d, want 1", res.RuntimePruned)
	}

	var n int
	_ = w.DB().QueryRow("SELECT COUNT(*) FROM pane_runtime_state WHERE mux_socket = ?", "/run/mux-B").Scan(&n)
	if n != 1 {
		t.Errorf("mux-B runtime rows wrongly affected: %d", n)
	}
}

func TestSyncCopiesRuntimeStateIntoTerminalPanes(t *testing.T) {
	w := openTestWez(t)
	mux := "/run/wezterm/mux"

	// Seed runtime for the claude pane (12) and the tomoe pane (20).
	if err := w.ApplyPreexec(mux, 12, "claude", "/tmp/proj", 100); err != nil {
		t.Fatalf("preexec 12: %v", err)
	}
	if err := w.BindClaudeSession(mux, 12, "sess-abc", "/tmp/proj"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := w.ApplyPreexec(mux, 20, "tomoe start --device hw:0,0", "/home/u/audio", 200); err != nil {
		t.Fatalf("preexec 20: %v", err)
	}

	raw, panes := loadFixture(t)
	if _, err := Sync(w, raw, panes, mux, time.UnixMilli(1000)); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Inspect terminal_panes for pane 12 — should have current_cmd = "claude" and claude_session_id.
	var currentCmd, claudeID, lastCmd []byte
	err := w.DB().QueryRow(
		`SELECT current_cmd, claude_session_id, last_cmd FROM terminal_panes WHERE pane_id = 12`,
	).Scan(&currentCmd, &claudeID, &lastCmd)
	if err != nil {
		t.Fatalf("query pane 12: %v", err)
	}
	if string(currentCmd) != "claude" {
		t.Errorf("pane 12 current_cmd = %q", currentCmd)
	}
	if string(claudeID) != "sess-abc" {
		t.Errorf("pane 12 claude_session_id = %q", claudeID)
	}

	// Pane 20.
	err = w.DB().QueryRow(
		`SELECT current_cmd, claude_session_id FROM terminal_panes WHERE pane_id = 20`,
	).Scan(&currentCmd, &claudeID)
	if err != nil {
		t.Fatalf("query pane 20: %v", err)
	}
	if string(currentCmd) != "tomoe start --device hw:0,0" {
		t.Errorf("pane 20 current_cmd = %q", currentCmd)
	}
	if len(claudeID) != 0 {
		t.Errorf("pane 20 should have no claude session, got %q", claudeID)
	}

	// Pane 1 has no runtime state — current_cmd and claude_session_id should be NULL.
	var currNull, claudeNull []byte
	err = w.DB().QueryRow(
		`SELECT current_cmd, claude_session_id FROM terminal_panes WHERE pane_id = 1`,
	).Scan(&currNull, &claudeNull)
	if err != nil {
		t.Fatalf("query pane 1: %v", err)
	}
	if len(currNull) != 0 {
		t.Errorf("pane 1 current_cmd should be NULL, got %q", currNull)
	}
}

func TestBuildTreeOrderStableAcrossInputs(t *testing.T) {
	in := []wezterm.RawPane{
		{WindowID: 2, TabID: 20, PaneID: 100, Workspace: "default"},
		{WindowID: 1, TabID: 10, PaneID: 50, Workspace: "default"},
		{WindowID: 2, TabID: 21, PaneID: 200, Workspace: "default"},
		{WindowID: 1, TabID: 10, PaneID: 51, Workspace: "default"},
	}
	tree := buildTree(in)
	if len(tree.Windows) != 2 {
		t.Fatalf("windows = %d", len(tree.Windows))
	}
	if tree.Windows[0].WindowID != 2 {
		t.Fatalf("first window should be 2 (first seen), got %d", tree.Windows[0].WindowID)
	}
	if tree.Windows[1].WindowID != 1 {
		t.Fatalf("second window should be 1, got %d", tree.Windows[1].WindowID)
	}
	if tree.Windows[1].WinIndex != 1 {
		t.Errorf("win_index = %d", tree.Windows[1].WinIndex)
	}
	// Window 1 has two panes in tab 10.
	if len(tree.Windows[1].Tabs[0].Panes) != 2 {
		t.Errorf("tab 10 panes = %d", len(tree.Windows[1].Tabs[0].Panes))
	}
}

func TestSyncEmptyPanesNoCrash(t *testing.T) {
	w := openTestWez(t)
	res, err := Sync(w, []byte(`[]`), nil, "/run/mux", time.UnixMilli(1000))
	if err != nil {
		t.Fatalf("Sync empty: %v", err)
	}
	if !res.DidWork {
		t.Error("empty sync should still write meta")
	}
	if got := countRows(t, w, "terminal_panes"); got != 0 {
		t.Errorf("pane rows = %d", got)
	}
}
