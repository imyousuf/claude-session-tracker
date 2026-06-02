// Package snapshot implements the wezterm-state sync and restore logic.
//
// Sync takes a `wezterm cli list` payload and atomically replaces the stored
// snapshot in wezterm.db. Closed windows/tabs/panes get deleted; pane_runtime_state
// is pruned for closed panes. Hash-skip avoids rewriting unchanged trees.
//
// Restore (see restore.go) reads the latest stored snapshot and reconstructs the
// wezterm layout via `wezterm cli spawn`.
package snapshot

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

// SyncResult reports what a Sync call did.
type SyncResult struct {
	DidWork         bool // false if hash-skipped
	SkippedClobber  bool // true if the incoming snapshot was refused (clobber guard)
	WindowsWritten  int
	TabsWritten     int
	PanesWritten    int
	RuntimePruned   int
	ContentHashHex  string
	SnapshotTakenAt int64
}

// Sync syncs a parsed `wezterm cli list` payload into the WezStore.
//
//   - If `raw`'s content hash matches the previous snapshot, only the timestamp
//     is touched and SyncResult.DidWork is false.
//   - Otherwise: open a transaction, wipe terminal_windows (cascades), insert
//     fresh windows/tabs/panes, copy per-pane runtime state (last_cmd,
//     current_cmd, claude_session_id) from pane_runtime_state into terminal_panes,
//     prune pane_runtime_state rows for closed panes belonging to muxSocket,
//     update snapshot_meta, commit.
//
// muxSocket must be the wezterm mux socket path that `panes` came from. It
// scopes the pane_runtime_state pruning so we don't disturb other wezterm
// instances' rows. Use wezterm.MuxSocket() when calling from inside wezterm.
func Sync(w *store.WezStore, raw []byte, panes []wezterm.RawPane, muxSocket string, now time.Time) (SyncResult, error) {
	res := SyncResult{}
	res.ContentHashHex = hashContent(raw)
	res.SnapshotTakenAt = now.UnixMilli()

	prev, hasPrev, err := w.GetSnapshotMeta()
	if err != nil {
		return res, fmt.Errorf("get snapshot meta: %w", err)
	}
	if hasPrev && prev.ContentHash == res.ContentHashHex {
		if err := w.TouchSnapshotTimestamp(res.SnapshotTakenAt); err != nil {
			return res, fmt.Errorf("touch snapshot meta: %w", err)
		}
		return res, nil
	}

	tree := buildTree(panes)

	// Clobber guard: refuse to overwrite a meaningful saved snapshot with an
	// empty fresh-start one. When wezterm is restarted, the new (empty) instance
	// fires a snapshot before `cst restore` can run; without this guard that
	// empty layout would DELETE the saved windows/tabs/panes — destroying the
	// very thing restore is supposed to read. The incoming tree carries no
	// command/claude state for its panes (the daemon hasn't seen any preexec yet
	// on a fresh start), so we detect "smaller AND command-less" and skip.
	if hasPrev {
		saved, err := w.GetSnapshotStats()
		if err != nil {
			return res, fmt.Errorf("get snapshot stats: %w", err)
		}
		if shouldSkipClobber(saved, incomingStats(w, tree, muxSocket)) {
			res.SkippedClobber = true
			// Keep the saved snapshot; just refresh the timestamp so the daemon's
			// debounce/age tracking still advances.
			if err := w.TouchSnapshotTimestamp(res.SnapshotTakenAt); err != nil {
				return res, fmt.Errorf("touch snapshot meta: %w", err)
			}
			return res, nil
		}
	}

	db := w.DB()
	tx, err := db.Begin()
	if err != nil {
		return res, fmt.Errorf("begin sync tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// 1. Wipe current snapshot. Cascades to tabs and panes.
	if _, err := tx.Exec(`DELETE FROM terminal_windows`); err != nil {
		return res, fmt.Errorf("wipe windows: %w", err)
	}

	// 2. Insert fresh windows, tabs, panes; collect live pane IDs for pruning.
	livePaneIDs := make([]int64, 0, len(panes))
	for _, win := range tree.Windows {
		if _, err := tx.Exec(
			`INSERT INTO terminal_windows (window_id, workspace, win_index) VALUES (?, ?, ?)`,
			win.WindowID, win.Workspace, win.WinIndex,
		); err != nil {
			return res, fmt.Errorf("insert window %d: %w", win.WindowID, err)
		}
		res.WindowsWritten++

		for _, tab := range win.Tabs {
			if _, err := tx.Exec(
				`INSERT INTO terminal_tabs (tab_id, window_id, tab_index) VALUES (?, ?, ?)`,
				tab.TabID, tab.WindowID, tab.TabIndex,
			); err != nil {
				return res, fmt.Errorf("insert tab %d: %w", tab.TabID, err)
			}
			res.TabsWritten++

			for _, pane := range tab.Panes {
				rs, err := lookupRuntimeInTx(tx, muxSocket, pane.PaneID)
				if err != nil {
					return res, err
				}
				if _, err := tx.Exec(
					`INSERT INTO terminal_panes (
						pane_id, tab_id, parent_pane_id, split_direction,
						size_cols, size_rows, cwd, title,
						foreground_pid, foreground_name,
						last_cmd, current_cmd, claude_session_id
					) VALUES (?, ?, NULL, NULL, ?, ?, ?, ?, NULL, NULL, ?, ?, ?)`,
					pane.PaneID, pane.TabID,
					pane.SizeCols, pane.SizeRows, pane.CWD, pane.Title,
					nullableString(rs.LastCommand),
					nullableString(rs.CurrentCommand),
					nullableString(rs.ClaudeSessionID),
				); err != nil {
					return res, fmt.Errorf("insert pane %d: %w", pane.PaneID, err)
				}
				res.PanesWritten++
				livePaneIDs = append(livePaneIDs, pane.PaneID)
			}
		}
	}

	// 3. Prune pane_runtime_state for closed panes in this mux.
	if muxSocket != "" {
		pruned, err := prunePaneRuntimeInTx(tx, muxSocket, livePaneIDs)
		if err != nil {
			return res, err
		}
		res.RuntimePruned = pruned
	}

	// 4. Update snapshot_meta.
	if _, err := tx.Exec(
		`INSERT INTO snapshot_meta (id, taken_at, content_hash) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET taken_at = excluded.taken_at, content_hash = excluded.content_hash`,
		res.SnapshotTakenAt, res.ContentHashHex,
	); err != nil {
		return res, fmt.Errorf("update snapshot meta: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit sync: %w", err)
	}
	committed = true
	res.DidWork = true
	return res, nil
}

// incomingStats summarizes a freshly-built tree for the clobber guard. Crucially
// it joins each incoming pane against pane_runtime_state (under muxSocket) so we
// can tell whether the incoming snapshot has any live command/claude state. A
// freshly-restarted, empty wezterm has none (no preexec seen yet); a live
// session that just lost a pane still has runtime state on its survivors.
func incomingStats(w *store.WezStore, tree store.WezTree, muxSocket string) store.SnapshotStats {
	var s store.SnapshotStats
	for _, win := range tree.Windows {
		for _, tab := range win.Tabs {
			for _, pane := range tab.Panes {
				s.PaneCount++
				if muxSocket == "" {
					continue
				}
				rs, ok, err := w.LookupPaneRuntime(muxSocket, pane.PaneID)
				if err != nil || !ok {
					continue
				}
				if rs.CurrentCommand != "" || rs.LastCommand != "" || rs.ClaudeSessionID != "" {
					s.CommandedPanes++
				}
				if rs.ClaudeSessionID != "" {
					s.ClaudePanes++
				}
			}
		}
	}
	return s
}

// shouldSkipClobber decides whether to refuse an incoming snapshot so it can't
// destroy a meaningful saved layout. This is the fix for the startup race where
// a freshly-restarted (empty) wezterm snapshots before `cst restore` runs and
// would otherwise DELETE the saved windows/tabs/panes.
//
// Refuse only the unambiguous clobber:
//   - the saved snapshot carries real state (commands or claude sessions), and
//   - the incoming snapshot carries NO command/claude state at all (the
//     empty-fresh-start signature), and
//   - the incoming snapshot is strictly smaller than what's saved.
//
// A genuine user-driven shrink (closing a pane mid-session) is preserved because
// its surviving panes still have runtime state, so incoming.CommandedPanes > 0
// and we allow the write.
func shouldSkipClobber(saved, incoming store.SnapshotStats) bool {
	if saved.CommandedPanes == 0 && saved.ClaudePanes == 0 {
		return false // nothing meaningful saved → always allow
	}
	if incoming.CommandedPanes > 0 || incoming.ClaudePanes > 0 {
		return false // incoming has real content → legitimate live update
	}
	return incoming.PaneCount < saved.PaneCount
}

// buildTree groups flat panes from `wezterm cli list` into windows → tabs → panes.
// Window/tab order is the order they were first seen in the input (which is
// wezterm's stable enumeration order). Tab index is per-window.
func buildTree(panes []wezterm.RawPane) store.WezTree {
	type tabKey struct{ winID, tabID int64 }

	winSeen := map[int64]int{}  // window_id -> index in tree.Windows
	tabSeen := map[tabKey]int{} // tab_key -> index in window.Tabs

	var tree store.WezTree

	for _, p := range panes {
		// Window: create if first time seen.
		winIdx, ok := winSeen[p.WindowID]
		if !ok {
			winIdx = len(tree.Windows)
			winSeen[p.WindowID] = winIdx
			tree.Windows = append(tree.Windows, store.WezWindow{
				WindowID:  p.WindowID,
				Workspace: p.Workspace,
				WinIndex:  winIdx,
			})
		}

		// Tab: create if first time seen.
		key := tabKey{p.WindowID, p.TabID}
		tabIdx, ok := tabSeen[key]
		if !ok {
			tabIdx = len(tree.Windows[winIdx].Tabs)
			tabSeen[key] = tabIdx
			tree.Windows[winIdx].Tabs = append(tree.Windows[winIdx].Tabs, store.WezTab{
				TabID:    p.TabID,
				WindowID: p.WindowID,
				TabIndex: tabIdx,
			})
		}

		// Pane.
		tree.Windows[winIdx].Tabs[tabIdx].Panes = append(
			tree.Windows[winIdx].Tabs[tabIdx].Panes,
			store.WezPane{
				PaneID:   p.PaneID,
				TabID:    p.TabID,
				SizeCols: p.Size.Cols,
				SizeRows: p.Size.Rows,
				CWD:      wezterm.ParseCWD(p.CWD),
				Title:    p.Title,
				// foreground_pid/name not set: wezterm cli list doesn't expose them.
				// Splits not detected here either; left for v2.
			},
		)
	}
	return tree
}

func hashContent(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// lookupRuntimeInTx reads pane_runtime_state inside an open transaction.
// Returns zero-value PaneRuntimeState (no error) if the row doesn't exist.
func lookupRuntimeInTx(tx *sql.Tx, muxSocket string, paneID int64) (store.PaneRuntimeState, error) {
	var s store.PaneRuntimeState
	var curCmd, lastCmd, claudeID sql.NullString
	var curStarted, lastFinished sql.NullInt64
	err := tx.QueryRow(`
		SELECT mux_socket, pane_id, current_command, current_started_at,
			last_command, last_finished_at, cwd, claude_session_id
		FROM pane_runtime_state WHERE mux_socket = ? AND pane_id = ?
	`, muxSocket, paneID).Scan(
		&s.MuxSocket, &s.PaneID,
		&curCmd, &curStarted, &lastCmd, &lastFinished, &s.CWD, &claudeID,
	)
	if err == sql.ErrNoRows {
		return store.PaneRuntimeState{}, nil
	}
	if err != nil {
		return s, fmt.Errorf("lookup runtime for pane %d: %w", paneID, err)
	}
	if curCmd.Valid {
		s.CurrentCommand = curCmd.String
	}
	if lastCmd.Valid {
		s.LastCommand = lastCmd.String
	}
	if claudeID.Valid {
		s.ClaudeSessionID = claudeID.String
	}
	if curStarted.Valid {
		s.CurrentStartedAt = curStarted.Int64
	}
	if lastFinished.Valid {
		s.LastFinishedAt = lastFinished.Int64
	}
	return s, nil
}

// prunePaneRuntimeInTx deletes pane_runtime_state rows whose pane_id is NOT in
// livePaneIDs, scoped to the given mux_socket.
func prunePaneRuntimeInTx(tx *sql.Tx, muxSocket string, livePaneIDs []int64) (int, error) {
	if len(livePaneIDs) == 0 {
		res, err := tx.Exec(
			`DELETE FROM pane_runtime_state WHERE mux_socket = ?`,
			muxSocket,
		)
		if err != nil {
			return 0, fmt.Errorf("prune runtime (all): %w", err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}

	placeholders := ""
	args := []any{muxSocket}
	for i, id := range livePaneIDs {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	q := fmt.Sprintf(
		`DELETE FROM pane_runtime_state WHERE mux_socket = ? AND pane_id NOT IN (%s)`,
		placeholders,
	)
	res, err := tx.Exec(q, args...)
	if err != nil {
		return 0, fmt.Errorf("prune runtime: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
