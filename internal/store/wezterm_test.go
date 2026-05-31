package store

import (
	"path/filepath"
	"testing"
)

func testWezStore(t *testing.T) *WezStore {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wezterm.db")
	w, err := OpenWez(dbPath)
	if err != nil {
		t.Fatalf("OpenWez: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// testWezStoreWithSessions opens a WezStore with an ATTACHed sessions.db that
// has one active session keyed by the given PID.
func testWezStoreWithSessions(t *testing.T, claudePID int, claudeSessionID string) *WezStore {
	t.Helper()
	dir := t.TempDir()

	// Create sessions.db with one active session.
	sessPath := filepath.Join(dir, "sessions.db")
	sess, err := Open(sessPath)
	if err != nil {
		t.Fatalf("Open sessions: %v", err)
	}
	pid := claudePID
	if err := sess.UpsertSession(Session{
		ID:           claudeSessionID,
		Project:      "/tmp/proj",
		CWD:          "/tmp/proj",
		StartedAt:    1,
		LastActivity: 1,
		PID:          &pid,
		Active:       true,
		Model:        "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	_ = sess.Close()

	wezPath := filepath.Join(dir, "wezterm.db")
	w, err := OpenWez(wezPath)
	if err != nil {
		t.Fatalf("OpenWez: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if err := w.AttachSessionsDB(sessPath); err != nil {
		t.Fatalf("AttachSessionsDB: %v", err)
	}
	return w
}

func TestOpenWezCreatesSchema(t *testing.T) {
	w := testWezStore(t)
	// Verify all five tables exist.
	want := []string{"snapshot_meta", "terminal_windows", "terminal_tabs", "terminal_panes", "pane_runtime_state"}
	for _, table := range want {
		var name string
		err := w.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

func TestSnapshotMetaUpsert(t *testing.T) {
	w := testWezStore(t)

	_, ok, err := w.GetSnapshotMeta()
	if err != nil {
		t.Fatalf("GetSnapshotMeta empty: %v", err)
	}
	if ok {
		t.Fatal("expected no meta initially")
	}

	if err := w.SetSnapshotMeta(SnapshotMeta{TakenAt: 100, ContentHash: "abc"}); err != nil {
		t.Fatalf("SetSnapshotMeta: %v", err)
	}
	got, ok, err := w.GetSnapshotMeta()
	if err != nil || !ok {
		t.Fatalf("GetSnapshotMeta after set: ok=%v err=%v", ok, err)
	}
	if got.TakenAt != 100 || got.ContentHash != "abc" {
		t.Fatalf("got %+v", got)
	}

	// Upsert: change hash.
	if err := w.SetSnapshotMeta(SnapshotMeta{TakenAt: 200, ContentHash: "xyz"}); err != nil {
		t.Fatalf("SetSnapshotMeta 2: %v", err)
	}
	got, _, _ = w.GetSnapshotMeta()
	if got.TakenAt != 200 || got.ContentHash != "xyz" {
		t.Fatalf("after upsert got %+v", got)
	}

	// Touch updates only the timestamp.
	if err := w.TouchSnapshotTimestamp(300); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, _, _ = w.GetSnapshotMeta()
	if got.TakenAt != 300 || got.ContentHash != "xyz" {
		t.Fatalf("after touch got %+v", got)
	}
}

func TestForeignKeyCascade(t *testing.T) {
	w := testWezStore(t)

	// Insert a window → tab → pane chain.
	_, err := w.db.Exec(`INSERT INTO terminal_windows(window_id, workspace, win_index) VALUES (1, 'default', 0)`)
	if err != nil {
		t.Fatalf("insert window: %v", err)
	}
	_, err = w.db.Exec(`INSERT INTO terminal_tabs(tab_id, window_id, tab_index) VALUES (10, 1, 0)`)
	if err != nil {
		t.Fatalf("insert tab: %v", err)
	}
	_, err = w.db.Exec(`INSERT INTO terminal_panes(pane_id, tab_id, size_cols, size_rows, cwd)
		VALUES (100, 10, 80, 24, '/tmp')`)
	if err != nil {
		t.Fatalf("insert pane: %v", err)
	}

	// Delete the window — tabs and panes should cascade.
	if _, err := w.db.Exec(`DELETE FROM terminal_windows WHERE window_id = 1`); err != nil {
		t.Fatalf("delete window: %v", err)
	}
	var n int
	_ = w.db.QueryRow(`SELECT COUNT(*) FROM terminal_tabs`).Scan(&n)
	if n != 0 {
		t.Fatalf("tabs not cascaded, got %d", n)
	}
	_ = w.db.QueryRow(`SELECT COUNT(*) FROM terminal_panes`).Scan(&n)
	if n != 0 {
		t.Fatalf("panes not cascaded, got %d", n)
	}
}

func TestSnapshotMetaSingleRowConstraint(t *testing.T) {
	w := testWezStore(t)
	if err := w.SetSnapshotMeta(SnapshotMeta{TakenAt: 1, ContentHash: "a"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Direct INSERT with id != 1 should fail.
	_, err := w.db.Exec(`INSERT INTO snapshot_meta (id, taken_at, content_hash) VALUES (2, 1, 'x')`)
	if err == nil {
		t.Fatal("expected CHECK constraint to reject id=2")
	}
}

func TestApplyPreexecAndPrecmd(t *testing.T) {
	w := testWezStore(t)

	// preexec: command starts running.
	if err := w.ApplyPreexec("/run/mux", 5, "tomoe start --device hw:0,0", "/home/u/proj", 1000); err != nil {
		t.Fatalf("ApplyPreexec: %v", err)
	}
	s, ok, _ := w.LookupPaneRuntime("/run/mux", 5)
	if !ok {
		t.Fatal("runtime missing after preexec")
	}
	if s.CurrentCommand != "tomoe start --device hw:0,0" {
		t.Fatalf("current cmd = %q", s.CurrentCommand)
	}
	if s.CurrentStartedAt != 1000 {
		t.Fatalf("started_at = %d", s.CurrentStartedAt)
	}
	if s.LastCommand != "" {
		t.Fatalf("last_command should be empty initially, got %q", s.LastCommand)
	}

	// precmd: command finished, becomes last_command.
	if err := w.ApplyPrecmd("/run/mux", 5, "/home/u/proj", 2000); err != nil {
		t.Fatalf("ApplyPrecmd: %v", err)
	}
	s, _, _ = w.LookupPaneRuntime("/run/mux", 5)
	if s.CurrentCommand != "" {
		t.Fatalf("current cmd should be cleared, got %q", s.CurrentCommand)
	}
	if s.LastCommand != "tomoe start --device hw:0,0" {
		t.Fatalf("last cmd = %q", s.LastCommand)
	}
	if s.LastFinishedAt != 2000 {
		t.Fatalf("finished_at = %d", s.LastFinishedAt)
	}

	// precmd with no prior current_command just updates cwd.
	if err := w.ApplyPrecmd("/run/mux", 5, "/home/u/proj/sub", 3000); err != nil {
		t.Fatalf("ApplyPrecmd 2: %v", err)
	}
	s, _, _ = w.LookupPaneRuntime("/run/mux", 5)
	if s.CWD != "/home/u/proj/sub" {
		t.Fatalf("cwd = %q", s.CWD)
	}
	// last_command should still be the prior one.
	if s.LastCommand != "tomoe start --device hw:0,0" {
		t.Fatalf("last cmd should persist, got %q", s.LastCommand)
	}
}

func TestInferMuxSocketEmptyWhenNoRuntime(t *testing.T) {
	w := testWezStore(t)
	sock, err := w.InferMuxSocket()
	if err != nil {
		t.Fatalf("InferMuxSocket: %v", err)
	}
	if sock != "" {
		t.Errorf("expected empty, got %q", sock)
	}
}

func TestInferMuxSocketReturnsMostRecent(t *testing.T) {
	w := testWezStore(t)
	// Seed two mux sockets at different times. Most-recent should win.
	if err := w.ApplyPreexec("/run/mux-A", 1, "cmd-A", "/tmp", 100); err != nil {
		t.Fatal(err)
	}
	if err := w.ApplyPreexec("/run/mux-B", 1, "cmd-B", "/tmp", 200); err != nil {
		t.Fatal(err)
	}
	// Finish mux-A's command — that gives it a last_finished_at > both.
	if err := w.ApplyPrecmd("/run/mux-A", 1, "/tmp", 300); err != nil {
		t.Fatal(err)
	}
	sock, err := w.InferMuxSocket()
	if err != nil {
		t.Fatalf("InferMuxSocket: %v", err)
	}
	if sock != "/run/mux-A" {
		t.Errorf("expected /run/mux-A (most recent), got %q", sock)
	}
}

func TestPruneDeadMux(t *testing.T) {
	w := testWezStore(t)
	for _, sock := range []string{"/run/mux-1", "/run/mux-2"} {
		if err := w.ApplyPreexec(sock, 1, "claude", "/tmp", 1); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	n, err := w.PruneDeadMux("/run/mux-1")
	if err != nil || n != 1 {
		t.Fatalf("PruneDeadMux: n=%d err=%v", n, err)
	}
	// /run/mux-2 still present.
	_, ok, _ := w.LookupPaneRuntime("/run/mux-2", 1)
	if !ok {
		t.Fatal("other mux row was incorrectly deleted")
	}
}

func TestPrunePaneRuntimeNotIn(t *testing.T) {
	w := testWezStore(t)
	sock := "/run/mux"
	for _, pid := range []int64{1, 2, 3, 4} {
		if err := w.ApplyPreexec(sock, pid, "claude", "/tmp", pid); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	n, err := w.PrunePaneRuntimeNotIn(sock, []int64{2, 4})
	if err != nil {
		t.Fatalf("PrunePaneRuntimeNotIn: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deletions, got %d", n)
	}
	for _, kept := range []int64{2, 4} {
		_, ok, _ := w.LookupPaneRuntime(sock, kept)
		if !ok {
			t.Fatalf("pane %d should be kept", kept)
		}
	}
	for _, gone := range []int64{1, 3} {
		_, ok, _ := w.LookupPaneRuntime(sock, gone)
		if ok {
			t.Fatalf("pane %d should be pruned", gone)
		}
	}
	// Empty live set = prune everything for this mux.
	n, err = w.PrunePaneRuntimeNotIn(sock, nil)
	if err != nil || n != 2 {
		t.Fatalf("prune all: n=%d err=%v", n, err)
	}
}

func TestReadTreeRoundtrip(t *testing.T) {
	w := testWezStore(t)

	// Seed: 2 windows, each with 1 tab, second window's tab has 2 panes (one split).
	_, err := w.db.Exec(`
		INSERT INTO terminal_windows(window_id, workspace, win_index) VALUES
			(1, 'default', 0), (2, 'default', 1);
		INSERT INTO terminal_tabs(tab_id, window_id, tab_index) VALUES
			(10, 1, 0), (20, 2, 0);
		INSERT INTO terminal_panes(pane_id, tab_id, parent_pane_id, split_direction,
			size_cols, size_rows, cwd, title, foreground_pid, foreground_name,
			last_cmd, current_cmd, claude_session_id) VALUES
			(100, 10, NULL, NULL, 200, 50, '/tmp/a', 'bash', 1111, 'bash', NULL, NULL, NULL),
			(200, 20, NULL, NULL, 100, 50, '/tmp/b', 'claude', 2222, 'claude', NULL, 'claude --resume X', 'sess-X'),
			(201, 20, 200, 'right', 100, 50, '/tmp/b', 'tomoe', 3333, 'tomoe', 'tomoe', NULL, NULL);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	tree, err := w.ReadTree()
	if err != nil {
		t.Fatalf("ReadTree: %v", err)
	}
	if len(tree.Windows) != 2 {
		t.Fatalf("windows = %d", len(tree.Windows))
	}
	if tree.Windows[0].WindowID != 1 || tree.Windows[1].WindowID != 2 {
		t.Fatal("window order wrong")
	}
	if len(tree.Windows[1].Tabs) != 1 {
		t.Fatalf("window 2 tabs = %d", len(tree.Windows[1].Tabs))
	}
	tab := tree.Windows[1].Tabs[0]
	if len(tab.Panes) != 2 {
		t.Fatalf("tab panes = %d", len(tab.Panes))
	}
	// Verify split metadata on second pane.
	split := tab.Panes[1]
	if split.ParentPaneID == nil || *split.ParentPaneID != 200 {
		t.Fatalf("split parent = %v", split.ParentPaneID)
	}
	if split.SplitDirection != "right" {
		t.Fatalf("split dir = %q", split.SplitDirection)
	}
	// Verify claude_session_id propagated.
	claudePane := tab.Panes[0]
	if claudePane.ClaudeSessionID != "sess-X" {
		t.Fatalf("claude session = %q", claudePane.ClaudeSessionID)
	}
}

func TestAttachSessionsDBAndLookupClaudeSessionByPID(t *testing.T) {
	w := testWezStoreWithSessions(t, 9876, "claude-sess-abc")

	id, err := w.LookupClaudeSessionByPID(9876)
	if err != nil {
		t.Fatalf("LookupClaudeSessionByPID: %v", err)
	}
	if id != "claude-sess-abc" {
		t.Fatalf("got id = %q", id)
	}

	// Non-matching PID returns empty without error.
	id, err = w.LookupClaudeSessionByPID(1)
	if err != nil {
		t.Fatalf("LookupClaudeSessionByPID miss: %v", err)
	}
	if id != "" {
		t.Fatalf("expected empty, got %q", id)
	}
}

func TestAttachSessionsDBIdempotent(t *testing.T) {
	w := testWezStoreWithSessions(t, 1, "s1")
	dir := t.TempDir()
	// Create a second sessions.db with a different active session.
	sessPath := filepath.Join(dir, "sessions2.db")
	s, err := Open(sessPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pid := 2
	if err := s.UpsertSession(Session{
		ID: "s2", Project: "/tmp", CWD: "/tmp",
		StartedAt: 1, LastActivity: 1, PID: &pid, Active: true,
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	_ = s.Close()

	// Re-attach to a different sessions.db.
	if err := w.AttachSessionsDB(sessPath); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	id, err := w.LookupClaudeSessionByPID(2)
	if err != nil || id != "s2" {
		t.Fatalf("after re-attach: id=%q err=%v", id, err)
	}
	// Old PID is no longer reachable.
	id, _ = w.LookupClaudeSessionByPID(1)
	if id != "" {
		t.Fatalf("old DB should be detached, got id=%q", id)
	}
}

func TestOpenWezReadOnly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wezterm.db")
	w, err := OpenWez(dbPath)
	if err != nil {
		t.Fatalf("OpenWez: %v", err)
	}
	if err := w.SetSnapshotMeta(SnapshotMeta{TakenAt: 42, ContentHash: "hash"}); err != nil {
		t.Fatalf("SetSnapshotMeta: %v", err)
	}
	_ = w.Close()

	ro, err := OpenWezReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenWezReadOnly: %v", err)
	}
	defer func() { _ = ro.Close() }()

	meta, ok, err := ro.GetSnapshotMeta()
	if err != nil || !ok {
		t.Fatalf("read meta: ok=%v err=%v", ok, err)
	}
	if meta.TakenAt != 42 {
		t.Fatalf("meta = %+v", meta)
	}

	// Write should fail.
	err = ro.SetSnapshotMeta(SnapshotMeta{TakenAt: 100, ContentHash: "x"})
	if err == nil {
		t.Fatal("expected read-only write to fail")
	}
}

// TestOpenWezReadOnlySeesUncheckpointedWAL is a regression test for the WAL
// visibility bug: a mode=ro connection cannot read un-checkpointed WAL frames,
// so `cst restore` saw an empty tree even though the daemon had just written a
// snapshot. OpenWezReadOnly now opens read-write (for -shm) with query_only, so
// it must see data that is still only in the WAL (writer kept open, no
// checkpoint).
func TestOpenWezReadOnlySeesUncheckpointedWAL(t *testing.T) {
	// Open a writer and keep it open (no checkpoint) so the inserted rows live
	// only in the -wal file when we read them back read-only.
	dir := t.TempDir()
	path := filepath.Join(dir, "wezterm.db")
	w, err := OpenWez(path)
	if err != nil {
		t.Fatalf("OpenWez: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if _, err := w.DB().Exec(
		`INSERT INTO terminal_windows (window_id, workspace, win_index) VALUES (1, 'default', 0)`,
	); err != nil {
		t.Fatalf("insert window: %v", err)
	}
	if _, err := w.DB().Exec(
		`INSERT INTO terminal_tabs (tab_id, window_id, tab_index) VALUES (10, 1, 0)`,
	); err != nil {
		t.Fatalf("insert tab: %v", err)
	}
	if _, err := w.DB().Exec(
		`INSERT INTO terminal_panes
			(pane_id, tab_id, parent_pane_id, split_direction, size_cols, size_rows,
			 cwd, title, foreground_pid, foreground_name, last_cmd, current_cmd, claude_session_id)
		 VALUES (100, 10, NULL, NULL, 80, 24, '/proj', 'pane', NULL, NULL, NULL, 'claude', 'sess-1')`,
	); err != nil {
		t.Fatalf("insert pane: %v", err)
	}

	// Read via the read-only path WITHOUT closing the writer (data is WAL-only).
	ro, err := OpenWezReadOnly(path)
	if err != nil {
		t.Fatalf("OpenWezReadOnly: %v", err)
	}
	defer func() { _ = ro.Close() }()

	tree, err := ro.ReadTree()
	if err != nil {
		t.Fatalf("ReadTree: %v", err)
	}
	if len(tree.Windows) != 1 {
		t.Fatalf("read-only open saw %d windows, want 1 (WAL data invisible?)", len(tree.Windows))
	}
	if got := len(tree.Windows[0].Tabs); got != 1 {
		t.Fatalf("tabs = %d, want 1", got)
	}
	panes := tree.Windows[0].Tabs[0].Panes
	if len(panes) != 1 || panes[0].ClaudeSessionID != "sess-1" {
		t.Fatalf("panes = %+v, want 1 pane with claude session sess-1", panes)
	}

	// Verify it really is read-only: writes must be rejected.
	if _, err := ro.DB().Exec(`INSERT INTO terminal_windows (window_id, workspace, win_index) VALUES (2, 'x', 1)`); err == nil {
		t.Error("expected write to be rejected on read-only handle")
	}
}
