package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/config"
	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

// Spawner is the interface Restore uses to talk to wezterm.
// Production: WeztermSpawner. Tests: a fake that asserts call sequences.
//
// Restore is "skeleton-first": it builds every window/tab/pane as a PLAIN SHELL
// (no command), reconstructing splits off each pane's real parent in the stored
// direction, then types the resolved command into each pane via SendText. This
// avoids splitting off a pane that is already running a fullscreen TUI
// (claude/tomoe), which was breaking restores.
type Spawner interface {
	// SpawnNewWindow opens a new wezterm window with one plain-shell pane in cwd.
	// Returns the spawned pane ID.
	SpawnNewWindow(cwd string, cmd []string) (paneID int64, err error)

	// SpawnTabInWindow opens a new tab (plain shell) in the given window.
	SpawnTabInWindow(windowID int64, cwd string, cmd []string) (paneID int64, err error)

	// SplitPaneDir splits parentPaneID in the given direction, opening a plain
	// shell in cwd. Returns the new pane ID.
	SplitPaneDir(parentPaneID int64, dir wezterm.SplitDirection, cwd string) (newPaneID int64, err error)

	// SendText types text into the given pane (used to run the resolved command
	// after the skeleton is built). Include a trailing carriage return to submit.
	SendText(paneID int64, text string) error

	// PaneExists reports whether the pane id is currently known to wezterm.
	// Used by the readiness poll before a pane is used as a split parent or a
	// send-text target.
	PaneExists(paneID int64) (bool, error)

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

func (WeztermSpawner) SplitPaneDir(parentPaneID int64, dir wezterm.SplitDirection, cwd string) (int64, error) {
	if dir == "" {
		dir = wezterm.SplitRight
	}
	return wezterm.SplitPane(wezterm.SplitArgs{PaneID: parentPaneID, Direction: dir, CWD: cwd})
}

func (WeztermSpawner) SendText(paneID int64, text string) error {
	return wezterm.SendText(wezterm.SendTextArgs{PaneID: paneID, Text: text, NoPaste: true})
}

func (WeztermSpawner) PaneExists(paneID int64) (bool, error) {
	panes, err := wezterm.List()
	if err != nil {
		return false, err
	}
	for _, p := range panes {
		if p.PaneID == paneID {
			return true, nil
		}
	}
	return false, nil
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

	// SkipFirst is deprecated and ignored. The old gui-startup pre-spawned a
	// default window and used this to avoid double-spawning; the current
	// gui-startup uses SpawnIfEmpty instead so the first saved pane (possibly a
	// linked coding-agent session) is never dropped.
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

	// PaneReadyTimeout bounds the per-pane readiness poll. Defaults to 2s.
	PaneReadyTimeout time.Duration
}

// RestoreResult summarizes what Restore did.
type RestoreResult struct {
	WindowsSpawned int
	TabsSpawned    int
	PanesSplit     int
	CommandsSent   int
	PanesSkipped   int
	Errors         []error
}

const defaultPaneReadyTimeout = 2 * time.Second
const paneReadyPollInterval = 50 * time.Millisecond

// Restore reads the latest snapshot from wezterm.db and rebuilds the layout.
// Pass 1 builds the full window/tab/pane skeleton as plain shells (splitting each
// pane off its real parent in the stored direction, with a readiness poll so a
// freshly-spawned pane id is visible before it's used). Pass 2 types each pane's
// resolved command (provider resume / literal replay) via send-text.
//
// Blocks until done (or DryRun, in which case it prints the plan).
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
	if opts.PaneReadyTimeout <= 0 {
		opts.PaneReadyTimeout = defaultPaneReadyTimeout
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

	// plans keyed by old pane id; sendOrder preserves capture order for pass 2.
	plans := map[int64]ReplayPlan{}
	var sendOrder []int64
	// newByOld maps a captured pane id to the freshly-spawned pane id.
	newByOld := map[int64]int64{}

	// --- Pass 1: build the skeleton (plain shells) ---
	for _, win := range tree.Windows {
		if opts.Workspace != "" && win.Workspace != opts.Workspace {
			continue
		}
		if len(win.Tabs) == 0 {
			continue
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}

		var liveWindowID int64
		windowOpened := false

		for _, tab := range win.Tabs {
			if len(tab.Panes) == 0 {
				continue
			}
			lead, rest := splitLead(tab.Panes)

			// Resolve + record plans for pass 2.
			for _, p := range tab.Panes {
				plan := resolver.Resolve(p)
				plans[p.PaneID] = plan
				sendOrder = append(sendOrder, p.PaneID)
			}

			// Spawn the tab lead.
			leadPlan := plans[lead.PaneID]
			if !windowOpened {
				if opts.DryRun {
					_, _ = fmt.Fprintf(opts.Out, "wezterm cli spawn --new-window --cwd %s\n", leadPlan.CWD)
					res.WindowsSpawned++
					windowOpened = true
					newByOld[lead.PaneID] = -1
				} else {
					paneID, err := opts.Spawner.SpawnNewWindow(leadPlan.CWD, nil)
					if err != nil {
						res.Errors = append(res.Errors, fmt.Errorf("spawn new window for %s: %w", leadPlan.CWD, err))
						break // abort this window; can't place its tabs without a window
					}
					wid, err := opts.Spawner.LookupWindowForPane(paneID)
					if err != nil {
						// Race fix: do NOT continue with windowOpened=false (that
						// spawned a second window per pane). Abort this window.
						res.Errors = append(res.Errors, fmt.Errorf("lookup window for pane %d: %w", paneID, err))
						break
					}
					liveWindowID = wid
					windowOpened = true
					newByOld[lead.PaneID] = paneID
					res.WindowsSpawned++
					waitForPane(ctx, opts, paneID)
				}
			} else {
				if opts.DryRun {
					_, _ = fmt.Fprintf(opts.Out, "wezterm cli spawn --window-id <win> --cwd %s\n", leadPlan.CWD)
					res.TabsSpawned++
					newByOld[lead.PaneID] = -1
				} else {
					paneID, err := opts.Spawner.SpawnTabInWindow(liveWindowID, leadPlan.CWD, nil)
					if err != nil {
						res.Errors = append(res.Errors, fmt.Errorf("spawn tab in window %d: %w", liveWindowID, err))
						continue
					}
					newByOld[lead.PaneID] = paneID
					res.TabsSpawned++
					waitForPane(ctx, opts, paneID)
				}
			}

			// Split the remaining panes off their real parents, topo-ordered.
			restoreTabSplits(ctx, opts, lead, rest, plans, newByOld, &res)
		}
	}

	// --- Pass 2: type the resolved commands into the skeleton panes ---
	for _, oldID := range sendOrder {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		plan := plans[oldID]
		if plan.SendLine == "" {
			continue
		}
		if opts.DryRun {
			_, _ = fmt.Fprintf(opts.Out, "wezterm cli send-text --pane-id %d -- %s\n", oldID, plan.SendLine)
			_, _ = fmt.Fprintf(opts.Out, "  reason: %s\n", plan.Reason)
			res.CommandsSent++
			continue
		}
		newID, ok := newByOld[oldID]
		if !ok || newID <= 0 {
			continue // skeleton spawn for this pane failed; already recorded
		}
		if err := opts.Spawner.SendText(newID, plan.SendLine+"\r"); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("send-text to pane %d: %w", newID, err))
			continue
		}
		res.CommandsSent++
	}

	spawnFallbackIfEmpty(&res, opts)

	if len(res.Errors) > 0 {
		_, _ = fmt.Fprintf(opts.Out, "\nrestore completed with %d error(s):\n", len(res.Errors))
		for _, e := range res.Errors {
			_, _ = fmt.Fprintf(opts.Out, "  - %v\n", e)
		}
	}
	return res, nil
}

// restoreTabSplits creates the non-lead panes of a tab by splitting each off its
// real parent (already created) in the stored direction. Processes in
// topological order; any pane whose parent never materializes falls back to a
// right split off the tab lead so it is not lost.
func restoreTabSplits(
	ctx context.Context, opts RestoreOptions,
	lead store.WezPane, rest []store.WezPane,
	plans map[int64]ReplayPlan, newByOld map[int64]int64, res *RestoreResult,
) {
	pending := append([]store.WezPane(nil), rest...)
	for {
		progress := false
		var still []store.WezPane
		for _, p := range pending {
			if ctx.Err() != nil {
				return
			}
			parentOld := lead.PaneID
			if p.ParentPaneID != nil {
				parentOld = *p.ParentPaneID
			}
			parentNew, ready := newByOld[parentOld]
			if !ready {
				still = append(still, p) // parent not built yet
				continue
			}
			dir := wezterm.SplitDirection(p.SplitDirection)
			plan := plans[p.PaneID]
			if opts.DryRun {
				_, _ = fmt.Fprintf(opts.Out, "wezterm cli split-pane --pane-id <%d> --%s --cwd %s\n",
					parentOld, dirOrDefault(dir), plan.CWD)
				newByOld[p.PaneID] = -1
				res.PanesSplit++
				progress = true
				continue
			}
			newID, err := opts.Spawner.SplitPaneDir(parentNew, dir, plan.CWD)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("split pane (parent %d): %w", parentNew, err))
				continue // drop this pane; don't retry forever
			}
			newByOld[p.PaneID] = newID
			res.PanesSplit++
			progress = true
			waitForPane(ctx, opts, newID)
		}
		pending = still
		if len(pending) == 0 || !progress {
			break
		}
	}
	// Leftover panes (parent never materialized) → fallback off the tab lead.
	for _, p := range pending {
		if ctx.Err() != nil {
			return
		}
		leadNew, ok := newByOld[lead.PaneID]
		plan := plans[p.PaneID]
		if opts.DryRun {
			_, _ = fmt.Fprintf(opts.Out, "wezterm cli split-pane --pane-id <%d> --right --cwd %s  (fallback)\n",
				lead.PaneID, plan.CWD)
			newByOld[p.PaneID] = -1
			res.PanesSplit++
			continue
		}
		if !ok || leadNew <= 0 {
			res.Errors = append(res.Errors, fmt.Errorf("no lead pane to split for pane %d", p.PaneID))
			continue
		}
		newID, err := opts.Spawner.SplitPaneDir(leadNew, wezterm.SplitRight, plan.CWD)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("fallback split off lead %d: %w", leadNew, err))
			continue
		}
		newByOld[p.PaneID] = newID
		res.PanesSplit++
		waitForPane(ctx, opts, newID)
	}
}

// splitLead returns the tab lead (the pane with no parent) and the rest. If no
// pane has a nil parent (shouldn't happen), the first pane is treated as lead.
func splitLead(panes []store.WezPane) (lead store.WezPane, rest []store.WezPane) {
	leadIdx := -1
	for i, p := range panes {
		if p.ParentPaneID == nil {
			leadIdx = i
			break
		}
	}
	if leadIdx < 0 {
		leadIdx = 0
	}
	lead = panes[leadIdx]
	for i, p := range panes {
		if i != leadIdx {
			rest = append(rest, p)
		}
	}
	return lead, rest
}

// waitForPane polls until the pane id is visible to wezterm (so it can be used as
// a split parent or send-text target) or the timeout elapses. No-op in DryRun.
func waitForPane(ctx context.Context, opts RestoreOptions, paneID int64) {
	if opts.DryRun {
		return
	}
	deadline := time.Now().Add(opts.PaneReadyTimeout)
	for {
		ok, err := opts.Spawner.PaneExists(paneID)
		if err == nil && ok {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(paneReadyPollInterval):
		}
	}
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

func dirOrDefault(d wezterm.SplitDirection) wezterm.SplitDirection {
	if d == "" {
		return wezterm.SplitRight
	}
	return d
}
