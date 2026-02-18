package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	DefaultDBDir     = ".cst"
	DefaultDBName    = "sessions.db"
	DefaultMaxCap    = 500
	DefaultMaxPrompt = 10
)

// Session represents a tracked Claude Code session.
type Session struct {
	ID           string
	Project      string
	CWD          string
	StartedAt    int64
	LastActivity int64
	PID          *int
	Active       bool
	Model        string
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
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &Store{db: db}
	if err := s.createTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}

	return s, nil
}

func (s *Store) createTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			cwd TEXT NOT NULL,
			started_at INTEGER NOT NULL,
			last_activity INTEGER NOT NULL,
			pid INTEGER,
			active INTEGER DEFAULT 0,
			model TEXT DEFAULT ''
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
	`)
	return err
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
	_, err := s.db.Exec(`
		INSERT INTO sessions (id, project, cwd, started_at, last_activity, pid, active, model)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			cwd = excluded.cwd,
			last_activity = excluded.last_activity,
			pid = excluded.pid,
			active = excluded.active,
			model = excluded.model
	`, sess.ID, project, cwd, sess.StartedAt, sess.LastActivity, sess.PID, active, sess.Model)
	return err
}

// Activate marks a session as active and updates its PID, model, cwd, and last_activity.
func (s *Store) Activate(id string, pid int, model, cwd string) error {
	now := time.Now().UnixMilli()
	resolvedCWD := ResolvePath(cwd)
	result, err := s.db.Exec(`
		UPDATE sessions SET active = 1, pid = ?, model = ?, cwd = ?, last_activity = ?
		WHERE id = ?
	`, pid, model, resolvedCWD, now, id)
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

// Deactivate marks a session as inactive and clears its PID.
func (s *Store) Deactivate(id string) error {
	_, err := s.db.Exec(`
		UPDATE sessions SET active = 0, pid = NULL WHERE id = ?
	`, id)
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

// AddPrompt inserts a prompt and evicts the oldest if the session exceeds the prompt cap.
func (s *Store) AddPrompt(sessionID, prompt string, ts int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
		SELECT s.id, s.project, s.cwd, s.started_at, s.last_activity, s.pid, s.active, s.model,
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
		SELECT s.id, s.project, s.cwd, s.started_at, s.last_activity, s.pid, s.active, s.model,
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
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		var active int
		var pid sql.NullInt64
		var promptTS sql.NullInt64
		err := rows.Scan(
			&sess.ID, &sess.Project, &sess.CWD, &sess.StartedAt, &sess.LastActivity,
			&pid, &active, &sess.Model, &sess.LastPrompt, &promptTS,
		)
		if err != nil {
			return nil, err
		}
		sess.Active = active != 0
		if pid.Valid {
			p := int(pid.Int64)
			sess.PID = &p
		}
		if promptTS.Valid {
			ts := promptTS.Int64
			sess.LastPromptTS = &ts
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
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
	defer rows.Close()

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

// RefreshActive checks all active sessions and deactivates those whose PID is no longer alive.
func (s *Store) RefreshActive(isAlive func(pid int) bool) error {
	rows, err := s.db.Query(`SELECT id, pid FROM sessions WHERE active = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var toDeactivate []string
	for rows.Next() {
		var id string
		var pid sql.NullInt64
		if err := rows.Scan(&id, &pid); err != nil {
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
