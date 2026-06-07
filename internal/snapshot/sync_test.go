package snapshot

import (
	"database/sql"
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
			Size:    wezterm.Size{Cols: 254, Rows: 59},
			CWD:     "file://host/home/user/projects/foo/",
			TTYName: "/dev/pts/15", IsActive: true,
		},
		{
			WindowID: 0, TabID: 6, PaneID: 12, Workspace: "default",
			Size:  wezterm.Size{Cols: 127, Rows: 59},
			CWD:   "file://host/home/user/projects/bar/",
			Title: "claude", LeftCol: 0, IsActive: true,
			TTYName: "/dev/pts/14",
		},
		{
			WindowID: 0, TabID: 6, PaneID: 13, Workspace: "default",
			Size:    wezterm.Size{Cols: 126, Rows: 59},
			CWD:     "file://host/home/user/projects/bar/",
			LeftCol: 128, TTYName: "/dev/pts/19",
		},
		{
			WindowID: 1, TabID: 8, PaneID: 20, Workspace: "default",
			Size:  wezterm.Size{Cols: 200, Rows: 50},
			CWD:   "file://host/home/user/audio/",
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

// --- Clobber guard: an empty restarted wezterm must not overwrite a saved layout ---

// helperEmptyStartupPanes returns a single-pane "fresh wezterm" payload (the
// shape an empty restarted instance produces) with a distinct mux socket.
func helperEmptyStartupPanes() ([]byte, []wezterm.RawPane, string) {
	panes := []wezterm.RawPane{
		{
			WindowID: 99, TabID: 99, PaneID: 99, Workspace: "default",
			Size:    wezterm.Size{Cols: 200, Rows: 50},
			CWD:     "file://host/home/user/",
			TTYName: "/dev/pts/0", IsActive: true,
		},
	}
	return []byte("empty-startup-1pane"), panes, "/run/wezterm/new-mux"
}

// TestSyncGuardRefusesEmptyStartupClobber is the regression test for the bug
// that wiped a real 12-pane layout: a restarted, empty wezterm fired a snapshot
// (1 pane, no command state) before `cst restore` ran, and Sync DELETEd the
// saved tree. The guard must refuse that write and keep the saved snapshot.
func TestSyncGuardRefusesEmptyStartupClobber(t *testing.T) {
	w := openTestWez(t)
	mux := "/run/wezterm/mux"

	// Seed a rich saved snapshot: 4 panes, with command + claude state bound.
	for _, pid := range []int64{1, 12, 13, 20} {
		if err := w.ApplyPreexec(mux, pid, "claude", "/tmp", 1); err != nil {
			t.Fatalf("seed %d: %v", pid, err)
		}
	}
	if err := w.BindClaudeSession(mux, 12, "sess-keep", "/tmp"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	raw, panes := loadFixture(t)
	if _, err := Sync(w, raw, panes, mux, time.UnixMilli(1000)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if got := countRows(t, w, "terminal_panes"); got != 4 {
		t.Fatalf("setup: pane rows = %d, want 4", got)
	}

	// Now a fresh, empty wezterm (different mux, 1 command-less pane) snapshots.
	eraw, epanes, emux := helperEmptyStartupPanes()
	res, err := Sync(w, eraw, epanes, emux, time.UnixMilli(2000))
	if err != nil {
		t.Fatalf("empty-startup sync: %v", err)
	}
	if !res.SkippedClobber {
		t.Error("expected SkippedClobber=true on empty-startup snapshot")
	}
	if res.DidWork {
		t.Error("guard should have prevented the destructive write (DidWork=true)")
	}

	// The saved 4-pane layout MUST still be intact.
	if got := countRows(t, w, "terminal_panes"); got != 4 {
		t.Errorf("saved layout was clobbered: pane rows = %d, want 4", got)
	}
	if got := countRows(t, w, "terminal_windows"); got != 2 {
		t.Errorf("saved windows clobbered: %d, want 2", got)
	}
	// The linked claude session must survive.
	var claudeID []byte
	if err := w.DB().QueryRow(`SELECT claude_session_id FROM terminal_panes WHERE pane_id = 12`).Scan(&claudeID); err != nil {
		t.Fatalf("query pane 12: %v", err)
	}
	if string(claudeID) != "sess-keep" {
		t.Errorf("claude session lost: %q", claudeID)
	}
	// Timestamp should still advance (debounce tracking).
	meta, _, _ := w.GetSnapshotMeta()
	if meta.TakenAt != 2000 {
		t.Errorf("taken_at = %d, want 2000 (touched)", meta.TakenAt)
	}
}

// TestSyncGuardAllowsLiveShrink ensures the guard does NOT block a legitimate
// user-driven shrink: closing a pane mid-session leaves runtime state on the
// survivors, so the incoming snapshot still carries command state and must be
// written.
func TestSyncGuardAllowsLiveShrink(t *testing.T) {
	w := openTestWez(t)
	mux := "/run/wezterm/mux"

	for _, pid := range []int64{1, 12, 13, 20} {
		if err := w.ApplyPreexec(mux, pid, "claude", "/tmp", 1); err != nil {
			t.Fatalf("seed %d: %v", pid, err)
		}
	}
	raw, panes := loadFixture(t)
	if _, err := Sync(w, raw, panes, mux, time.UnixMilli(1000)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// User closes two panes (same mux; survivors keep their runtime state).
	smaller := []wezterm.RawPane{panes[0], panes[1]}
	res, err := Sync(w, []byte(`live-shrink`), smaller, mux, time.UnixMilli(2000))
	if err != nil {
		t.Fatalf("shrink sync: %v", err)
	}
	if res.SkippedClobber {
		t.Error("guard wrongly blocked a live shrink with surviving runtime state")
	}
	if !res.DidWork {
		t.Error("live shrink should have been written")
	}
	if got := countRows(t, w, "terminal_panes"); got != 2 {
		t.Errorf("pane rows after shrink = %d, want 2", got)
	}
}

// TestSyncGuardAllowsWriteWhenNoSavedState confirms first-run / stateless saved
// snapshots are always overwritable (nothing meaningful to protect).
func TestSyncGuardAllowsWriteWhenNoSavedState(t *testing.T) {
	w := openTestWez(t)
	// Save a snapshot with NO runtime state (command-less panes).
	raw, panes := loadFixture(t)
	if _, err := Sync(w, raw, panes, "/run/wezterm/mux", time.UnixMilli(1000)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// A smaller, empty-startup snapshot should be allowed (saved had no state).
	eraw, epanes, emux := helperEmptyStartupPanes()
	res, err := Sync(w, eraw, epanes, emux, time.UnixMilli(2000))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.SkippedClobber {
		t.Error("guard should not protect a stateless saved snapshot")
	}
	if got := countRows(t, w, "terminal_panes"); got != 1 {
		t.Errorf("pane rows = %d, want 1 (overwrite allowed)", got)
	}
}

func TestShouldSkipClobberUnit(t *testing.T) {
	cases := []struct {
		name            string
		saved, incoming store.SnapshotStats
		want            bool
	}{
		{"empty-startup over rich save", store.SnapshotStats{PaneCount: 12, CommandedPanes: 6, ClaudePanes: 6}, store.SnapshotStats{PaneCount: 1}, true},
		{"nothing saved", store.SnapshotStats{PaneCount: 3}, store.SnapshotStats{PaneCount: 1}, false},
		{"incoming has command state", store.SnapshotStats{PaneCount: 4, CommandedPanes: 4}, store.SnapshotStats{PaneCount: 2, CommandedPanes: 2}, false},
		{"incoming larger", store.SnapshotStats{PaneCount: 2, CommandedPanes: 2}, store.SnapshotStats{PaneCount: 5}, false},
		{"equal size, no incoming state", store.SnapshotStats{PaneCount: 4, CommandedPanes: 4}, store.SnapshotStats{PaneCount: 4}, false},
	}
	for _, c := range cases {
		if got := shouldSkipClobber(c.saved, c.incoming); got != c.want {
			t.Errorf("%s: shouldSkipClobber = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- Split geometry capture (Fix A) ---

// tomoeTabPanes builds the real tomoe layout from the user's machine:
//
//	A: full-height left column
//	B: top-right
//	C: bottom-right (vertical split of the right column)
//
// Geometry uses the confirmed divider gap of 1 cell.
func tomoeTabPanes() []wezterm.RawPane {
	return []wezterm.RawPane{
		{WindowID: 1, TabID: 1, PaneID: 1, Workspace: "default",
			LeftCol: 0, TopRow: 0, Size: wezterm.Size{Cols: 122, Rows: 59},
			CWD: "file://host/proj/tomoe/"},
		{WindowID: 1, TabID: 1, PaneID: 2, Workspace: "default",
			LeftCol: 123, TopRow: 0, Size: wezterm.Size{Cols: 124, Rows: 30},
			CWD: "file://host/proj/tomoe/"},
		{WindowID: 1, TabID: 1, PaneID: 15, Workspace: "default",
			LeftCol: 123, TopRow: 31, Size: wezterm.Size{Cols: 124, Rows: 28},
			CWD: "file://host/proj/tomoe/"},
	}
}

func TestBuildTreeInfersSplits(t *testing.T) {
	tree := buildTree(tomoeTabPanes())
	if len(tree.Windows) != 1 || len(tree.Windows[0].Tabs) != 1 {
		t.Fatalf("expected 1 window/1 tab, got %d/%d", len(tree.Windows), len(tree.Windows[0].Tabs))
	}
	byID := map[int64]store.WezPane{}
	for _, p := range tree.Windows[0].Tabs[0].Panes {
		byID[p.PaneID] = p
	}

	// A (pane 1) is the tab lead: no parent.
	if a := byID[1]; a.ParentPaneID != nil || a.SplitDirection != "" {
		t.Errorf("lead pane 1: parent=%v dir=%q, want nil/\"\"", a.ParentPaneID, a.SplitDirection)
	}
	// B (pane 2) is a right split of A.
	if b := byID[2]; b.ParentPaneID == nil || *b.ParentPaneID != 1 || b.SplitDirection != "right" {
		t.Errorf("pane 2: parent=%v dir=%q, want parent=1 dir=right", ptr(b.ParentPaneID), b.SplitDirection)
	}
	// C (pane 15) is a bottom split of B — NOT of the full-height A.
	if c := byID[15]; c.ParentPaneID == nil || *c.ParentPaneID != 2 || c.SplitDirection != "bottom" {
		t.Errorf("pane 15: parent=%v dir=%q, want parent=2 dir=bottom", ptr(c.ParentPaneID), c.SplitDirection)
	}
}

func TestBuildTreeSinglePaneNoParent(t *testing.T) {
	tree := buildTree([]wezterm.RawPane{
		{WindowID: 0, TabID: 1, PaneID: 1, Workspace: "default",
			Size: wezterm.Size{Cols: 100, Rows: 30}, CWD: "file://host/x/"},
	})
	p := tree.Windows[0].Tabs[0].Panes[0]
	if p.ParentPaneID != nil || p.SplitDirection != "" {
		t.Errorf("single pane: parent=%v dir=%q, want nil/\"\"", p.ParentPaneID, p.SplitDirection)
	}
}

func TestBuildTreeFallbackOnZeroGeometry(t *testing.T) {
	// All-zero geometry (degenerate / synthetic) → non-lead panes hang off the
	// lead, split right. No pane dropped.
	tree := buildTree([]wezterm.RawPane{
		{WindowID: 0, TabID: 1, PaneID: 1, Workspace: "default", CWD: "file://host/x/"},
		{WindowID: 0, TabID: 1, PaneID: 2, Workspace: "default", CWD: "file://host/x/"},
		{WindowID: 0, TabID: 1, PaneID: 3, Workspace: "default", CWD: "file://host/x/"},
	})
	byID := map[int64]store.WezPane{}
	for _, p := range tree.Windows[0].Tabs[0].Panes {
		byID[p.PaneID] = p
	}
	// Lead is the smallest pane_id (all geometry equal → tie-break by id).
	if byID[1].ParentPaneID != nil {
		t.Errorf("expected pane 1 to be lead, got parent=%v", byID[1].ParentPaneID)
	}
	for _, id := range []int64{2, 3} {
		p := byID[id]
		if p.ParentPaneID == nil || *p.ParentPaneID != 1 || p.SplitDirection != "right" {
			t.Errorf("pane %d fallback: parent=%v dir=%q, want parent=1 dir=right", id, ptr(p.ParentPaneID), p.SplitDirection)
		}
	}
}

func TestSyncPersistsSplits(t *testing.T) {
	w := openTestWez(t)
	if _, err := Sync(w, []byte("tomoe-tab"), tomoeTabPanes(), "/run/mux", time.UnixMilli(1000)); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Regression vs the old hardcoded NULL: B/C rows must carry parent+direction.
	type row struct {
		parent sql.NullInt64
		dir    sql.NullString
	}
	get := func(pane int64) row {
		var r row
		if err := w.DB().QueryRow(
			`SELECT parent_pane_id, split_direction FROM terminal_panes WHERE pane_id = ?`, pane,
		).Scan(&r.parent, &r.dir); err != nil {
			t.Fatalf("query pane %d: %v", pane, err)
		}
		return r
	}
	if r := get(1); r.parent.Valid {
		t.Errorf("pane 1 parent should be NULL, got %d", r.parent.Int64)
	}
	if r := get(2); !r.parent.Valid || r.parent.Int64 != 1 || r.dir.String != "right" {
		t.Errorf("pane 2 persisted parent=%v dir=%q, want 1/right", r.parent, r.dir.String)
	}
	if r := get(15); !r.parent.Valid || r.parent.Int64 != 2 || r.dir.String != "bottom" {
		t.Errorf("pane 15 persisted parent=%v dir=%q, want 2/bottom", r.parent, r.dir.String)
	}
}

func ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
