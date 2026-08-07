package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const DefaultWezDBName = "wezterm.db"

//go:embed wezterm_schema.sql
var wezSchemaSQL string

// WezPane represents a single wezterm pane as stored in wezterm.db.
type WezPane struct {
	PaneID          int64
	TabID           int64
	ParentPaneID    *int64
	SplitDirection  string
	SizeCols        int
	SizeRows        int
	CWD             string
	Title           string
	ForegroundPID   *int
	ForegroundName  string
	LastCmd         string
	CurrentCmd      string
	SessionProvider string
	SessionID       string
	// ClaudeSessionID is retained for source compatibility with integrations
	// built before provider-aware session links. New code should use the two
	// fields above.
	ClaudeSessionID string
}

// WezTab represents a wezterm tab and its panes.
type WezTab struct {
	TabID    int64
	WindowID int64
	TabIndex int
	Panes    []WezPane
}

// WezWindow represents a wezterm window and its tabs.
type WezWindow struct {
	WindowID  int64
	Workspace string
	WinIndex  int
	Tabs      []WezTab
}

// WezTree is the full snapshot of wezterm state.
type WezTree struct {
	Windows []WezWindow
}

// PaneRuntimeState is the long-lived per-pane state written by shell hooks
// and the SessionStart/End hooks.
type PaneRuntimeState struct {
	MuxSocket        string
	PaneID           int64
	CurrentCommand   string
	CurrentStartedAt int64
	LastCommand      string
	LastFinishedAt   int64
	CWD              string
	SessionProvider  string // bound by SessionStart hook; cleared by SessionEnd
	SessionID        string
	ClaudeSessionID  string // deprecated compatibility alias
}

// SnapshotMeta is the single-row snapshot metadata used for hash-skip and debounce.
type SnapshotMeta struct {
	TakenAt     int64
	ContentHash string
}

// WezStore wraps the SQLite database for wezterm layout tracking.
// Owned exclusively by the cst daemon. Other processes may open it read-only
// (e.g. `cst restore` reads it at startup).
type WezStore struct {
	db *sql.DB
}

// DefaultWezDBPath returns the default wezterm.db path (~/.cst/wezterm.db).
func DefaultWezDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, DefaultDBDir, DefaultWezDBName)
}

// OpenWez opens or creates the wezterm tracking database at the given path.
func OpenWez(dbPath string) (*WezStore, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open wezterm.db: %w", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping wezterm.db: %w", err)
	}

	w := &WezStore{db: db}
	if _, err := w.db.Exec(wezSchemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply wezterm schema: %w", err)
	}
	if err := w.migrateSessionColumns(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate wezterm session columns: %w", err)
	}

	return w, nil
}

func (w *WezStore) migrateSessionColumns() error {
	for _, table := range []string{"terminal_panes", "pane_runtime_state"} {
		for _, column := range []string{"session_provider", "session_id"} {
			if err := ensureColumn(w.db, table, column, "TEXT"); err != nil {
				return err
			}
		}
		if _, err := w.db.Exec(`UPDATE ` + table + `
			SET session_provider = 'claude', session_id = claude_session_id
			WHERE session_id IS NULL AND claude_session_id IS NOT NULL`); err != nil {
			return err
		}
	}
	return nil
}

// OpenWezReadOnly opens wezterm.db for read-only access (used by `cst restore`).
//
// IMPORTANT: we deliberately do NOT use mode=ro. The database is in WAL mode and
// the daemon's most recent snapshot usually lives in the -wal file, not yet
// checkpointed into the main db file. A mode=ro connection cannot read
// un-checkpointed WAL frames — reading the WAL requires creating/writing the
// -shm shared-memory index, which read-only access forbids — so it silently
// falls back to the stale main-file checkpoint (often zero rows). Restore would
// then see an empty tree and spawn nothing.
//
// Instead we open with normal (read-write-capable) file access so SQLite can
// build the -shm index and read the live WAL, and set query_only(true) to forbid
// any writes. This gives true read-only semantics while still seeing the
// daemon's latest committed snapshot. WAL allows our reader to coexist with the
// daemon's single writer.
func OpenWezReadOnly(dbPath string) (*WezStore, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("wezterm.db not found at %s: %w", dbPath, err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=query_only(true)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open wezterm.db read-only: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping wezterm.db: %w", err)
	}
	return &WezStore{db: db}, nil
}

// Close closes the database connection.
func (w *WezStore) Close() error {
	return w.db.Close()
}

// DB exposes the underlying *sql.DB. Used by the daemon for transactions
// that span multiple methods (e.g. snapshot sync).
func (w *WezStore) DB() *sql.DB {
	return w.db
}

// AttachSessionsDB attaches a read-only view of sessions.db as "sessions" so
// the daemon can reconcile provider/session links written by older versions.
//
// Idempotent: safe to call multiple times; subsequent calls re-attach.
func (w *WezStore) AttachSessionsDB(sessionsDBPath string) error {
	// Detach if already attached (ignore error if not attached).
	_, _ = w.db.Exec(`DETACH DATABASE sessions`)
	_, err := w.db.Exec(fmt.Sprintf(`ATTACH DATABASE 'file:%s?mode=ro' AS sessions`, sessionsDBPath))
	if err != nil {
		return fmt.Errorf("attach sessions.db: %w", err)
	}
	return w.reconcileAttachedSessionProviders()
}

// reconcileAttachedSessionProviders corrects pane links written by older
// daemons that had only claude_session_id. sessions.db is authoritative when a
// matching ID belongs to Codex (or another future provider).
func (w *WezStore) reconcileAttachedSessionProviders() error {
	for _, table := range []string{"terminal_panes", "pane_runtime_state"} {
		if _, err := w.db.Exec(`UPDATE ` + table + `
			SET session_provider = (
				SELECT provider FROM sessions.sessions s WHERE s.id = session_id
			),
			claude_session_id = CASE WHEN (
				SELECT provider FROM sessions.sessions s WHERE s.id = session_id
			) = 'claude' THEN session_id ELSE NULL END
			WHERE session_id IS NOT NULL
			  AND EXISTS (SELECT 1 FROM sessions.sessions s WHERE s.id = session_id)`); err != nil {
			return fmt.Errorf("reconcile %s session providers: %w", table, err)
		}
	}
	return nil
}

// LookupSessionByPID returns the active coding-agent session owning the given
// PID. Empty values mean no match. Requires AttachSessionsDB to have run.
func (w *WezStore) LookupSessionByPID(pid int) (provider, id string, err error) {
	err = w.db.QueryRow(
		`SELECT provider, id FROM sessions.sessions WHERE pid = ? AND active = 1 LIMIT 1`,
		pid,
	).Scan(&provider, &id)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return NormalizeProvider(provider), id, nil
}

// LookupClaudeSessionByPID is the pre-provider compatibility wrapper. It
// returns a value only when the owning session is a Claude session.
func (w *WezStore) LookupClaudeSessionByPID(pid int) (string, error) {
	provider, id, err := w.LookupSessionByPID(pid)
	if err != nil || provider != ProviderClaude {
		return "", err
	}
	return id, nil
}

// --- Snapshot meta ---

// GetSnapshotMeta returns the current snapshot meta row. ok=false if none exists.
func (w *WezStore) GetSnapshotMeta() (SnapshotMeta, bool, error) {
	var m SnapshotMeta
	err := w.db.QueryRow(`SELECT taken_at, content_hash FROM snapshot_meta WHERE id = 1`).
		Scan(&m.TakenAt, &m.ContentHash)
	if err == sql.ErrNoRows {
		return SnapshotMeta{}, false, nil
	}
	if err != nil {
		return SnapshotMeta{}, false, err
	}
	return m, true, nil
}

// SnapshotStats summarizes the currently-stored snapshot tree. Used by Sync's
// clobber guard to decide whether an incoming (possibly empty) snapshot should
// be allowed to replace what's saved.
type SnapshotStats struct {
	PaneCount      int // total terminal_panes rows
	CommandedPanes int // panes with current_cmd, last_cmd, or session_id set
	SessionPanes   int // panes with a linked coding-agent session
	ClaudePanes    int // deprecated compatibility alias for SessionPanes
}

// GetSnapshotStats returns counts describing the stored snapshot. A zero-value
// SnapshotStats (all zero) means there is no meaningful saved layout.
func (w *WezStore) GetSnapshotStats() (SnapshotStats, error) {
	var s SnapshotStats
	err := w.db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN current_cmd IS NOT NULL
			                    OR last_cmd IS NOT NULL
			                    OR session_id IS NOT NULL
			                    OR claude_session_id IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN session_id IS NOT NULL
			                    OR claude_session_id IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM terminal_panes
	`).Scan(&s.PaneCount, &s.CommandedPanes, &s.SessionPanes)
	if err != nil {
		return SnapshotStats{}, err
	}
	s.ClaudePanes = s.SessionPanes
	return s, nil
}

// SetSnapshotMeta upserts the single snapshot_meta row.
func (w *WezStore) SetSnapshotMeta(m SnapshotMeta) error {
	_, err := w.db.Exec(`
		INSERT INTO snapshot_meta (id, taken_at, content_hash) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET taken_at = excluded.taken_at, content_hash = excluded.content_hash
	`, m.TakenAt, m.ContentHash)
	return err
}

// TouchSnapshotTimestamp updates only the taken_at timestamp without changing the hash.
// Used when a snapshot was triggered but the content hash matched the previous snapshot.
func (w *WezStore) TouchSnapshotTimestamp(takenAt int64) error {
	_, err := w.db.Exec(`UPDATE snapshot_meta SET taken_at = ? WHERE id = 1`, takenAt)
	return err
}

// --- Pane runtime state (written by shell hooks via the daemon) ---

// UpsertPaneRuntime replaces a full pane_runtime_state row.
func (w *WezStore) UpsertPaneRuntime(s PaneRuntimeState) error {
	if s.SessionID == "" && s.ClaudeSessionID != "" {
		s.SessionProvider = ProviderClaude
		s.SessionID = s.ClaudeSessionID
	}
	if s.SessionID != "" {
		s.SessionProvider = NormalizeProvider(s.SessionProvider)
	}
	_, err := w.db.Exec(`
		INSERT INTO pane_runtime_state
			(mux_socket, pane_id, current_command, current_started_at, last_command, last_finished_at, cwd, session_provider, session_id, claude_session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(mux_socket, pane_id) DO UPDATE SET
			current_command    = excluded.current_command,
			current_started_at = excluded.current_started_at,
			last_command       = excluded.last_command,
			last_finished_at   = excluded.last_finished_at,
			cwd                = excluded.cwd,
			session_provider   = excluded.session_provider,
			session_id         = excluded.session_id,
			claude_session_id  = excluded.claude_session_id
	`,
		s.MuxSocket, s.PaneID,
		nullableString(s.CurrentCommand), nullableInt64(s.CurrentStartedAt),
		nullableString(s.LastCommand), nullableInt64(s.LastFinishedAt),
		s.CWD, nullableString(s.SessionProvider), nullableString(s.SessionID),
		nullableString(claudeSessionID(s.SessionProvider, s.SessionID)),
	)
	return err
}

// BindSession links a coding-agent session to a pane. CWD is required so the
// SessionStart hook can create the runtime row before a shell preexec arrives.
func (w *WezStore) BindSession(muxSocket string, paneID int64, provider, sessionID, cwd string) error {
	provider = NormalizeProvider(provider)
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// A session can only be attached to one client pane. This also repairs
	// legacy Codex links created from the app-server's stale WEZTERM_PANE.
	if _, err := tx.Exec(`
		UPDATE pane_runtime_state
		SET session_provider = NULL, session_id = NULL, claude_session_id = NULL
		WHERE session_provider = ? AND session_id = ?
		  AND NOT (mux_socket = ? AND pane_id = ?)
	`, provider, sessionID, muxSocket, paneID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO pane_runtime_state (mux_socket, pane_id, cwd, session_provider, session_id, claude_session_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(mux_socket, pane_id) DO UPDATE SET
			session_provider = excluded.session_provider,
			session_id       = excluded.session_id,
			claude_session_id = excluded.claude_session_id,
			cwd              = excluded.cwd
	`, muxSocket, paneID, cwd, provider, sessionID,
		nullableString(claudeSessionID(provider, sessionID))); err != nil {
		return err
	}
	return tx.Commit()
}

// BindClaudeSession is the pre-provider compatibility wrapper.
func (w *WezStore) BindClaudeSession(muxSocket string, paneID int64, sessionID, cwd string) error {
	return w.BindSession(muxSocket, paneID, ProviderClaude, sessionID, cwd)
}

// UnbindSession clears a provider/session link. Matching the session id keeps a
// delayed SessionEnd event from clearing a newer session in the same pane.
func (w *WezStore) UnbindSession(muxSocket string, paneID int64, provider, sessionID string) error {
	_, err := w.db.Exec(`
		UPDATE pane_runtime_state
		SET session_provider = NULL, session_id = NULL, claude_session_id = NULL
		WHERE mux_socket = ? AND pane_id = ?
		  AND session_provider = ?
		  AND (? = '' OR session_id = ?)
	`, muxSocket, paneID, NormalizeProvider(provider), sessionID, sessionID)
	return err
}

// UnbindClaudeSession is the pre-provider compatibility wrapper.
func (w *WezStore) UnbindClaudeSession(muxSocket string, paneID int64) error {
	return w.UnbindSession(muxSocket, paneID, ProviderClaude, "")
}

// ApplyPreexec sets current_command and cwd; called from `cst hook preexec`.
// Preserves last_command (the previous completed command).
func (w *WezStore) ApplyPreexec(muxSocket string, paneID int64, command, cwd string, startedAt int64) error {
	_, err := w.db.Exec(`
		INSERT INTO pane_runtime_state
			(mux_socket, pane_id, current_command, current_started_at, last_command, last_finished_at, cwd)
		VALUES (?, ?, ?, ?, NULL, NULL, ?)
		ON CONFLICT(mux_socket, pane_id) DO UPDATE SET
			current_command    = excluded.current_command,
			current_started_at = excluded.current_started_at,
			cwd                = excluded.cwd
	`, muxSocket, paneID, command, startedAt, cwd)
	return err
}

// ApplyPrecmd clears current_command and moves it to last_command; called from
// `cst hook precmd` when a command has just finished and bash returned to the prompt.
func (w *WezStore) ApplyPrecmd(muxSocket string, paneID int64, cwd string, finishedAt int64) error {
	// Read previous current_command so we can move it into last_command.
	var prev sql.NullString
	err := w.db.QueryRow(`
		SELECT current_command FROM pane_runtime_state WHERE mux_socket = ? AND pane_id = ?
	`, muxSocket, paneID).Scan(&prev)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if prev.Valid && prev.String != "" {
		_, err = w.db.Exec(`
			INSERT INTO pane_runtime_state
				(mux_socket, pane_id, current_command, current_started_at, last_command, last_finished_at, cwd)
			VALUES (?, ?, NULL, NULL, ?, ?, ?)
			ON CONFLICT(mux_socket, pane_id) DO UPDATE SET
				current_command    = NULL,
				current_started_at = NULL,
				last_command       = excluded.last_command,
				last_finished_at   = excluded.last_finished_at,
				cwd                = excluded.cwd
		`, muxSocket, paneID, prev.String, finishedAt, cwd)
		return err
	}

	// No prior current_command. Just update cwd.
	_, err = w.db.Exec(`
		INSERT INTO pane_runtime_state (mux_socket, pane_id, cwd)
		VALUES (?, ?, ?)
		ON CONFLICT(mux_socket, pane_id) DO UPDATE SET cwd = excluded.cwd
	`, muxSocket, paneID, cwd)
	return err
}

// LookupPaneRuntime returns the runtime state for a pane. ok=false if not tracked.
func (w *WezStore) LookupPaneRuntime(muxSocket string, paneID int64) (PaneRuntimeState, bool, error) {
	var s PaneRuntimeState
	var curCmd, lastCmd, provider, sessionID sql.NullString
	var curStarted, lastFinished sql.NullInt64
	err := w.db.QueryRow(`
		SELECT mux_socket, pane_id, current_command, current_started_at,
			last_command, last_finished_at, cwd, session_provider, session_id
		FROM pane_runtime_state WHERE mux_socket = ? AND pane_id = ?
	`, muxSocket, paneID).Scan(
		&s.MuxSocket, &s.PaneID,
		&curCmd, &curStarted, &lastCmd, &lastFinished, &s.CWD, &provider, &sessionID,
	)
	if err == sql.ErrNoRows {
		return PaneRuntimeState{}, false, nil
	}
	if err != nil {
		return PaneRuntimeState{}, false, err
	}
	if curCmd.Valid {
		s.CurrentCommand = curCmd.String
	}
	if curStarted.Valid {
		s.CurrentStartedAt = curStarted.Int64
	}
	if lastCmd.Valid {
		s.LastCommand = lastCmd.String
	}
	if lastFinished.Valid {
		s.LastFinishedAt = lastFinished.Int64
	}
	if provider.Valid {
		s.SessionProvider = NormalizeProvider(provider.String)
	}
	if sessionID.Valid {
		s.SessionID = sessionID.String
		s.SessionProvider = NormalizeProvider(s.SessionProvider)
		if s.SessionProvider == ProviderClaude {
			s.ClaudeSessionID = sessionID.String
		}
	}
	return s, true, nil
}

// InferMuxSocket returns the mux_socket of the most-recently-touched
// pane_runtime_state row. Used by the daemon to discover the wezterm mux
// socket when its own env doesn't have $WEZTERM_UNIX_SOCKET (e.g. when the
// daemon is started by systemd-user, which inherits no wezterm env vars).
//
// Returns "" with nil error if no runtime state exists yet.
func (w *WezStore) InferMuxSocket() (string, error) {
	var sock string
	err := w.db.QueryRow(`
		SELECT mux_socket
		FROM pane_runtime_state
		ORDER BY COALESCE(last_finished_at, current_started_at, 0) DESC
		LIMIT 1
	`).Scan(&sock)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return sock, nil
}

// PruneDeadMux deletes all pane_runtime_state rows belonging to a mux socket
// that no longer exists. Called by the daemon when it detects a dead socket.
func (w *WezStore) PruneDeadMux(muxSocket string) (int, error) {
	res, err := w.db.Exec(`DELETE FROM pane_runtime_state WHERE mux_socket = ?`, muxSocket)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PrunePaneRuntimeNotIn deletes rows for the given mux_socket whose pane_id is
// not in the supplied set. Called at the end of a snapshot sync.
func (w *WezStore) PrunePaneRuntimeNotIn(muxSocket string, livePaneIDs []int64) (int, error) {
	if len(livePaneIDs) == 0 {
		return w.PruneDeadMux(muxSocket)
	}
	// Build (?, ?, ?) list for IN clause.
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
	res, err := w.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// --- Tree read (used by cst restore) ---

// ReadTree returns the full snapshot tree from wezterm.db.
// Windows, tabs, and panes are returned in their stored order.
//
// Implementation note: we collect rows into temporary maps keyed by ID, then
// assemble the final tree in one pass. Earlier versions kept *WezWindow /
// *WezTab pointers across appends, which silently broke when slice growth
// relocated the backing arrays.
func (w *WezStore) ReadTree() (WezTree, error) {
	tree := WezTree{}

	// 1. Windows (ordered).
	winRows, err := w.db.Query(`SELECT window_id, workspace, win_index FROM terminal_windows ORDER BY win_index, window_id`)
	if err != nil {
		return tree, fmt.Errorf("query windows: %w", err)
	}
	var orderedWindowIDs []int64
	windows := map[int64]WezWindow{}
	for winRows.Next() {
		var win WezWindow
		if err := winRows.Scan(&win.WindowID, &win.Workspace, &win.WinIndex); err != nil {
			_ = winRows.Close()
			return tree, err
		}
		windows[win.WindowID] = win
		orderedWindowIDs = append(orderedWindowIDs, win.WindowID)
	}
	if err := winRows.Err(); err != nil {
		_ = winRows.Close()
		return tree, err
	}
	_ = winRows.Close()

	// 2. Tabs (grouped by window, ordered within).
	tabRows, err := w.db.Query(`SELECT tab_id, window_id, tab_index FROM terminal_tabs ORDER BY window_id, tab_index, tab_id`)
	if err != nil {
		return tree, fmt.Errorf("query tabs: %w", err)
	}
	tabsByWindow := map[int64][]WezTab{}
	tabIndex := map[int64]struct {
		winID, idxInWindow int64
	}{}
	for tabRows.Next() {
		var tab WezTab
		if err := tabRows.Scan(&tab.TabID, &tab.WindowID, &tab.TabIndex); err != nil {
			_ = tabRows.Close()
			return tree, err
		}
		tabsByWindow[tab.WindowID] = append(tabsByWindow[tab.WindowID], tab)
		tabIndex[tab.TabID] = struct {
			winID, idxInWindow int64
		}{tab.WindowID, int64(len(tabsByWindow[tab.WindowID]) - 1)}
	}
	if err := tabRows.Err(); err != nil {
		_ = tabRows.Close()
		return tree, err
	}
	_ = tabRows.Close()

	// 3. Panes (grouped by tab). Read-only restore may be the first command run
	// after upgrading CST, so tolerate the old schema without requiring a write
	// migration first.
	hasProvider, err := tableHasColumn(w.db, "terminal_panes", "session_provider")
	if err != nil {
		return tree, fmt.Errorf("inspect pane session provider column: %w", err)
	}
	hasSessionID, err := tableHasColumn(w.db, "terminal_panes", "session_id")
	if err != nil {
		return tree, fmt.Errorf("inspect pane session id column: %w", err)
	}
	paneQuery := `
		SELECT pane_id, tab_id, parent_pane_id, split_direction,
			size_cols, size_rows, cwd, title, foreground_pid, foreground_name,
			last_cmd, current_cmd,
			COALESCE(session_provider, CASE WHEN claude_session_id IS NOT NULL THEN 'claude' END),
			COALESCE(session_id, claude_session_id)
		FROM terminal_panes
		ORDER BY tab_id, pane_id
	`
	if !hasProvider || !hasSessionID {
		paneQuery = `
			SELECT pane_id, tab_id, parent_pane_id, split_direction,
				size_cols, size_rows, cwd, title, foreground_pid, foreground_name,
				last_cmd, current_cmd,
				CASE WHEN claude_session_id IS NOT NULL THEN 'claude' END,
				claude_session_id
			FROM terminal_panes
			ORDER BY tab_id, pane_id
		`
	}
	paneRows, err := w.db.Query(paneQuery)
	if err != nil {
		return tree, fmt.Errorf("query panes: %w", err)
	}
	panesByTab := map[int64][]WezPane{}
	for paneRows.Next() {
		var p WezPane
		var parent sql.NullInt64
		var split, title, fgName, lastCmd, currentCmd, provider, sessionID sql.NullString
		var fgPID sql.NullInt64
		if err := paneRows.Scan(
			&p.PaneID, &p.TabID, &parent, &split,
			&p.SizeCols, &p.SizeRows, &p.CWD, &title, &fgPID, &fgName,
			&lastCmd, &currentCmd, &provider, &sessionID,
		); err != nil {
			_ = paneRows.Close()
			return tree, err
		}
		if parent.Valid {
			pp := parent.Int64
			p.ParentPaneID = &pp
		}
		if split.Valid {
			p.SplitDirection = split.String
		}
		if title.Valid {
			p.Title = title.String
		}
		if fgPID.Valid {
			pid := int(fgPID.Int64)
			p.ForegroundPID = &pid
		}
		if fgName.Valid {
			p.ForegroundName = fgName.String
		}
		if lastCmd.Valid {
			p.LastCmd = lastCmd.String
		}
		if currentCmd.Valid {
			p.CurrentCmd = currentCmd.String
		}
		if provider.Valid {
			p.SessionProvider = NormalizeProvider(provider.String)
		}
		if sessionID.Valid {
			p.SessionID = sessionID.String
			p.SessionProvider = NormalizeProvider(p.SessionProvider)
			if p.SessionProvider == ProviderClaude {
				p.ClaudeSessionID = sessionID.String
			}
		}
		panesByTab[p.TabID] = append(panesByTab[p.TabID], p)
	}
	if err := paneRows.Err(); err != nil {
		_ = paneRows.Close()
		return tree, err
	}
	_ = paneRows.Close()

	// 4. Assemble: walk windows in order, attach their tabs, attach panes to each tab.
	for _, wid := range orderedWindowIDs {
		win := windows[wid]
		for _, tab := range tabsByWindow[wid] {
			tab.Panes = panesByTab[tab.TabID]
			win.Tabs = append(win.Tabs, tab)
		}
		tree.Windows = append(tree.Windows, win)
	}
	return tree, nil
}

// --- helpers ---

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt64(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}

func claudeSessionID(provider, sessionID string) string {
	if NormalizeProvider(provider) == ProviderClaude {
		return sessionID
	}
	return ""
}
