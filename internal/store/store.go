package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	DefaultDBDir     = ".cst"
	DefaultDBName    = "sessions.db"
	DefaultMaxCap    = 500
	DefaultMaxPrompt = 10
	ProviderClaude   = "claude"
	ProviderCodex    = "codex"
)

// Session represents a tracked coding-agent session.
type Session struct {
	ID           string
	Provider     string
	Project      string
	CWD          string
	StartedAt    int64
	LastActivity int64
	PID          *int
	Active       bool
	Model        string
	// DetachedAt records when the foreground CLI returned to its shell.
	// LifecycleEndedAt records the distinct provider hook; Codex can detach a
	// client while keeping the thread open, and SessionEnd may run much later.
	DetachedAt       *int64
	LifecycleEndedAt *int64
	ActiveMuxSocket  string
	ActivePaneID     *int64
	// Populated by joined queries for display:
	LastPrompt   string
	LastPromptTS *int64
}

// Prompt represents a user prompt within a session.
type Prompt struct {
	ID        int64
	SessionID string
	Text      string
	Timestamp int64
}

// Store wraps the SQLite database for session tracking.
type Store struct {
	db *sql.DB
}

// ResolvePath resolves symlinks to get the canonical path.
// Falls back to the original path if resolution fails.
func ResolvePath(p string) string {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return resolved
}

// DefaultDBPath returns the default database path (~/.cst/sessions.db).
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, DefaultDBDir, DefaultDBName)
}

// Open opens or creates the session tracking database at the given path.
func Open(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &Store{db: db}
	if err := s.createTables(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}

	return s, nil
}

func (s *Store) createTables() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL DEFAULT 'claude',
			project TEXT NOT NULL,
			cwd TEXT NOT NULL,
			started_at INTEGER NOT NULL,
			last_activity INTEGER NOT NULL,
			pid INTEGER,
			active INTEGER DEFAULT 0,
			model TEXT DEFAULT '',
			detached_at INTEGER,
			lifecycle_ended_at INTEGER,
			active_mux_socket TEXT,
			active_pane_id INTEGER
		);

		CREATE TABLE IF NOT EXISTS prompts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			prompt TEXT NOT NULL,
			timestamp INTEGER NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project);
		CREATE INDEX IF NOT EXISTS idx_sessions_active ON sessions(active);
		CREATE INDEX IF NOT EXISTS idx_sessions_last_activity ON sessions(last_activity DESC);
		CREATE INDEX IF NOT EXISTS idx_prompts_session ON prompts(session_id, timestamp DESC);
	`); err != nil {
		return err
	}

	// Existing CST databases predate multi-provider tracking. SQLite's
	// CREATE TABLE IF NOT EXISTS does not add new columns, so migrate them in
	// place and preserve every existing row as a Claude session.
	if err := ensureColumn(s.db, "sessions", "provider", "TEXT NOT NULL DEFAULT 'claude'"); err != nil {
		return fmt.Errorf("add sessions.provider: %w", err)
	}
	for column, definition := range map[string]string{
		"detached_at":        "INTEGER",
		"lifecycle_ended_at": "INTEGER",
		"active_mux_socket":  "TEXT",
		"active_pane_id":     "INTEGER",
	} {
		if err := ensureColumn(s.db, "sessions", column, definition); err != nil {
			return fmt.Errorf("add sessions.%s: %w", column, err)
		}
	}
	return nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// UpsertSession inserts a new session or updates an existing one.
// Paths are resolved to their canonical form to handle symlinks.
func (s *Store) UpsertSession(sess Session) error {
	active := 0
	if sess.Active {
		active = 1
	}
	project := ResolvePath(sess.Project)
	cwd := ResolvePath(sess.CWD)
	provider := NormalizeProvider(sess.Provider)
	_, err := s.db.Exec(`
		INSERT INTO sessions (
			id, provider, project, cwd, started_at, last_activity, pid, active, model,
			detached_at, lifecycle_ended_at, active_mux_socket, active_pane_id
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			provider = excluded.provider,
			cwd = excluded.cwd,
			last_activity = excluded.last_activity,
			pid = excluded.pid,
			active = excluded.active,
			model = excluded.model,
			detached_at = excluded.detached_at,
			lifecycle_ended_at = excluded.lifecycle_ended_at,
			active_mux_socket = excluded.active_mux_socket,
			active_pane_id = excluded.active_pane_id
	`, sess.ID, provider, project, cwd, sess.StartedAt, sess.LastActivity, sess.PID, active, sess.Model,
		sess.DetachedAt, sess.LifecycleEndedAt, nullableString(sess.ActiveMuxSocket), sess.ActivePaneID)
	return err
}

// Activate marks a session as active and updates its PID, model, cwd, and last_activity.
func (s *Store) Activate(id string, pid int, model, cwd string) error {
	return s.ActivateProvider(id, ProviderClaude, pid, model, cwd)
}

// ActivateProvider marks a provider-specific session as active without a pane
// attachment. It is retained for callers that have a reliable process PID.
func (s *Store) ActivateProvider(id, provider string, pid int, model, cwd string) error {
	return s.ActivateAttached(id, provider, &pid, model, cwd, "", 0)
}

// ActivateAttached marks a provider-specific session as attached to a CLI.
// Codex hooks execute under a long-lived app-server, so pid may deliberately be
// nil; muxSocket/paneID identify the foreground client instead.
func (s *Store) ActivateAttached(id, provider string, pid *int, model, cwd, muxSocket string, paneID int64) error {
	now := time.Now().UnixMilli()
	resolvedCWD := ResolvePath(cwd)
	var activePaneID any
	if paneID != 0 {
		activePaneID = paneID
	}
	result, err := s.db.Exec(`
		UPDATE sessions SET provider = ?, active = 1, pid = ?, model = ?, cwd = ?, last_activity = ?,
			detached_at = NULL, lifecycle_ended_at = NULL, active_mux_socket = ?, active_pane_id = ?
		WHERE id = ?
	`, NormalizeProvider(provider), pid, model, resolvedCWD, now,
		nullableString(muxSocket), activePaneID, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Deactivate marks a session as no longer attached to a foreground CLI. It
// does not mark the resumable conversation lifecycle as ended.
func (s *Store) Deactivate(id string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		UPDATE sessions SET active = 0, pid = NULL,
			detached_at = COALESCE(detached_at, ?),
			active_mux_socket = NULL, active_pane_id = NULL
		WHERE id = ?
	`, now, id)
	return err
}

// DetachSession marks a CLI attachment as exited only if it still belongs to
// the same pane. This prevents a delayed precmd from an old pane from clearing
// a session that has already been resumed elsewhere.
func (s *Store) DetachSession(id, provider, muxSocket string, paneID, detachedAt int64) (bool, error) {
	if detachedAt == 0 {
		detachedAt = time.Now().UnixMilli()
	}
	result, err := s.db.Exec(`
		UPDATE sessions SET active = 0, pid = NULL, detached_at = ?,
			last_activity = MAX(last_activity, ?),
			active_mux_socket = NULL, active_pane_id = NULL
		WHERE id = ? AND provider = ? AND active = 1
		  AND ((active_mux_socket = ? AND active_pane_id = ?)
		       OR (active_mux_socket IS NULL AND active_pane_id IS NULL))
	`, detachedAt, detachedAt, id, NormalizeProvider(provider), muxSocket, paneID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

// RecordLifecycleEnd records the provider's SessionEnd hook without changing
// client attachment state. Attachment ends independently through shell precmd
// or frontend PID validation, and the thread remains resumable either way.
func (s *Store) RecordLifecycleEnd(id string, endedAt int64) error {
	if endedAt == 0 {
		endedAt = time.Now().UnixMilli()
	}
	_, err := s.db.Exec(`
		UPDATE sessions SET lifecycle_ended_at = ? WHERE id = ?
	`, endedAt, id)
	return err
}

// UpdateActivity updates the last_activity timestamp and cwd for a session.
func (s *Store) UpdateActivity(id, cwd string, ts int64) error {
	resolvedCWD := ResolvePath(cwd)
	_, err := s.db.Exec(`
		UPDATE sessions SET last_activity = ?, cwd = ? WHERE id = ?
	`, ts, resolvedCWD, id)
	return err
}

// IsSessionActive returns the latest attachment state for a session.
func (s *Store) IsSessionActive(id string) (bool, error) {
	var active int
	err := s.db.QueryRow(`SELECT active FROM sessions WHERE id = ?`, id).Scan(&active)
	if err != nil {
		return false, err
	}
	return active != 0, nil
}

// AddPrompt inserts a prompt and evicts the oldest if the session exceeds the prompt cap.
func (s *Store) AddPrompt(sessionID, prompt string, ts int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		INSERT INTO prompts (session_id, prompt, timestamp) VALUES (?, ?, ?)
	`, sessionID, prompt, ts)
	if err != nil {
		return err
	}

	// Evict oldest prompts if over the cap
	_, err = tx.Exec(`
		DELETE FROM prompts WHERE id IN (
			SELECT id FROM prompts
			WHERE session_id = ?
			ORDER BY timestamp DESC
			LIMIT -1 OFFSET ?
		)
	`, sessionID, DefaultMaxPrompt)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ListByProject returns sessions for a given project, ordered by last_activity DESC.
// Each session includes the most recent prompt text and timestamp.
// The project path is resolved to its canonical form to handle symlinks.
func (s *Store) ListByProject(project string) ([]Session, error) {
	resolved := ResolvePath(project)
	return s.listSessions(`
		SELECT s.id, s.provider, s.project, s.cwd, s.started_at, s.last_activity, s.pid, s.active, s.model,
			s.detached_at, s.lifecycle_ended_at, s.active_mux_socket, s.active_pane_id,
			COALESCE(p.prompt, ''), p.timestamp
		FROM sessions s
		LEFT JOIN (
			SELECT session_id, prompt, timestamp,
				ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY timestamp DESC) as rn
			FROM prompts
		) p ON p.session_id = s.id AND p.rn = 1
		WHERE s.project = ?
		ORDER BY s.last_activity DESC
	`, resolved)
}

// ListAll returns all sessions, ordered by last_activity DESC.
func (s *Store) ListAll() ([]Session, error) {
	return s.listSessions(`
		SELECT s.id, s.provider, s.project, s.cwd, s.started_at, s.last_activity, s.pid, s.active, s.model,
			s.detached_at, s.lifecycle_ended_at, s.active_mux_socket, s.active_pane_id,
			COALESCE(p.prompt, ''), p.timestamp
		FROM sessions s
		LEFT JOIN (
			SELECT session_id, prompt, timestamp,
				ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY timestamp DESC) as rn
			FROM prompts
		) p ON p.session_id = s.id AND p.rn = 1
		ORDER BY s.last_activity DESC
	`)
}

func (s *Store) listSessions(query string, args ...any) ([]Session, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var sessions []Session
	for rows.Next() {
		var sess Session
		var active int
		var pid sql.NullInt64
		var detachedAt, endedAt, activePaneID sql.NullInt64
		var activeMuxSocket sql.NullString
		var promptTS sql.NullInt64
		err := rows.Scan(
			&sess.ID, &sess.Provider, &sess.Project, &sess.CWD, &sess.StartedAt, &sess.LastActivity,
			&pid, &active, &sess.Model, &detachedAt, &endedAt, &activeMuxSocket, &activePaneID,
			&sess.LastPrompt, &promptTS,
		)
		if err != nil {
			return nil, err
		}
		sess.Active = active != 0
		if pid.Valid {
			p := int(pid.Int64)
			sess.PID = &p
		}
		if detachedAt.Valid {
			ts := detachedAt.Int64
			sess.DetachedAt = &ts
		}
		if endedAt.Valid {
			ts := endedAt.Int64
			sess.LifecycleEndedAt = &ts
		}
		if activeMuxSocket.Valid {
			sess.ActiveMuxSocket = activeMuxSocket.String
		}
		if activePaneID.Valid {
			paneID := activePaneID.Int64
			sess.ActivePaneID = &paneID
		}
		if promptTS.Valid {
			ts := promptTS.Int64
			sess.LastPromptTS = &ts
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// NormalizeProvider returns the canonical provider name. Empty values are
// legacy Claude records and therefore default to "claude".
func NormalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return ProviderClaude
	}
	return provider
}

func tableHasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ensureColumn is safe when two freshly-upgraded hook processes race through
// startup: if another process wins the ALTER TABLE, verify the resulting schema
// and treat the duplicate-column error as success.
func ensureColumn(db *sql.DB, table, column, definition string) error {
	hasColumn, err := tableHasColumn(db, table, column)
	if err != nil || hasColumn {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		hasColumn, checkErr := tableHasColumn(db, table, column)
		if checkErr == nil && hasColumn {
			return nil
		}
		return err
	}
	return nil
}

// GetPrompts returns the last N prompts for a session, ordered newest first.
func (s *Store) GetPrompts(sessionID string, limit int) ([]Prompt, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, prompt, timestamp
		FROM prompts
		WHERE session_id = ?
		ORDER BY timestamp DESC
		LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var prompts []Prompt
	for rows.Next() {
		var p Prompt
		if err := rows.Scan(&p.ID, &p.SessionID, &p.Text, &p.Timestamp); err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

// DeleteSession removes a session and its prompts (cascade).
func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// Cleanup removes inactive sessions older than the specified number of days.
func (s *Store) Cleanup(olderThanDays int) (int, error) {
	cutoff := time.Now().Add(-time.Duration(olderThanDays) * 24 * time.Hour).UnixMilli()
	result, err := s.db.Exec(`
		DELETE FROM sessions WHERE active = 0 AND last_activity < ?
	`, cutoff)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	return int(rows), err
}

// EnforceCap removes the oldest inactive sessions if the total count exceeds maxSessions.
func (s *Store) EnforceCap(maxSessions int) error {
	_, err := s.db.Exec(`
		DELETE FROM sessions WHERE id IN (
			SELECT id FROM sessions
			WHERE active = 0
			ORDER BY last_activity ASC
			LIMIT MAX(0, (SELECT COUNT(*) FROM sessions) - ?)
		)
	`, maxSessions)
	return err
}

// RefreshActive checks PID-backed sessions and deactivates those whose
// interactive provider process is gone. Codex SessionStart resolves the real
// frontend PID rather than the persistent app-server that launches its hooks.
func (s *Store) RefreshActive(isAlive func(pid int) bool) error {
	rows, err := s.db.Query(`SELECT id, provider, pid FROM sessions WHERE active = 1`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var toDeactivate []string
	for rows.Next() {
		var id, provider string
		var pid sql.NullInt64
		if err := rows.Scan(&id, &provider, &pid); err != nil {
			return err
		}
		if !pid.Valid || !isAlive(int(pid.Int64)) {
			toDeactivate = append(toDeactivate, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range toDeactivate {
		if err := s.Deactivate(id); err != nil {
			return err
		}
	}
	return nil
}
