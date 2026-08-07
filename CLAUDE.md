# Coding Session Tracker (CST)

## Project Overview

CST tracks Claude Code and Codex CLI sessions via lifecycle hooks and provides
a shared TUI launcher ordered by recent activity. It stores the provider,
session metadata, and prompt history in a local SQLite database.

## Architecture

```
cmd/cst/main.go              # Cobra CLI: root, hook, launch, list, cleanup, version,
                             # snapshot, daemon, daemon-status, restore,
                             # setup, setup-codex, setup-shell, setup-wezterm,
                             # setup-daemon,
                             # config (set, replay-list/add/remove)
internal/
  store/store.go             # sessions.db: provider-aware sessions + prompts
  store/wezterm.go           # wezterm.db: snapshot tree + per-pane runtime state
  hook/handler.go            # Hook handlers: SessionStart, UserPromptSubmit, SessionEnd
  launcher/launcher.go       # Bubbletea TUI
  procutil/procutil.go       # PID liveness checking
  daemon/                    # Per-user snapshot daemon (Unix socket, event loop)
  snapshot/                  # Sync (wezterm cli list → wezterm.db) + Restore
                             # + Resolver (replay-command registry resolution)
  wezterm/                   # wezterm cli wrapper (list/spawn/split-pane)
  shellsetup/                # Install shell hooks (bash/zsh/fish)
  codexsetup/                # Merge-safe ~/.codex/hooks.json installation
  wezsetup/                  # Install wezterm Lua integration
  daemonsetup/               # Install systemd-user service
.claude-plugin/plugin.json   # Plugin manifest
hooks/hooks.json             # Hook event -> `cst hook <event>` wiring
```

### Two-database split

- `~/.cst/sessions.db` — Claude/Codex metadata, client attachment state,
  lifecycle-end timestamps, and prompts. Writers: lifecycle and shell hooks.
  Reader: TUI launcher.
- `~/.cst/wezterm.db` — wezterm tree snapshot (`terminal_windows`/`tabs`/`panes`)
  + `pane_runtime_state` + `snapshot_meta`. Single writer: the cst daemon.
  Reader: `cst restore` (read-only). Cross-DB joins use SQLite `ATTACH DATABASE`
  when the daemon needs to look up coding-agent sessions by PID.

### Daemon

- Listens on `$XDG_RUNTIME_DIR/cst-daemon-$UID.sock` (or `/tmp/...` fallback).
- Wire protocol: NDJSON over Unix socket. `protocol.go` defines `Event` with
  type discriminator (snapshot_request | preexec | precmd | session_start |
  session_end | shutdown).
- Event loop coalesces `snapshot_request` events into a single `wezterm cli list`
  call per ~100 ms window. `preexec`/`precmd`/`session_*` events are applied
  immediately (cheap UPSERTs). A linked pane's `precmd` marks its CLI detached
  and resumable; `SessionEnd` is a separate, potentially delayed event.
- Standalone mode self-exits after 30 minutes of no events and re-spawns lazily
  via `daemon.SpawnDetached` (setsid-detached). The systemd unit uses
  `--idle-timeout=0` and remains supervised.
- PID file: `~/.cst/daemon.pid`. Log file (non-systemd): `~/.cst/daemon.log`.

### Per-user isolation

- Socket path keyed by `$UID`.
- DBs in `$HOME/.cst/` (mode `0600`).
- `CheckSocketDirOwnership` refuses to bind if `$XDG_RUNTIME_DIR` isn't owned
  by the current uid (defense-in-depth on shared machines).
- Two users on the same host each run their own daemon; no shared state, no setuid.

### Replay-command registry

`config.ReplayCommands []string` — flat allow-list of command names. OOTB
defaults: `["claude", "codex", "sosuke", "tomoe"]`. Defaults are filled in by `WithDefaults()`
at runtime; the on-disk config only contains what the user has explicitly
written (so removing a command writes a list without it; the user can re-add).

Restore resolution (`internal/snapshot/registry.go::Resolver.Resolve`):
1. `current_cmd` if set, else `last_cmd`, else plain shell.
2. First non-env-prefix token → if in registry, replay; if not, plain shell.
3. Provider special cases are checked before the registry: a linked Claude
   pane becomes `claude --resume <id>` plus `config.ClaudeArgs()`; a linked
   Codex pane becomes `codex resume <id>`.
4. Otherwise replay literal as `sh -c "exec <captured>"` (quote-safe).

Sosuke has no lifecycle hooks yet, so it is literal-replay only. The generic
`provider`/`session_id` schema is ready for it once hooks and resume semantics
exist.

Layout reconstruction (`internal/snapshot/restore.go::Restore`): each snapshot
window → new wezterm window; each tab → a tab; additional panes within a tab →
re-split into that tab via `wezterm cli split-pane` (direction is approximate —
`wezterm cli list` exposes no split geometry). gui-startup calls
`cst restore --spawn-if-empty` (NOT `--skip-first`): it does not pre-spawn a
window, so the first saved pane — possibly a linked agent session — is never dropped;
`--spawn-if-empty` opens one default window only when there's no snapshot.

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
- **WAL mode + busy_timeout**: Handles concurrent writes from multiple agents.
- **Provider-aware active detection**: Claude uses its hook ancestry. Codex
  resolves the terminal-backed frontend PID and wezterm pane because hooks run
  under a persistent app-server. Shell `precmd` and PID validation detach the
  client; lifecycle end is stored separately.
- **Hooks call the binary**: Claude defaults to provider `claude`; Codex's installed hooks pass `--provider codex`.

## Database Schema

```sql
sessions (id TEXT PK, provider, project, cwd, started_at, last_activity, pid,
          active, model, detached_at, lifecycle_ended_at,
          active_mux_socket, active_pane_id)
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

The provider is supplied by the installed hook command (`claude` by default,
or `--provider codex`), not trusted from the lifecycle JSON payload.

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
