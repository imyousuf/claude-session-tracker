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
	// restore plan. Used by wezterm's gui-startup hook so we don't double-
	// spawn alongside the default window wezterm already opened.
	SkipFirst bool

	// DryRun prints the planned wezterm cli calls to Out instead of executing.
	DryRun bool

	Spawner Spawner   // defaults to WeztermSpawner{}
	Out     io.Writer // defaults to os.Stdout

	// Replay overrides the ReplayCommands list (otherwise read from config).
	Replay []string
}

// RestoreResult summarizes what Restore did.
type RestoreResult struct {
	WindowsSpawned int
	TabsSpawned    int
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
	}

	w, err := store.OpenWezReadOnly(opts.WezDBPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(opts.Out, "no snapshot to restore (wezterm.db not found)")
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
		fmt.Fprintln(opts.Out, "no windows in snapshot; nothing to restore")
		return res, nil
	}

	resolver := NewResolver(opts.Replay)
	skippedFirst := false

	for _, win := range tree.Windows {
		if opts.Workspace != "" && win.Workspace != opts.Workspace {
			continue
		}

		// Collect all panes for this window in (tab, pane) order.
		var winPanes []store.WezPane
		for _, tab := range win.Tabs {
			winPanes = append(winPanes, tab.Panes...)
		}
		if len(winPanes) == 0 {
			continue
		}

		var liveWindowID int64
		for i, pane := range winPanes {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}

			plan := resolver.Resolve(pane)

			// SkipFirst applies to the very first pane of the very first
			// window we process.
			if i == 0 && opts.SkipFirst && !skippedFirst {
				skippedFirst = true
				res.PanesSkipped++
				continue
			}

			if i == 0 {
				// New window.
				if opts.DryRun {
					fmt.Fprintf(opts.Out, "wezterm cli spawn --new-window --cwd %s %s\n",
						plan.CWD, formatCmd(plan.Command))
					fmt.Fprintf(opts.Out, "  reason: %s\n", plan.Reason)
					res.WindowsSpawned++
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
				res.WindowsSpawned++
				continue
			}

			// Subsequent panes in this window → new tab.
			if opts.DryRun {
				fmt.Fprintf(opts.Out, "wezterm cli spawn --window-id <new> --cwd %s %s\n",
					plan.CWD, formatCmd(plan.Command))
				fmt.Fprintf(opts.Out, "  reason: %s\n", plan.Reason)
				res.TabsSpawned++
				continue
			}
			if _, err := opts.Spawner.SpawnTabInWindow(liveWindowID, plan.CWD, plan.Command); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("spawn tab in window %d: %w", liveWindowID, err))
				continue
			}
			res.TabsSpawned++
		}
	}

	if len(res.Errors) > 0 {
		fmt.Fprintf(opts.Out, "\nrestore completed with %d error(s):\n", len(res.Errors))
		for _, e := range res.Errors {
			fmt.Fprintf(opts.Out, "  - %v\n", e)
		}
	}
	return res, nil
}

func formatCmd(cmd []string) string {
	if len(cmd) == 0 {
		return "(default shell)"
	}
	return "-- " + strings.Join(cmd, " ")
}
