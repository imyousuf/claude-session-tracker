# Claude Session Tracker (CST)

A Claude Code plugin that tracks your sessions and provides an interactive TUI launcher to browse and resume previous sessions.

## Features

- **Session tracking** via Claude Code lifecycle hooks (SessionStart, UserPromptSubmit, SessionEnd)
- **Prompt history** - stores the last 10 user prompts per session for context
- **Interactive TUI** with search, preview pane, and keyboard navigation
- **Active session detection** - identifies and filters currently-running sessions
- **Cross-platform** - pure Go binary, no CGO required
- **Concurrent-safe** - SQLite WAL mode handles multiple simultaneous Claude sessions
- **Wezterm layout snapshot + restore** *(optional)* - captures your wezterm windows/tabs/CWDs on every meaningful event; re-launches the same layout (with `claude --resume` for the panes that had claude running) on the next wezterm start. See [Wezterm Integration](#wezterm-integration).

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
cmd/cst/             CLI entry point (cobra)
internal/
  store/             SQLite session store (modernc.org/sqlite, pure Go)
                     - sessions.db: claude session tracking (existing)
                     - wezterm.db:  wezterm layout snapshot (new; daemon-owned)
  hook/              Hook event handlers (read stdin JSON, update store)
  launcher/          Bubbletea TUI (session list + preview pane)
  procutil/          Cross-platform process liveness checking
  daemon/            Per-user snapshot daemon (Unix socket, event loop, coalesce)
  snapshot/          Snapshot sync + restore logic + replay-command resolver
  wezterm/           `wezterm cli list/spawn/split-pane` wrapper
  shellsetup/        Install shell hooks into ~/.bashrc / .zshrc / fish config
  wezsetup/          Install wezterm Lua integration
  daemonsetup/       Install systemd-user service for the daemon
```

## Wezterm Integration

Optional opt-in subsystem that snapshots your wezterm layout on every meaningful
event and restores it on the next wezterm start.

### Quick start

```bash
cst setup --all     # installs shell hooks + wezterm Lua + (if systemd-user) daemon
```

Then restart your terminal (or open a new wezterm window). Done.

What gets installed:

| File | Purpose |
|---|---|
| `~/.bashrc` (or `.zshrc`, fish config) | A marker-fenced block that registers per-prompt hooks. |
| `~/.cst/bash-preexec.sh` | Downloaded `rcaloras/bash-preexec` helper (bash only; zsh/fish use native hooks). Pinned version, verified by SHA. |
| `~/.config/wezterm/cst.lua` | Event handlers (`gui-startup`, `new-tab-button-click`, `mux-is-process-stateful`, `window-focus-changed`) and snapshot-on-keybinding wrappers. |
| `~/.wezterm.lua` | A short loader block (between markers) that `require`s `cst.lua`. |
| `~/.config/systemd/user/cst-daemon.service` | systemd unit (if `cst setup --all` detected systemd-user). |
| `~/.cst/wezterm.db` | The daemon's snapshot DB (separate from `sessions.db`). |
| `$XDG_RUNTIME_DIR/cst-daemon-$UID.sock` | The daemon's per-user Unix socket. |

### How it works

Three observer surfaces push events to a per-user daemon over a Unix socket:

```
shell precmd  ─┐
shell preexec ─┤
wezterm Lua   ─┼──► (push 1 JSON event over /run/user/$UID/cst-daemon-$UID.sock)
cst hooks     ─┘
                       │
                       ▼
                 cst-daemon
                 - coalesces snapshot requests in a ~100ms window
                 - runs `wezterm cli list` once per window
                 - syncs wezterm.db (closes => deleted, opens => upserted)

       on next wezterm launch:
       wezterm gui-startup → cst restore (blocks until layout is rebuilt)
```

Per-prompt cost is **sub-millisecond** on the shell side (just a socket write).
Restore is the only command that ever blocks on real work.

### Two databases — why split

- `~/.cst/sessions.db` (existing) — claude session metadata + prompt history.
  Single writer: the `cst hook` commands. Single reader: the TUI.
- `~/.cst/wezterm.db` (new) — wezterm tree snapshot + per-pane runtime state.
  Single writer: the cst daemon. Single reader: `cst restore` (read-only).

Splitting by writer eliminates contention; uninstalling the wezterm integration
removes only `wezterm.db` (your claude sessions are untouched).

### bash-preexec dependency

Bash's native `PROMPT_COMMAND` + raw `DEBUG` trap is too leaky to register clean
preexec/precmd hooks. `cst setup-shell` downloads
[`rcaloras/bash-preexec`](https://github.com/rcaloras/bash-preexec) (pinned
version, verified by SHA-256) into `~/.cst/bash-preexec.sh` and sources it from
the rc-file block. Zsh and fish have native hooks, so no extra download.

### Replay-command registry

Out of the box, two commands are replayed on restore: **`claude`** and **`tomoe`**.
For each captured pane, on `cst restore`:

| Pane state | Result |
|---|---|
| `last_cmd` starts with `claude` AND a session ID is linked | spawn `claude --resume <id>` |
| `last_cmd` starts with `claude` (no linked session) | spawn the literal capture, e.g. `claude --some-flag` |
| `last_cmd` starts with `tomoe` (or any registered command) | spawn the captured literal (full args) |
| `last_cmd` not in registry (e.g. `vim`, `ssh`, `htop`) | open a plain shell in the saved CWD |
| Nothing was captured for the pane | open a plain shell |

Customize:

```bash
cst config replay-list                  # current effective list
cst config replay-add my-tool           # add a command name
cst config replay-remove tomoe          # remove (writes resolved list)
```

The match uses the **first non-env-prefix token** of the captured command line,
so `FOO=1 tomoe start --device hw:0,0` matches `tomoe`.

### Daemon supervision

Preferred: **systemd-user**, installed automatically by `cst setup --all`
when available:

```bash
systemctl --user status cst-daemon       # check status
journalctl --user -u cst-daemon -f       # follow logs
```

Fallback: **wezterm-auto-start** + **client-side auto-spawn**. The wezterm
`gui-startup` hook starts the daemon if it's not running. The shell-side push
clients (`cst snapshot`, `cst hook preexec`, etc.) also auto-spawn the daemon
if their first socket connect fails. The daemon self-exits after 30 minutes
of no events; the next event re-spawns it.

### Multi-user

One daemon per user, isolated by:
- per-UID socket at `$XDG_RUNTIME_DIR/cst-daemon-$UID.sock`
- per-user `~/.cst/wezterm.db`
- UNIX file permissions (mode `0600` on socket and DBs)
- daemon refuses to bind if `$XDG_RUNTIME_DIR` isn't owned by `$UID`

Two users on the same machine each see only their own terminals.

### Verification checklist

After `cst setup --all`:

1. `cst daemon-status` → `reachable: true`.
2. `ls -l ~/.cst/` → both `sessions.db` and `wezterm.db` present, mode `0600`.
3. Open wezterm; open 2-3 tabs with different commands (e.g. `claude`, `tomoe`).
4. Wait ~200 ms; `sqlite3 ~/.cst/wezterm.db 'SELECT pane_id, cwd, current_cmd, claude_session_id FROM terminal_panes;'` → confirm rows.
5. `cst restore --dry-run` → prints the planned `wezterm cli` calls.
6. Quit wezterm, re-launch → layout reconstructed.

### Uninstall

```bash
cst setup --uninstall      # reverses everything: shell hooks, wezterm Lua, systemd
```

Or piecemeal:

```bash
cst setup-shell --uninstall
cst setup-wezterm --uninstall
cst setup-daemon --uninstall
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
