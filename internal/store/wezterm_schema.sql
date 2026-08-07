-- wezterm.db schema. Owned exclusively by the cst daemon.
--
-- See ~/.claude/plans/sunny-strolling-locket.md for the design rationale,
-- especially: single-snapshot sync (not multi-gen), pane_runtime_state as a
-- buffer between async shell hooks and periodic snapshots, and the
-- ATTACH-based cross-DB join to sessions.db for linked coding-agent sessions.

CREATE TABLE IF NOT EXISTS snapshot_meta (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    taken_at     INTEGER NOT NULL,
    content_hash TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS terminal_windows (
    window_id INTEGER PRIMARY KEY,
    workspace TEXT NOT NULL,
    win_index INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS terminal_tabs (
    tab_id    INTEGER PRIMARY KEY,
    window_id INTEGER NOT NULL
              REFERENCES terminal_windows(window_id) ON DELETE CASCADE,
    tab_index INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS terminal_panes (
    pane_id           INTEGER PRIMARY KEY,
    tab_id            INTEGER NOT NULL
                      REFERENCES terminal_tabs(tab_id) ON DELETE CASCADE,
    parent_pane_id    INTEGER,
    split_direction   TEXT,
    size_cols         INTEGER NOT NULL,
    size_rows         INTEGER NOT NULL,
    cwd               TEXT NOT NULL,
    title             TEXT,
    foreground_pid    INTEGER,
    foreground_name   TEXT,
    last_cmd          TEXT,
    current_cmd       TEXT,
    session_provider  TEXT,
    session_id        TEXT,
    -- Legacy compatibility column. Existing databases are migrated into the
    -- provider-aware fields when opened.
    claude_session_id TEXT
);

CREATE INDEX IF NOT EXISTS idx_panes_tab ON terminal_panes(tab_id);
CREATE INDEX IF NOT EXISTS idx_panes_fg_pid ON terminal_panes(foreground_pid);

CREATE TABLE IF NOT EXISTS pane_runtime_state (
    mux_socket         TEXT NOT NULL,
    pane_id            INTEGER NOT NULL,
    current_command    TEXT,
    current_started_at INTEGER,
    last_command       TEXT,
    last_finished_at   INTEGER,
    cwd                TEXT NOT NULL,
    -- Set by cst's SessionStart hook (which has $WEZTERM_PANE in env) and
    -- cleared by SessionEnd. Sync copies these into terminal_panes so restore
    -- can invoke the provider-specific resume command.
    session_provider   TEXT,
    session_id         TEXT,
    -- Legacy compatibility column; see terminal_panes above.
    claude_session_id  TEXT,
    PRIMARY KEY (mux_socket, pane_id)
);
