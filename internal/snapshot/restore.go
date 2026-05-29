package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/imyousuf/claude-session-tracker/internal/config"
	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

// Spawner is the interface Restore uses to talk to wezterm.
// Production: WeztermSpawner. Tests: a fake that asserts call sequences.
type Spawner interface {
	// SpawnNewWindow opens a new wezterm window with one pane running cmd
	// (or default shell if cmd is empty). Returns the spawned pane ID.
	SpawnNewWindow(cwd string, cmd []string) (paneID int64, err error)

	// SpawnTabInWindow opens a new tab in the given window. Returns pane ID.
	SpawnTabInWindow(windowID int64, cwd string, cmd []string) (paneID int64, err error)

	// SplitPane splits the given pane and runs cmd in the new pane (or default
	// shell if cmd is empty). Returns the new pane ID. Used to reconstruct the
	// extra panes of a multi-pane tab.
	SplitPane(paneID int64, cwd string, cmd []string) (newPaneID int64, err error)

	// LookupWindowForPane queries wezterm for the window ID that owns the
	// given pane. Used to thread a fresh window's ID into subsequent
	// SpawnTabInWindow calls.
	LookupWindowForPane(paneID int64) (windowID int64, err error)
}

// WeztermSpawner is the production Spawner that drives `wezterm cli`.
type WeztermSpawner struct{}

func (WeztermSpawner) SpawnNewWindow(cwd string, cmd []string) (int64, error) {
	return wezterm.Spawn(wezterm.SpawnArgs{NewWindow: true, CWD: cwd, Command: cmd})
}

func (WeztermSpawner) SpawnTabInWindow(windowID int64, cwd string, cmd []string) (int64, error) {
	return wezterm.Spawn(wezterm.SpawnArgs{WindowID: windowID, CWD: cwd, Command: cmd})
}

func (WeztermSpawner) SplitPane(paneID int64, cwd string, cmd []string) (int64, error) {
	// Direction isn't recoverable from `wezterm cli list` (it exposes no split
	// geometry), so we always split to the right. This keeps the panes in the
	// same tab — the important property — even if the exact orientation differs
	// from the original layout.
	return wezterm.SplitPane(wezterm.SplitArgs{
		PaneID:    paneID,
		Direction: wezterm.SplitRight,
		CWD:       cwd,
		Command:   cmd,
	})
}

func (WeztermSpawner) LookupWindowForPane(paneID int64) (int64, error) {
	panes, err := wezterm.List()
	if err != nil {
		return 0, err
	}
	for _, p := range panes {
		if p.PaneID == paneID {
			return p.WindowID, nil
		}
	}
	return 0, fmt.Errorf("pane %d not found in wezterm cli list", paneID)
}

// RestoreOptions controls Restore behavior.
type RestoreOptions struct {
	WezDBPath string // defaults to ~/.cst/wezterm.db
	Workspace string // "" = restore all workspaces

	// SkipFirst skips the very first pane of the very first window in the
	// restore plan. Legacy: was used by gui-startup when it pre-spawned a
	// default window. The current gui-startup uses SpawnIfEmpty instead so the
	// first pane (which may be running claude) is never dropped.
	SkipFirst bool

	// SpawnIfEmpty opens a single default window when the restore would
	// otherwise spawn nothing (no snapshot, empty snapshot, missing DB, or
	// workspace filter matched nothing). Used by gui-startup so wezterm always
	// has a window to show even on a first run with no saved layout.
	SpawnIfEmpty bool

	// DryRun prints the planned wezterm cli calls to Out instead of executing.
	DryRun bool

	Spawner Spawner   // defaults to WeztermSpawner{}
	Out     io.Writer // defaults to os.Stdout

	// Replay overrides the ReplayCommands list (otherwise read from config).
	Replay []string

	// ClaudeArgs are appended to `claude --resume <id>` when restoring a pane
	// with a linked claude session (e.g. --dangerously-skip-permissions for YOLO
	// mode plus any configured extra_args). Loaded from config alongside Replay
	// when Replay is nil; callers that set Replay explicitly (tests) own this
	// too.
	ClaudeArgs []string
}

// RestoreResult summarizes what Restore did.
type RestoreResult struct {
	WindowsSpawned int
	TabsSpawned    int
	PanesSplit     int
	PanesSkipped   int
	Errors         []error
}

// Restore reads the latest snapshot from wezterm.db and recreates the layout
// via `wezterm cli spawn`. Each window becomes a new wezterm window; each tab
// (including its panes — splits are flattened in v1) becomes a tab in that
// window. The first pane's spawn opens the window; subsequent panes spawn as
// tabs in that window.
//
// Per-pane spawn command is determined by the replay-command resolver
// (registry.go). Panes whose command isn't in the registry get a plain shell.
//
// Blocks until all spawns complete (or DryRun is true, in which case it just
// prints the plan).
func Restore(ctx context.Context, opts RestoreOptions) (RestoreResult, error) {
	res := RestoreResult{}

	if opts.WezDBPath == "" {
		opts.WezDBPath = store.DefaultWezDBPath()
	}
	if opts.Spawner == nil {
		opts.Spawner = WeztermSpawner{}
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Replay == nil {
		cfg, _ := config.Load(config.DefaultConfigPath())
		cfg = cfg.WithDefaults()
		opts.Replay = cfg.ReplayCommands
		opts.ClaudeArgs = cfg.ClaudeArgs()
	}

	w, err := store.OpenWezReadOnly(opts.WezDBPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintln(opts.Out, "no snapshot to restore (wezterm.db not found)")
			spawnFallbackIfEmpty(&res, opts)
			return res, nil
		}
		return res, fmt.Errorf("open wezterm.db: %w", err)
	}
	defer func() { _ = w.Close() }()

	tree, err := w.ReadTree()
	if err != nil {
		return res, fmt.Errorf("read tree: %w", err)
	}
	if len(tree.Windows) == 0 {
		_, _ = fmt.Fprintln(opts.Out, "no windows in snapshot; nothing to restore")
		spawnFallbackIfEmpty(&res, opts)
		return res, nil
	}

	resolver := NewResolver(opts.Replay, opts.ClaudeArgs)
	skippedFirst := false

	// Each snapshot window becomes a new wezterm window; each tab within it
	// becomes a tab; additional panes within a tab are re-split into that tab.
	for _, win := range tree.Windows {
		if opts.Workspace != "" && win.Workspace != opts.Workspace {
			continue
		}
		if len(win.Tabs) == 0 {
			continue
		}

		var liveWindowID int64 // wezterm window the new tabs/splits go into
		windowOpened := false  // have we spawned this window's first pane yet?

		for _, tab := range win.Tabs {
			if len(tab.Panes) == 0 {
				continue
			}

			// firstPaneID is the lead pane of this tab — the one subsequent
			// panes in the same tab split off of.
			var firstPaneID int64
			tabLeadDone := false

			for paneIdx, pane := range tab.Panes {
				if ctx.Err() != nil {
					return res, ctx.Err()
				}

				plan := resolver.Resolve(pane)

				// SkipFirst skips the very first pane of the very first window:
				// wezterm's gui-startup already opened one window+pane for us.
				if !windowOpened && paneIdx == 0 && opts.SkipFirst && !skippedFirst {
					skippedFirst = true
					windowOpened = true
					res.PanesSkipped++
					// This skipped pane is the tab's lead pane, but we have no
					// pane ID to split off of, so extra panes in this tab fall
					// back to new tabs below.
					continue
				}

				switch {
				case !windowOpened:
					// First pane of the window → open a new window.
					if opts.DryRun {
						_, _ = fmt.Fprintf(opts.Out, "wezterm cli spawn --new-window --cwd %s %s\n",
							plan.CWD, formatCmd(plan.Command))
						_, _ = fmt.Fprintf(opts.Out, "  reason: %s\n", plan.Reason)
						res.WindowsSpawned++
						windowOpened = true
						firstPaneID = -1 // unknown in dry-run
						tabLeadDone = true
						continue
					}
					paneID, err := opts.Spawner.SpawnNewWindow(plan.CWD, plan.Command)
					if err != nil {
						res.Errors = append(res.Errors, fmt.Errorf("spawn new window for %s: %w", plan.CWD, err))
						continue
					}
					wid, err := opts.Spawner.LookupWindowForPane(paneID)
					if err != nil {
						res.Errors = append(res.Errors, fmt.Errorf("lookup window for pane %d: %w", paneID, err))
						continue
					}
					liveWindowID = wid
					windowOpened = true
					firstPaneID = paneID
					tabLeadDone = true
					res.WindowsSpawned++

				case !tabLeadDone:
					// First pane of a subsequent tab → new tab in this window.
					if opts.DryRun {
						_, _ = fmt.Fprintf(opts.Out, "wezterm cli spawn --window-id <new> --cwd %s %s\n",
							plan.CWD, formatCmd(plan.Command))
						_, _ = fmt.Fprintf(opts.Out, "  reason: %s\n", plan.Reason)
						res.TabsSpawned++
						firstPaneID = -1
						tabLeadDone = true
						continue
					}
					paneID, err := opts.Spawner.SpawnTabInWindow(liveWindowID, plan.CWD, plan.Command)
					if err != nil {
						res.Errors = append(res.Errors, fmt.Errorf("spawn tab in window %d: %w", liveWindowID, err))
						continue
					}
					firstPaneID = paneID
					tabLeadDone = true
					res.TabsSpawned++

				default:
					// Additional pane in the same tab → split off the tab lead.
					if opts.DryRun {
						_, _ = fmt.Fprintf(opts.Out, "wezterm cli split-pane --pane-id <tab-lead> --cwd %s %s\n",
							plan.CWD, formatCmd(plan.Command))
						_, _ = fmt.Fprintf(opts.Out, "  reason: %s\n", plan.Reason)
						res.PanesSplit++
						continue
					}
					if firstPaneID <= 0 {
						// No lead pane to split off (e.g. the tab lead was the
						// skipped first pane). Fall back to a new tab so the pane
						// isn't lost.
						if _, err := opts.Spawner.SpawnTabInWindow(liveWindowID, plan.CWD, plan.Command); err != nil {
							res.Errors = append(res.Errors, fmt.Errorf("spawn fallback tab in window %d: %w", liveWindowID, err))
							continue
						}
						res.TabsSpawned++
						continue
					}
					if _, err := opts.Spawner.SplitPane(firstPaneID, plan.CWD, plan.Command); err != nil {
						res.Errors = append(res.Errors, fmt.Errorf("split pane %d: %w", firstPaneID, err))
						continue
					}
					res.PanesSplit++
				}
			}
		}
	}

	// If the layout produced no windows (e.g. every window was filtered out or
	// errored) and the caller asked for a fallback, open a default window.
	spawnFallbackIfEmpty(&res, opts)

	if len(res.Errors) > 0 {
		_, _ = fmt.Fprintf(opts.Out, "\nrestore completed with %d error(s):\n", len(res.Errors))
		for _, e := range res.Errors {
			_, _ = fmt.Fprintf(opts.Out, "  - %v\n", e)
		}
	}
	return res, nil
}

// spawnFallbackIfEmpty opens a single default window when SpawnIfEmpty is set
// and nothing has been spawned yet. Keeps wezterm from starting with no window
// on a first run (no snapshot). No-op in dry-run.
func spawnFallbackIfEmpty(res *RestoreResult, opts RestoreOptions) {
	if !opts.SpawnIfEmpty || res.WindowsSpawned > 0 {
		return
	}
	if opts.DryRun {
		_, _ = fmt.Fprintln(opts.Out, "wezterm cli spawn --new-window  (fallback: empty snapshot)")
		res.WindowsSpawned++
		return
	}
	if _, err := opts.Spawner.SpawnNewWindow("", nil); err != nil {
		res.Errors = append(res.Errors, fmt.Errorf("spawn fallback window: %w", err))
		return
	}
	res.WindowsSpawned++
}

func formatCmd(cmd []string) string {
	if len(cmd) == 0 {
		return "(default shell)"
	}
	return "-- " + strings.Join(cmd, " ")
}
