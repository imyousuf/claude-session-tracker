# Claude Session Tracker (CST)

A Claude Code plugin that tracks your sessions and provides an interactive TUI launcher to browse and resume previous sessions.

## Features

- **Session tracking** via Claude Code lifecycle hooks (SessionStart, UserPromptSubmit, SessionEnd)
- **Prompt history** - stores the last 10 user prompts per session for context
- **Interactive TUI** with search, preview pane, and keyboard navigation
- **Active session detection** - identifies and filters currently-running sessions
- **Cross-platform** - pure Go binary, no CGO required
- **Concurrent-safe** - SQLite WAL mode handles multiple simultaneous Claude sessions

## Installation

### 1. Install the binary

**From releases:**
```bash
# Linux (amd64)
curl -L https://github.com/imyousuf/claude-session-tracker/releases/download/dev/cst-linux-amd64.tar.gz | tar xz
mv cst ~/.local/bin/

# macOS (Apple Silicon)
curl -L https://github.com/imyousuf/claude-session-tracker/releases/download/dev/cst-darwin-arm64.tar.gz | tar xz
mv cst ~/.local/bin/
```

**From source:**
```bash
git clone https://github.com/imyousuf/claude-session-tracker.git
cd claude-session-tracker
make install  # installs to $GOPATH/bin
```

### 2. Enable the plugin

Clone the repo (if not done already) and enable it in Claude Code:

```bash
git clone https://github.com/imyousuf/claude-session-tracker.git ~/projects/claude-session-tracker
```

Then in Claude Code, use `/plugin` to add and enable `session-tracker`.

## Usage

### TUI Launcher

```bash
cst                          # Sessions for current project
cst --all                    # All sessions across all projects
cst --project /path/to/proj  # Sessions for a specific project
```

**Key bindings:**
| Key | Action |
|-----|--------|
| `j/k` or `↑/↓` | Navigate sessions |
| `Enter` | Resume selected session |
| `Tab` | Toggle current project / all projects |
| `/` | Search/filter sessions |
| `d` | Delete session entry |
| `q` / `Esc` | Quit |

### Non-Interactive List

```bash
cst list                     # Table output
cst list --all --json        # JSON output for scripting
```

### Maintenance

```bash
cst cleanup                  # Remove inactive sessions older than 30 days
cst cleanup --days 7         # Custom age threshold
cst version                  # Show version info
```

## How It Works

CST uses three Claude Code lifecycle hooks:

1. **SessionStart** - Records the session as active with its project path, model, and PID
2. **UserPromptSubmit** - Captures the user's prompt (skipping slash commands) and updates activity timestamp
3. **SessionEnd** - Marks the session as inactive

Session data is stored in `~/.cst/sessions.db` (SQLite with WAL mode).

When launching the TUI, CST validates active sessions by checking if their PIDs are still alive, automatically cleaning up stale entries from crashed sessions.

## Architecture

```
cmd/cst/          CLI entry point (cobra)
internal/
  store/          SQLite session store (modernc.org/sqlite, pure Go)
  hook/           Hook event handlers (read stdin JSON, update store)
  launcher/       Bubbletea TUI (session list + preview pane)
  procutil/       Cross-platform process liveness checking
```

## Development

```bash
make build       # Build to bin/cst
make test        # Run tests with race detector
make test-fast   # Run tests without race detector
make fmt         # Format code
make lint        # Run golangci-lint
make install     # Build and install to $GOPATH/bin
```

## License

Apache-2.0
