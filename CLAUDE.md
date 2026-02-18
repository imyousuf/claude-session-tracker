# Claude Session Tracker (CST)

## Project Overview

CST is a Claude Code plugin that tracks sessions via lifecycle hooks and provides a TUI launcher to browse and resume previous sessions. It stores session metadata and prompt history in a local SQLite database.

## Architecture

```
cmd/cst/main.go              # Cobra CLI: root, hook, launch, list, cleanup, version commands
internal/
  store/store.go             # SQLite store: sessions + prompts tables, WAL mode, concurrent access
  hook/handler.go            # Hook handlers: SessionStart, UserPromptSubmit, SessionEnd
  launcher/launcher.go       # Bubbletea TUI: session list + preview pane with prompt history
  launcher/styles.go         # Lipgloss styles for the TUI
  procutil/procutil.go       # Cross-platform PID liveness checking
.claude-plugin/plugin.json   # Plugin manifest
hooks/hooks.json             # Hook event -> `cst hook <event>` wiring
```

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
