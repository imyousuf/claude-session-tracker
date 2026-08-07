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
//     current_cmd, provider/session link) from pane_runtime_state into terminal_panes,
//     prune pane_runtime_state rows for closed panes belonging to muxSocket,
//     update snapshot_meta, commit.
//
// muxSocket must be the wezterm mux socket path that `panes` came from. It
// scopes the pane_runtime_state pruning so we don't disturb other wezterm
// instances' rows. Use wezterm.MuxSocket() when calling from inside wezterm.
func Sync(w *store.WezStore, raw []byte, panes []wezterm.RawPane, muxSocket string, now time.Time) (SyncResult, error) {
	res := SyncResult{}
	res.SnapshotTakenAt = now.UnixMilli()
	tree := buildTree(panes)
	var err error
	res.ContentHashHex, err = hashSnapshotContent(raw, w, tree, muxSocket)
	if err != nil {
		return res, fmt.Errorf("hash snapshot content: %w", err)
	}

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

	// Clobber guard: refuse to overwrite a meaningful saved snapshot with an
	// empty fresh-start one. When wezterm is restarted, the new (empty) instance
	// fires a snapshot before `cst restore` can run; without this guard that
	// empty layout would DELETE the saved windows/tabs/panes — destroying the
	// very thing restore is supposed to read. The incoming tree carries no
	// command/session state for its panes (the daemon hasn't seen any preexec yet
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
						last_cmd, current_cmd, session_provider, session_id, claude_session_id
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?, ?)`,
					pane.PaneID, pane.TabID,
					nullableInt64Ptr(pane.ParentPaneID), nullableString(pane.SplitDirection),
					pane.SizeCols, pane.SizeRows, pane.CWD, pane.Title,
					nullableString(rs.LastCommand),
					nullableString(rs.CurrentCommand),
					nullableString(rs.SessionProvider),
					nullableString(rs.SessionID),
					nullableString(legacyClaudeSessionID(rs)),
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

func legacyClaudeSessionID(state store.PaneRuntimeState) string {
	if store.NormalizeProvider(state.SessionProvider) == store.ProviderClaude {
		return state.SessionID
	}
	return ""
}

// incomingStats summarizes a freshly-built tree for the clobber guard. Crucially
// it joins each incoming pane against pane_runtime_state (under muxSocket) so we
// can tell whether the incoming snapshot has any live command/session state. A
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
				if rs.CurrentCommand != "" || rs.LastCommand != "" || rs.SessionID != "" {
					s.CommandedPanes++
				}
				if rs.SessionID != "" {
					s.SessionPanes++
					s.ClaudePanes = s.SessionPanes
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
//   - the saved snapshot carries real state (commands or coding sessions), and
//   - the incoming snapshot carries NO command/session state at all (the
//     empty-fresh-start signature), and
//   - the incoming snapshot is strictly smaller than what's saved.
//
// A genuine user-driven shrink (closing a pane mid-session) is preserved because
// its surviving panes still have runtime state, so incoming.CommandedPanes > 0
// and we allow the write.
func shouldSkipClobber(saved, incoming store.SnapshotStats) bool {
	if saved.CommandedPanes == 0 && linkedPaneCount(saved) == 0 {
		return false // nothing meaningful saved → always allow
	}
	if incoming.CommandedPanes > 0 || linkedPaneCount(incoming) > 0 {
		return false // incoming has real content → legitimate live update
	}
	return incoming.PaneCount < saved.PaneCount
}

func linkedPaneCount(stats store.SnapshotStats) int {
	if stats.SessionPanes > stats.ClaudePanes {
		return stats.SessionPanes
	}
	return stats.ClaudePanes
}

// buildTree groups flat panes from `wezterm cli list` into windows → tabs → panes.
// Window/tab order is the order they were first seen in the input (which is
// wezterm's stable enumeration order). Tab index is per-window. Within each tab,
// pane split structure (parent_pane_id + split_direction) is inferred from the
// captured geometry (left_col/top_row/size) by inferTabSplits.
func buildTree(panes []wezterm.RawPane) store.WezTree {
	type tabKey struct{ winID, tabID int64 }

	winSeen := map[int64]int{}  // window_id -> index in tree.Windows
	tabSeen := map[tabKey]int{} // tab_key -> index in window.Tabs

	// Collect raw panes per tab (preserving first-seen order) so geometry is in
	// hand when we infer splits.
	rawByTab := map[tabKey][]wezterm.RawPane{}

	var tree store.WezTree

	for _, p := range panes {
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

		key := tabKey{p.WindowID, p.TabID}
		if _, ok := tabSeen[key]; !ok {
			tabIdx := len(tree.Windows[winIdx].Tabs)
			tabSeen[key] = tabIdx
			tree.Windows[winIdx].Tabs = append(tree.Windows[winIdx].Tabs, store.WezTab{
				TabID:    p.TabID,
				WindowID: p.WindowID,
				TabIndex: tabIdx,
			})
		}
		rawByTab[key] = append(rawByTab[key], p)
	}

	// Second pass: build panes per tab with inferred parent/direction.
	for wi := range tree.Windows {
		win := &tree.Windows[wi]
		for ti := range win.Tabs {
			tab := &win.Tabs[ti]
			tabPanes := rawByTab[tabKey{win.WindowID, tab.TabID}]
			splits := inferTabSplits(tabPanes)
			for _, p := range tabPanes {
				s := splits[p.PaneID]
				tab.Panes = append(tab.Panes, store.WezPane{
					PaneID:         p.PaneID,
					TabID:          p.TabID,
					ParentPaneID:   s.parent,
					SplitDirection: s.dir,
					SizeCols:       p.Size.Cols,
					SizeRows:       p.Size.Rows,
					CWD:            wezterm.ParseCWD(p.CWD),
					Title:          p.Title,
					// foreground_pid/name not set: wezterm cli list doesn't expose them.
				})
			}
		}
	}
	return tree
}

// paneSplit is the inferred split relationship for one pane within its tab.
type paneSplit struct {
	parent *int64 // nil = tab lead (no parent)
	dir    string // "" for lead; else right/bottom/left/top relative to parent
}

// inferTabSplits derives each pane's parent + split direction from wezterm
// geometry (left_col/top_row/size). The top-left-most pane is the tab lead
// (parent=nil). Every other pane is attached to the nearest sibling it is
// edge-adjacent to (a divider gap of ~1 cell), preferring the sibling whose
// perpendicular band most tightly matches — so a column that is itself split
// vertically chains B→C rather than both hanging off the full-height lead.
//
// Panes that don't match any adjacency (zero/degenerate geometry, e.g. older
// wezterm or synthetic test data) fall back to "right split off the tab lead",
// which reproduces the pre-geometry behavior and never drops a pane.
func inferTabSplits(panes []wezterm.RawPane) map[int64]paneSplit {
	out := make(map[int64]paneSplit, len(panes))
	if len(panes) == 0 {
		return out
	}

	// Tab lead = smallest (top_row, left_col), tie-broken by pane_id for stability.
	leadIdx := 0
	for i := 1; i < len(panes); i++ {
		if less := paneBefore(panes[i], panes[leadIdx]); less {
			leadIdx = i
		}
	}
	lead := panes[leadIdx]
	out[lead.PaneID] = paneSplit{parent: nil, dir: ""}

	const tol = 1 // wezterm split divider is 1 cell (confirmed empirically)

	for i, p := range panes {
		if i == leadIdx {
			continue
		}
		bestParent := int64(-1)
		bestDir := ""
		bestScore := 1 << 30 // lower = tighter perpendicular band match

		for j, q := range panes {
			if j == i {
				continue
			}
			pl, pt := p.LeftCol, p.TopRow
			pc, pr := p.Size.Cols, p.Size.Rows
			ql, qt := q.LeftCol, q.TopRow
			qc, qr := q.Size.Cols, q.Size.Rows

			// P is a RIGHT split of Q: P starts just past Q's right edge and
			// their row bands overlap.
			if abs(pl-(ql+qc)) <= tol && bandsOverlap(pt, pr, qt, qr) {
				score := abs(pt-qt) + abs((pt+pr)-(qt+qr)) // row-band mismatch
				if score < bestScore {
					bestScore, bestParent, bestDir = score, q.PaneID, "right"
				}
			}
			// P is a BOTTOM split of Q: P starts just past Q's bottom edge and
			// their column bands overlap.
			if abs(pt-(qt+qr)) <= tol && bandsOverlap(pl, pc, ql, qc) {
				score := abs(pl-ql) + abs((pl+pc)-(ql+qc)) // col-band mismatch
				if score < bestScore {
					bestScore, bestParent, bestDir = score, q.PaneID, "bottom"
				}
			}
		}

		if bestParent < 0 {
			// Fallback: hang off the tab lead, split right. Reproduces the
			// pre-geometry behavior; never drops a pane.
			lp := lead.PaneID
			out[p.PaneID] = paneSplit{parent: &lp, dir: "right"}
			continue
		}
		bp := bestParent
		out[p.PaneID] = paneSplit{parent: &bp, dir: bestDir}
	}
	return out
}

// paneBefore reports whether a sorts before b by (top_row, left_col, pane_id).
func paneBefore(a, b wezterm.RawPane) bool {
	if a.TopRow != b.TopRow {
		return a.TopRow < b.TopRow
	}
	if a.LeftCol != b.LeftCol {
		return a.LeftCol < b.LeftCol
	}
	return a.PaneID < b.PaneID
}

// bandsOverlap reports whether [aStart, aStart+aLen) overlaps [bStart, bStart+bLen).
func bandsOverlap(aStart, aLen, bStart, bLen int) bool {
	return aStart < bStart+bLen && bStart < aStart+aLen
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// hashSnapshotContent includes both wezterm's raw layout and CST's per-pane
// runtime state. A SessionStart hook can change only the provider/session link
// while `wezterm cli list` remains byte-for-byte identical; hashing raw layout
// alone would incorrectly skip that snapshot and lose resumability.
func hashSnapshotContent(raw []byte, w *store.WezStore, tree store.WezTree, muxSocket string) (string, error) {
	h := sha256.New()
	_, _ = h.Write(raw)
	if muxSocket != "" {
		for _, win := range tree.Windows {
			for _, tab := range win.Tabs {
				for _, pane := range tab.Panes {
					state, ok, err := w.LookupPaneRuntime(muxSocket, pane.PaneID)
					if err != nil {
						return "", err
					}
					if !ok {
						continue
					}
					_, _ = fmt.Fprintf(h, "\x00%d\x00%s\x00%d\x00%s\x00%d\x00%s\x00%s\x00%s",
						pane.PaneID, state.CurrentCommand, state.CurrentStartedAt,
						state.LastCommand, state.LastFinishedAt, state.CWD,
						state.SessionProvider, state.SessionID)
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// lookupRuntimeInTx reads pane_runtime_state inside an open transaction.
// Returns zero-value PaneRuntimeState (no error) if the row doesn't exist.
func lookupRuntimeInTx(tx *sql.Tx, muxSocket string, paneID int64) (store.PaneRuntimeState, error) {
	var s store.PaneRuntimeState
	var curCmd, lastCmd, provider, sessionID sql.NullString
	var curStarted, lastFinished sql.NullInt64
	err := tx.QueryRow(`
		SELECT mux_socket, pane_id, current_command, current_started_at,
			last_command, last_finished_at, cwd, session_provider, session_id
		FROM pane_runtime_state WHERE mux_socket = ? AND pane_id = ?
	`, muxSocket, paneID).Scan(
		&s.MuxSocket, &s.PaneID,
		&curCmd, &curStarted, &lastCmd, &lastFinished, &s.CWD, &provider, &sessionID,
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
	if provider.Valid {
		s.SessionProvider = store.NormalizeProvider(provider.String)
	}
	if sessionID.Valid {
		s.SessionID = sessionID.String
		s.SessionProvider = store.NormalizeProvider(s.SessionProvider)
		if s.SessionProvider == store.ProviderClaude {
			s.ClaudeSessionID = sessionID.String
		}
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

func nullableInt64Ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
