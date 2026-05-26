# Claude Session Tracker (CST)

## Project Overview

CST is a Claude Code plugin that tracks sessions via lifecycle hooks and provides a TUI launcher to browse and resume previous sessions. It stores session metadata and prompt history in a local SQLite database.

## Architecture

```
cmd/cst/main.go              # Cobra CLI: root, hook, launch, list, cleanup, version,
                             # snapshot, daemon, daemon-status, restore,
                             # setup, setup-shell, setup-wezterm, setup-daemon,
                             # config (set, replay-list/add/remove)
internal/
  store/store.go             # sessions.db: claude session metadata + prompts
  store/wezterm.go           # wezterm.db: snapshot tree + per-pane runtime state
  hook/handler.go            # Hook handlers: SessionStart, UserPromptSubmit, SessionEnd
  launcher/launcher.go       # Bubbletea TUI
  procutil/procutil.go       # PID liveness checking
  daemon/                    # Per-user snapshot daemon (Unix socket, event loop)
  snapshot/                  # Sync (wezterm cli list → wezterm.db) + Restore
                             # + Resolver (replay-command registry resolution)
  wezterm/                   # wezterm cli wrapper (list/spawn/split-pane)
  shellsetup/                # Install shell hooks (bash/zsh/fish)
  wezsetup/                  # Install wezterm Lua integration
  daemonsetup/               # Install systemd-user service
.claude-plugin/plugin.json   # Plugin manifest
hooks/hooks.json             # Hook event -> `cst hook <event>` wiring
```

### Two-database split

- `~/.cst/sessions.db` — claude session metadata + prompts (legacy single-DB).
  Writers: `cst hook session-start/prompt/session-end`. Reader: TUI launcher.
- `~/.cst/wezterm.db` — wezterm tree snapshot (`terminal_windows`/`tabs`/`panes`)
  + `pane_runtime_state` + `snapshot_meta`. Single writer: the cst daemon.
  Reader: `cst restore` (read-only). Cross-DB joins use SQLite `ATTACH DATABASE`
  when the daemon needs to look up claude sessions by PID.

### Daemon

- Listens on `$XDG_RUNTIME_DIR/cst-daemon-$UID.sock` (or `/tmp/...` fallback).
- Wire protocol: NDJSON over Unix socket. `protocol.go` defines `Event` with
  type discriminator (snapshot_request | preexec | precmd | session_start |
  session_end | shutdown).
- Event loop coalesces `snapshot_request` events into a single `wezterm cli list`
  call per ~100 ms window. `preexec`/`precmd`/`session_*` events are applied
  immediately (cheap UPSERTs).
- Self-exits after 30 minutes of no events. Re-spawns lazily on the next event
  via `daemon.SpawnDetached` (setsid-detached) or systemd-user.
- PID file: `~/.cst/daemon.pid`. Log file (non-systemd): `~/.cst/daemon.log`.

### Per-user isolation

- Socket path keyed by `$UID`.
- DBs in `$HOME/.cst/` (mode `0600`).
- `CheckSocketDirOwnership` refuses to bind if `$XDG_RUNTIME_DIR` isn't owned
  by the current uid (defense-in-depth on shared machines).
- Two users on the same host each run their own daemon; no shared state, no setuid.

### Replay-command registry

`config.ReplayCommands []string` — flat allow-list of command names. OOTB
defaults: `["claude", "tomoe"]`. Defaults are filled in by `WithDefaults()`
at runtime; the on-disk config only contains what the user has explicitly
written (so removing `tomoe` writes a list without it; the user can re-add).

Restore resolution (`internal/snapshot/registry.go::Resolver.Resolve`):
1. `current_cmd` if set, else `last_cmd`, else plain shell.
2. First non-env-prefix token → if in registry, replay; if not, plain shell.
3. Special case: `claude` with linked `claude_session_id` → `claude --resume <id>`.
4. Otherwise replay literal as `sh -c "exec <captured>"` (quote-safe).

### bash-preexec dependency

Bash's `PROMPT_COMMAND` + raw `DEBUG` trap is too leaky for a clean preexec.
`shellsetup/install.go::EnsureBashPreexec` downloads
`rcaloras/bash-preexec` (pinned URL + optional SHA-256) into `~/.cst/`.
Zsh and fish use native hooks; no extra download.

## Tech Stack

- **Language:** Go 1.24+
- **Database:** SQLite via `modernc.org/sqlite` (pure Go, no CGO)
- **TUI:** `charmbracelet/bubbletea` + `bubbles` + `lipgloss`
- **CLI:** `spf13/cobra`
- **Storage location:** `~/.cst/sessions.db`

## Key Design Decisions

- **Pure Go SQLite** (`modernc.org/sqlite`): No CGO dependency, enabling simple cross-compilation with `CGO_ENABLED=0`
- **Two-table schema**: `sessions` (metadata, low-frequency writes) + `prompts` (history, high-frequency writes). Prompts capped at 10 per session.
- **WAL mode + busy_timeout**: Handles concurrent writes from multiple Claude sessions running hooks simultaneously
- **PID-based active detection**: Records `os.Getppid()` in SessionStart hook; validates via `kill(pid, 0)` + `/proc/pid/cmdline` on launch
- **Hooks call the binary**: Plugin hooks run `cst hook session-start` etc., reading JSON from stdin. Binary must be on PATH.

## Database Schema

```sql
sessions (id TEXT PK, project, cwd, started_at, last_activity, pid, active, model)
prompts  (id INTEGER PK, session_id FK, prompt, timestamp)
```

## Hook Input Format (stdin JSON)

```json
{
  "session_id": "uuid",
  "cwd": "/path/to/project",
  "hook_event_name": "SessionStart|UserPromptSubmit|SessionEnd",
  "source": "startup|resume|compact|clear",
  "model": "claude-sonnet-4-6",
  "prompt": "user prompt text",
  "reason": "other|clear|logout"
}
```

## Build & Test

```bash
make build       # Build to bin/cst
make test        # Run tests with race detector
make test-fast   # Run tests without race detector
make fmt         # Format code
make lint        # Run golangci-lint
make install     # Install to $GOPATH/bin
```

## Development Guidelines

- Follow Go stdlib `testing` patterns (no testify)
- Hooks must complete within 5 seconds (timeout in hooks.json)
- All hook handlers should be idempotent
- Prompt text truncated to 200 chars before storage
- Slash commands (starting with `/`) are skipped in prompt hook
- Session cap: 500 entries with LRU eviction of oldest inactive
