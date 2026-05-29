package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/imyousuf/claude-session-tracker/internal/config"
	"github.com/imyousuf/claude-session-tracker/internal/daemon"
	"github.com/imyousuf/claude-session-tracker/internal/daemonsetup"
	"github.com/imyousuf/claude-session-tracker/internal/hook"
	"github.com/imyousuf/claude-session-tracker/internal/launcher"
	"github.com/imyousuf/claude-session-tracker/internal/shellsetup"
	"github.com/imyousuf/claude-session-tracker/internal/snapshot"
	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezsetup"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

// Build-time variables set via ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var (
	flagAll     bool
	flagProject string
	flagDays    int
	flagJSON    bool
)

var rootCmd = &cobra.Command{
	Use:   "cst [-- claude-args...]",
	Short: "Claude Session Tracker - track and resume Claude Code sessions",
	Long:  "A tool that tracks Claude Code sessions via lifecycle hooks and provides a TUI launcher to browse and resume previous sessions.\n\nAny arguments after -- are passed through to the claude CLI on resume.",
	RunE:  launchTUI,
	Args:  cobra.ArbitraryArgs,
}

func init() {
	rootCmd.AddCommand(hookCmd)
	rootCmd.AddCommand(launchCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(cleanupCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(snapshotCmd)
	rootCmd.AddCommand(daemonCmd)
	rootCmd.AddCommand(daemonStatusCmd)
	rootCmd.AddCommand(restoreCmd)
	rootCmd.AddCommand(setupShellCmd)
	rootCmd.AddCommand(setupWeztermCmd)
	rootCmd.AddCommand(setupDaemonCmd)
	rootCmd.AddCommand(setupCmd)

	// Launch flags (also on root)
	rootCmd.Flags().BoolVarP(&flagAll, "all", "a", false, "Show sessions from all projects")
	rootCmd.Flags().StringVarP(&flagProject, "project", "p", "", "Filter by project path")

	launchCmd.Flags().BoolVarP(&flagAll, "all", "a", false, "Show sessions from all projects")
	launchCmd.Flags().StringVarP(&flagProject, "project", "p", "", "Filter by project path")

	listCmd.Flags().BoolVarP(&flagAll, "all", "a", false, "Show sessions from all projects")
	listCmd.Flags().StringVarP(&flagProject, "project", "p", "", "Filter by project path")
	listCmd.Flags().BoolVar(&flagJSON, "json", false, "Output as JSON")

	cleanupCmd.Flags().IntVar(&flagDays, "days", 30, "Remove inactive sessions older than N days")
}

// --- Hook Commands ---

var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Hook handlers called by Claude Code lifecycle events",
}

func init() {
	hookCmd.AddCommand(hookSessionStartCmd)
	hookCmd.AddCommand(hookPromptCmd)
	hookCmd.AddCommand(hookSessionEndCmd)
	hookCmd.AddCommand(hookPreexecCmd)
	hookCmd.AddCommand(hookPrecmdCmd)
}

var hookSessionStartCmd = &cobra.Command{
	Use:   "session-start",
	Short: "Handle SessionStart hook event",
	RunE: func(cmd *cobra.Command, args []string) error {
		input, err := runHookReturningInput(hook.HandleSessionStart)
		if err != nil {
			return err
		}
		pushSessionEventIfInWezterm(daemon.EventSessionStart, input)
		return nil
	},
}

var hookPromptCmd = &cobra.Command{
	Use:   "prompt",
	Short: "Handle UserPromptSubmit hook event",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runHook(hook.HandlePrompt)
	},
}

var hookSessionEndCmd = &cobra.Command{
	Use:   "session-end",
	Short: "Handle SessionEnd hook event",
	RunE: func(cmd *cobra.Command, args []string) error {
		input, err := runHookReturningInput(hook.HandleSessionEnd)
		if err != nil {
			return err
		}
		pushSessionEventIfInWezterm(daemon.EventSessionEnd, input)
		return nil
	},
}

// hookPreexecCmd is invoked by bash-preexec preexec hook (or equivalent on
// zsh/fish). Pushes an event to the daemon recording what's about to run.
// The full command line is the single positional argument.
var hookPreexecCmd = &cobra.Command{
	Use:   "preexec <command>",
	Short: "Handle shell preexec event (about-to-run command)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !wezterm.InWezterm() {
			return nil
		}
		// Concatenate args so users can pass either `cst hook preexec "tomoe start"`
		// or `cst hook preexec tomoe start` — both produce the same recorded line.
		command := strings.Join(args, " ")
		ev := daemon.Event{
			Type:      daemon.EventPreexec,
			MuxSocket: wezterm.MuxSocket(),
			PaneID:    wezterm.PaneID(),
			Command:   command,
			CWD:       currentCWD(),
			Timestamp: time.Now().UnixMilli(),
		}
		// Don't surface error — we never want a shell hook to fail the prompt.
		_ = daemon.PushEvent(ev)
		return nil
	},
}

// hookPrecmdCmd is invoked by bash-preexec precmd hook (after every command).
// Pushes an event so the daemon can clear current_command and promote it to
// last_command, plus refresh CWD.
var hookPrecmdCmd = &cobra.Command{
	Use:   "precmd",
	Short: "Handle shell precmd event (just-finished command)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !wezterm.InWezterm() {
			return nil
		}
		ev := daemon.Event{
			Type:      daemon.EventPrecmd,
			MuxSocket: wezterm.MuxSocket(),
			PaneID:    wezterm.PaneID(),
			CWD:       currentCWD(),
			Timestamp: time.Now().UnixMilli(),
		}
		_ = daemon.PushEvent(ev)
		return nil
	},
}

// pushSessionEventIfInWezterm pushes a SessionStart/End event to the daemon
// when the current process is inside a wezterm pane. Best-effort.
func pushSessionEventIfInWezterm(t daemon.EventType, input hook.HookInput) {
	if !wezterm.InWezterm() {
		return
	}
	ev := daemon.Event{
		Type:      t,
		MuxSocket: wezterm.MuxSocket(),
		PaneID:    wezterm.PaneID(),
		SessionID: input.SessionID,
		PID:       os.Getppid(),
		CWD:       input.CWD,
	}
	_ = daemon.PushEvent(ev)
}

func runHook(handler func(*store.Store, hook.HookInput) error) error {
	_, err := runHookReturningInput(handler)
	return err
}

func runHookReturningInput(handler func(*store.Store, hook.HookInput) error) (hook.HookInput, error) {
	input, err := hook.ReadInput(os.Stdin)
	if err != nil {
		return input, err
	}

	s, err := store.Open(store.DefaultDBPath())
	if err != nil {
		return input, err
	}
	defer func() { _ = s.Close() }()

	return input, handler(s, input)
}

// currentCWD returns the process working directory (best-effort).
func currentCWD() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// --- Launch Command ---

var launchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Launch the interactive session picker TUI",
	RunE:  launchTUI,
}

func launchTUI(cmd *cobra.Command, args []string) error {
	project := flagProject
	if !flagAll && project == "" {
		var err error
		project, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}
	project = store.ResolvePath(project)

	s, err := store.Open(store.DefaultDBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	m := launcher.New(s, project, flagAll)
	p := tea.NewProgram(m, tea.WithAltScreen())

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("run TUI: %w", err)
	}

	result := finalModel.(launcher.Model).GetResult()
	if result == nil {
		return nil // User quit without selecting
	}

	return resumeSession(result.SessionID, result.Project, args)
}

func resumeSession(sessionID, project string, extraArgs []string) error {
	// Load config for additional claude args
	cfg, err := config.Load(config.DefaultConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not load config: %v\n", err)
	}

	// Build claude command: claude --resume <id> [config args] [-- extra args]
	claudeArgs := []string{"claude", "--resume", sessionID}
	claudeArgs = append(claudeArgs, cfg.ClaudeArgs()...)
	claudeArgs = append(claudeArgs, extraArgs...)

	fmt.Printf("Resuming session %s...\n", sessionID[:8])

	// Change to the project directory
	if err := os.Chdir(project); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not cd to %s: %v\n", project, err)
	}

	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found in PATH: %w", err)
	}

	return syscall.Exec(claudeBin, claudeArgs, os.Environ())
}

// --- List Command ---

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions (non-interactive)",
	RunE: func(cmd *cobra.Command, args []string) error {
		project := flagProject
		if !flagAll && project == "" {
			var err error
			project, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		project = store.ResolvePath(project)

		s, err := store.Open(store.DefaultDBPath())
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()

		var sessions []store.Session
		if flagAll || project == "" {
			sessions, err = s.ListAll()
		} else {
			sessions, err = s.ListByProject(project)
		}
		if err != nil {
			return err
		}

		if len(sessions) == 0 {
			fmt.Println("No sessions found.")
			return nil
		}

		if flagJSON {
			return printSessionsJSON(sessions)
		}

		// Table output
		fmt.Printf("%-8s  %-8s  %-10s  %-14s  %s\n", "STATUS", "ID", "LAST SEEN", "MODEL", "LAST PROMPT")
		fmt.Println("--------  --------  ----------  --------------  -----------")
		for _, sess := range sessions {
			status := "inactive"
			if sess.Active {
				status = "ACTIVE"
			}
			idShort := sess.ID
			if len(idShort) > 8 {
				idShort = idShort[:8]
			}
			relTime := launcher.FormatRelativeTime(sess.LastActivity)
			model := sess.Model
			if len(model) > 14 {
				model = model[:14]
			}
			prompt := sess.LastPrompt
			if prompt == "" {
				prompt = "(none)"
			}
			if len(prompt) > 60 {
				prompt = prompt[:57] + "..."
			}
			fmt.Printf("%-8s  %-8s  %-10s  %-14s  %s\n", status, idShort, relTime, model, prompt)
		}
		return nil
	},
}

func printSessionsJSON(sessions []store.Session) error {
	fmt.Println("[")
	for i, sess := range sessions {
		active := "false"
		if sess.Active {
			active = "true"
		}
		fmt.Printf(`  {"id":"%s","project":"%s","active":%s,"model":"%s","last_prompt":"%s","last_activity":%d}`,
			sess.ID, sess.Project, active, sess.Model, escapeJSON(sess.LastPrompt), sess.LastActivity)
		if i < len(sessions)-1 {
			fmt.Println(",")
		} else {
			fmt.Println()
		}
	}
	fmt.Println("]")
	return nil
}

func escapeJSON(s string) string {
	var result []byte
	for _, c := range s {
		switch c {
		case '"':
			result = append(result, '\\', '"')
		case '\\':
			result = append(result, '\\', '\\')
		case '\n':
			result = append(result, '\\', 'n')
		case '\r':
			result = append(result, '\\', 'r')
		case '\t':
			result = append(result, '\\', 't')
		default:
			result = append(result, byte(c))
		}
	}
	return string(result)
}

// --- Cleanup Command ---

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove old inactive sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := store.Open(store.DefaultDBPath())
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()

		removed, err := s.Cleanup(flagDays)
		if err != nil {
			return err
		}

		fmt.Printf("Removed %d inactive sessions older than %d days.\n", removed, flagDays)
		return nil
	},
}

// --- Config Command ---

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "View or modify CST configuration",
	Long:  "View or modify CST configuration stored in ~/.cst/config.json.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(config.DefaultConfigPath())
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		fmt.Printf("\nConfig path: %s\n", config.DefaultConfigPath())
		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a config value",
	Long: `Set a configuration value. Available keys:
  dangerously_skip_permissions  (true/false) - Always pass --dangerously-skip-permissions to claude
  extra_args                    (comma-separated) - Additional args to pass to claude on resume`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgPath := config.DefaultConfigPath()
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}

		key, value := args[0], args[1]
		switch key {
		case "dangerously_skip_permissions":
			switch value {
			case "true":
				cfg.DangerouslySkipPermissions = true
			case "false":
				cfg.DangerouslySkipPermissions = false
			default:
				return fmt.Errorf("invalid value %q for %s, expected true or false", value, key)
			}
		case "extra_args":
			if value == "" || value == "[]" {
				cfg.ExtraArgs = nil
			} else {
				cfg.ExtraArgs = splitArgs(value)
			}
		default:
			return fmt.Errorf("unknown config key: %q\nAvailable: dangerously_skip_permissions, extra_args", key)
		}

		if err := config.Save(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("Set %s = %s\n", key, value)
		return nil
	},
}

func splitArgs(s string) []string {
	var args []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			args = append(args, part)
		}
	}
	return args
}

var configReplayListCmd = &cobra.Command{
	Use:   "replay-list",
	Short: "List replay-enabled commands (annotated with (default) for OOTB entries the user hasn't touched)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgPath := config.DefaultConfigPath()
		raw, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		// Detect whether the user has explicitly written ReplayCommands.
		userTouched := raw.ReplayCommands != nil
		resolved := raw.WithDefaults()

		if len(resolved.ReplayCommands) == 0 {
			fmt.Println("(none)")
			return nil
		}

		defaults := map[string]bool{}
		for _, n := range config.Defaults().ReplayCommands {
			defaults[n] = true
		}
		for _, n := range resolved.ReplayCommands {
			annotation := ""
			if defaults[n] && !userTouched {
				annotation = "  (default)"
			}
			fmt.Printf("%s%s\n", n, annotation)
		}
		return nil
	},
}

var configReplayAddCmd = &cobra.Command{
	Use:   "replay-add <name>",
	Short: "Add a command to the replay-on-restore registry",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		if name == "" {
			return fmt.Errorf("command name cannot be empty")
		}
		cfgPath := config.DefaultConfigPath()
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		// Materialize the OOTB list before mutating so we persist a complete picture.
		cfg = cfg.WithDefaults()
		if added := cfg.AddReplayCommand(name); !added {
			fmt.Printf("%q is already in the replay list\n", name)
			return nil
		}
		if err := config.Save(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("Added %q to replay list (%d total)\n", name, len(cfg.ReplayCommands))
		return nil
	},
}

var configReplayRemoveCmd = &cobra.Command{
	Use:   "replay-remove <name>",
	Short: "Remove a command from the replay-on-restore registry",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		cfgPath := config.DefaultConfigPath()
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		cfg = cfg.WithDefaults()
		if removed := cfg.RemoveReplayCommand(name); !removed {
			fmt.Printf("%q is not in the replay list\n", name)
			return nil
		}
		if err := config.Save(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("Removed %q from replay list (%d remaining)\n", name, len(cfg.ReplayCommands))
		return nil
	},
}

func init() {
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configReplayListCmd)
	configCmd.AddCommand(configReplayAddCmd)
	configCmd.AddCommand(configReplayRemoveCmd)
}

// --- Version Command ---

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("cst %s\n", Version)
		fmt.Printf("  commit: %s\n", Commit)
		fmt.Printf("  built:  %s\n", BuildDate)
	},
}

// --- Snapshot Command (thin daemon client) ---

var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Push a snapshot-request event to the cst daemon (sub-ms)",
	Long: `Push a snapshot request to the cst daemon over its Unix socket.
The daemon coalesces snapshot requests inside a ~100ms window and runs
` + "`wezterm cli list`" + ` once per window. If no daemon is running, this command
auto-spawns one and retries; if that also fails, falls back to an inline snapshot.

Always non-blocking on the happy path (<1 ms).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Push and exit — never surface errors to the caller (this is invoked
		// from shell hooks where the prompt must never fail).
		_ = daemon.PushEvent(daemon.Event{Type: daemon.EventSnapshotRequest})
		return nil
	},
}

// --- Daemon Command ---

var (
	flagDaemonSocket   string
	flagDaemonWezDB    string
	flagDaemonSessions string
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run the cst snapshot daemon (per-user, blocking)",
	Long: `Run the cst snapshot daemon. Listens on $XDG_RUNTIME_DIR/cst-daemon-$UID.sock
and coalesces snapshot requests into one ` + "`wezterm cli list`" + ` per ~100ms.

Refuses to start if its socket is already bound by another daemon. Self-exits
after 30 minutes of no events.

Usually started by systemd-user (see ` + "`cst setup-daemon`" + `) or auto-spawned by
` + "`cst snapshot`" + ` / shell hooks. Rarely invoked directly.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := daemon.New(daemon.Options{
			SocketPath:     flagDaemonSocket,
			WezStorePath:   flagDaemonWezDB,
			SessionsDBPath: flagDaemonSessions,
		})
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()

		if err := daemon.WritePIDFile(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: WritePIDFile: %v\n", err)
		}
		defer func() { _ = daemon.RemovePIDFile() }()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Graceful shutdown on SIGTERM/SIGINT.
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			s := <-sigs
			fmt.Fprintf(os.Stderr, "received %s; draining\n", s)
			cancel()
		}()

		return d.Serve(ctx)
	},
}

func init() {
	daemonCmd.Flags().StringVar(&flagDaemonSocket, "socket", "",
		"Unix socket path (defaults to $XDG_RUNTIME_DIR/cst-daemon-$UID.sock)")
	daemonCmd.Flags().StringVar(&flagDaemonWezDB, "wez-db", "",
		"Path to wezterm.db (defaults to ~/.cst/wezterm.db)")
	daemonCmd.Flags().StringVar(&flagDaemonSessions, "sessions-db", "",
		"Path to sessions.db to ATTACH (defaults to ~/.cst/sessions.db if present)")
}

// --- Restore Command ---

var (
	flagRestoreDryRun       bool
	flagRestoreSkipFirst    bool
	flagRestoreSpawnIfEmpty bool
	flagRestoreWorkspace    string
	flagRestoreWezDB        string
)

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Reconstruct the wezterm layout from the latest snapshot",
	Long: `Read the latest snapshot from ~/.cst/wezterm.db and reconstruct the layout
via ` + "`wezterm cli spawn`" + `. Each window becomes a new wezterm window; each tab
becomes a tab in that window; additional panes within a tab are re-split into
that tab via ` + "`wezterm cli split-pane`" + ` (split direction is approximate —
wezterm does not expose the original geometry).

Per-pane spawn command is decided by the replay-command registry:
  - claude (with linked session): ` + "`claude --resume <id>`" + ` plus any configured
    claude args (e.g. --dangerously-skip-permissions)
  - other registry hit: replays the literal captured command
  - registry miss: opens a plain shell in the saved CWD

Typically run from wezterm's gui-startup hook with --spawn-if-empty so a default
window opens when there's no saved layout.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := snapshot.Restore(ctx, snapshot.RestoreOptions{
			WezDBPath:    flagRestoreWezDB,
			Workspace:    flagRestoreWorkspace,
			SkipFirst:    flagRestoreSkipFirst,
			SpawnIfEmpty: flagRestoreSpawnIfEmpty,
			DryRun:       flagRestoreDryRun,
		})
		return err
	},
}

func init() {
	restoreCmd.Flags().BoolVar(&flagRestoreDryRun, "dry-run", false,
		"Print the planned wezterm cli calls without executing")
	restoreCmd.Flags().BoolVar(&flagRestoreSkipFirst, "skip-first", false,
		"Skip the very first pane (legacy; gui-startup now uses --spawn-if-empty)")
	restoreCmd.Flags().BoolVar(&flagRestoreSpawnIfEmpty, "spawn-if-empty", false,
		"Open a default window if there's no snapshot to restore (for gui-startup)")
	restoreCmd.Flags().StringVar(&flagRestoreWorkspace, "workspace", "",
		"Only restore windows in this workspace")
	restoreCmd.Flags().StringVar(&flagRestoreWezDB, "wez-db", "",
		"Path to wezterm.db (defaults to ~/.cst/wezterm.db)")
}

// --- Setup-Shell Command ---

var (
	flagSetupShellShell     string
	flagSetupShellPrint     bool
	flagSetupShellUninstall bool
)

var setupShellCmd = &cobra.Command{
	Use:   "setup-shell",
	Short: "Install (or remove) cst's shell hooks in your rc file",
	Long: `Install cst's per-prompt hooks into your shell rc file (~/.bashrc,
~/.zshrc, or ~/.config/fish/config.fish), idempotently.

The hooks push events to the cst daemon on every prompt cycle so the daemon
can track per-pane state without polling.

For bash, the bash-preexec.sh helper is downloaded to ~/.cst/ first
(rcaloras/bash-preexec, pinned release). zsh and fish use their native
precmd/preexec hooks; no extra download.

Re-running is safe — the existing cst block is replaced in place.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		shell := shellsetup.Shell(strings.ToLower(flagSetupShellShell))
		if shell == "" {
			shell = detectShell()
		}
		inst, err := shellsetup.NewInstallerForShell(shell)
		if err != nil {
			return err
		}

		if flagSetupShellPrint {
			fmt.Println(shellsetup.BlockStart)
			fmt.Print(inst.Snippet())
			fmt.Println(shellsetup.BlockEnd)
			return nil
		}

		if flagSetupShellUninstall {
			changed, err := shellsetup.Uninstall(inst)
			if err != nil {
				return err
			}
			if changed {
				fmt.Printf("Removed cst block from %s\n", inst.RCFilePath())
			} else {
				fmt.Printf("No cst block found in %s\n", inst.RCFilePath())
			}
			return nil
		}

		changed, err := shellsetup.Install(inst)
		if err != nil {
			return err
		}
		if changed {
			fmt.Printf("Installed cst shell hooks into %s\n", inst.RCFilePath())
		} else {
			fmt.Printf("cst shell hooks already current in %s (no changes)\n", inst.RCFilePath())
		}
		fmt.Println("\nNext step: open a new terminal, or `source` your rc file.")
		return nil
	},
}

func init() {
	setupShellCmd.Flags().StringVar(&flagSetupShellShell, "shell", "",
		"Target shell: bash, zsh, or fish (defaults to $SHELL detection)")
	setupShellCmd.Flags().BoolVar(&flagSetupShellPrint, "print", false,
		"Print the snippet to stdout instead of writing to your rc file")
	setupShellCmd.Flags().BoolVar(&flagSetupShellUninstall, "uninstall", false,
		"Remove the cst block from your rc file")
}

// detectShell inspects $SHELL and returns the matching shellsetup.Shell.
// Falls back to bash if it can't tell.
func detectShell() shellsetup.Shell {
	sh := os.Getenv("SHELL")
	switch {
	case strings.HasSuffix(sh, "/zsh"):
		return shellsetup.ShellZsh
	case strings.HasSuffix(sh, "/fish"):
		return shellsetup.ShellFish
	default:
		return shellsetup.ShellBash
	}
}

// --- Setup-Wezterm Command ---

var (
	flagSetupWeztermLuaPath    string
	flagSetupWeztermModulePath string
	flagSetupWeztermPrint      bool
	flagSetupWeztermUninstall  bool
)

var setupWeztermCmd = &cobra.Command{
	Use:   "setup-wezterm",
	Short: "Install (or remove) cst's wezterm Lua integration",
	Long: `Install cst's wezterm event handlers + restore hook.

Writes two files:
  - ~/.config/wezterm/cst.lua  — the module (gui-startup, new-tab-button-click,
                                 mux-is-process-stateful, window-focus-changed,
                                 plus keybinding wrappers)
  - ~/.wezterm.lua             — appends a short loader block (between markers)
                                 that puts the module on the Lua path and
                                 requires it.

Idempotent: re-running replaces the loader block and rewrites cst.lua.
Existing wezterm config is preserved.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := wezsetup.Options{
			WeztermLuaPath: flagSetupWeztermLuaPath,
			CstModulePath:  flagSetupWeztermModulePath,
		}
		if flagSetupWeztermPrint {
			fmt.Println("# ~/.wezterm.lua loader block:")
			fmt.Println(wezsetup.BlockStart)
			fmt.Print(wezsetup.LoaderSnippet)
			fmt.Println(wezsetup.BlockEnd)
			fmt.Println("\n# ~/.config/wezterm/cst.lua module:")
			fmt.Print(wezsetup.LuaModule)
			return nil
		}
		if flagSetupWeztermUninstall {
			changed, err := wezsetup.Uninstall(opts)
			if err != nil {
				return err
			}
			weztermLua, cstModule := wezsetup.PathsForDisplay(opts)
			if changed {
				fmt.Printf("Removed wezterm integration from %s\n", weztermLua)
				fmt.Printf("Removed %s\n", cstModule)
			} else {
				fmt.Println("No wezterm integration found")
			}
			return nil
		}
		changed, err := wezsetup.Install(opts)
		if err != nil {
			return err
		}
		weztermLua, cstModule := wezsetup.PathsForDisplay(opts)
		if changed {
			fmt.Printf("Installed wezterm module → %s\n", cstModule)
			fmt.Printf("Updated loader block in %s\n", weztermLua)
		} else {
			fmt.Println("wezterm integration already current (no changes)")
		}
		fmt.Println("\nNext step: restart wezterm (or run `wezterm cli kill-pane` to reload Lua).")
		return nil
	},
}

func init() {
	setupWeztermCmd.Flags().StringVar(&flagSetupWeztermLuaPath, "wezterm-config", "",
		"Path to ~/.wezterm.lua (defaults to that)")
	setupWeztermCmd.Flags().StringVar(&flagSetupWeztermModulePath, "module-path", "",
		"Path to install cst.lua module (defaults to ~/.config/wezterm/cst.lua)")
	setupWeztermCmd.Flags().BoolVar(&flagSetupWeztermPrint, "print", false,
		"Print the snippets to stdout instead of writing")
	setupWeztermCmd.Flags().BoolVar(&flagSetupWeztermUninstall, "uninstall", false,
		"Remove the wezterm integration")
}

// --- Setup-Daemon Command ---

var (
	flagSetupDaemonEnable    bool
	flagSetupDaemonUninstall bool
	flagSetupDaemonPrint     bool
)

var setupDaemonCmd = &cobra.Command{
	Use:   "setup-daemon",
	Short: "Install (or remove) cst-daemon systemd-user service",
	Long: `Install ~/.config/systemd/user/cst-daemon.service so systemd-user supervises
the cst daemon. With --enable, also runs:
  systemctl --user enable --now cst-daemon

Use --uninstall to disable and remove the service.
Use --print to print the unit file without writing anything.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := daemonsetup.Options{EnableNow: flagSetupDaemonEnable}

		if flagSetupDaemonPrint {
			exe, _ := os.Executable()
			fmt.Printf(daemonsetup.UnitTemplate, exe)
			return nil
		}

		if flagSetupDaemonUninstall {
			res, err := daemonsetup.Uninstall(opts)
			if err != nil {
				return err
			}
			fmt.Printf("Removed %s\n", res.UnitPath)
			return nil
		}

		res, err := daemonsetup.Install(opts)
		if err != nil {
			return err
		}
		if res.Wrote {
			fmt.Printf("Wrote %s\n", res.UnitPath)
		} else {
			fmt.Printf("Unit file already current: %s\n", res.UnitPath)
		}
		if res.Enabled {
			fmt.Println("Enabled and started cst-daemon (systemd will restart on reboot).")
		} else if flagSetupDaemonEnable {
			fmt.Println("Note: --enable was requested but enable step did not run.")
		} else {
			fmt.Println("\nTo enable and start:")
			fmt.Println("  systemctl --user enable --now cst-daemon")
		}
		return nil
	},
}

func init() {
	setupDaemonCmd.Flags().BoolVar(&flagSetupDaemonEnable, "enable", false,
		"Also run `systemctl --user enable --now cst-daemon`")
	setupDaemonCmd.Flags().BoolVar(&flagSetupDaemonUninstall, "uninstall", false,
		"Disable and remove the service")
	setupDaemonCmd.Flags().BoolVar(&flagSetupDaemonPrint, "print", false,
		"Print the unit file to stdout")
}

// --- Setup (umbrella) Command ---

var (
	flagSetupNoSystemd bool
	flagSetupUninstall bool
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "One-shot: shell hooks + wezterm integration + daemon supervision",
	Long: `Runs setup-shell, setup-wezterm, and (if systemd-user is available) setup-daemon
--enable in sequence. Use --uninstall to reverse all three in one go.

Use --no-systemd to skip the daemon supervision step (wezterm will auto-start
the daemon on demand instead).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagSetupUninstall {
			// Best-effort: try each step, report results, keep going on errors.
			runStep := func(name string, fn func() error) {
				if err := fn(); err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
				}
			}
			runStep("setup-shell --uninstall", func() error {
				inst, err := shellsetup.NewInstallerForShell(detectShell())
				if err != nil {
					return err
				}
				_, err = shellsetup.Uninstall(inst)
				return err
			})
			runStep("setup-wezterm --uninstall", func() error {
				_, err := wezsetup.Uninstall(wezsetup.Options{})
				return err
			})
			runStep("setup-daemon --uninstall", func() error {
				_, err := daemonsetup.Uninstall(daemonsetup.Options{})
				return err
			})
			fmt.Println("\nDone. Restart your terminal for changes to take effect.")
			return nil
		}

		// Install.
		inst, err := shellsetup.NewInstallerForShell(detectShell())
		if err != nil {
			return fmt.Errorf("shell install: %w", err)
		}
		shellChanged, err := shellsetup.Install(inst)
		if err != nil {
			return fmt.Errorf("shell install: %w", err)
		}
		if shellChanged {
			fmt.Printf("✓ Shell hooks installed → %s\n", inst.RCFilePath())
		} else {
			fmt.Printf("· Shell hooks already current → %s\n", inst.RCFilePath())
		}

		wezChanged, err := wezsetup.Install(wezsetup.Options{})
		if err != nil {
			return fmt.Errorf("wezterm install: %w", err)
		}
		weztermLua, _ := wezsetup.PathsForDisplay(wezsetup.Options{})
		if wezChanged {
			fmt.Printf("✓ Wezterm integration installed → %s\n", weztermLua)
		} else {
			fmt.Printf("· Wezterm integration already current → %s\n", weztermLua)
		}

		if flagSetupNoSystemd || !daemonsetup.SystemdAvailable() {
			if flagSetupNoSystemd {
				fmt.Println("· Skipping systemd setup (--no-systemd)")
			} else {
				fmt.Println("· Skipping systemd setup (systemd-user not detected on this host)")
				fmt.Println("  The daemon will auto-start on first event via the wezterm hook.")
			}
		} else {
			res, err := daemonsetup.Install(daemonsetup.Options{EnableNow: true})
			if err != nil {
				return fmt.Errorf("daemon install: %w", err)
			}
			if res.Enabled {
				fmt.Printf("✓ Daemon enabled via systemd → %s\n", res.UnitPath)
			}
		}

		fmt.Println("\nDone. Restart your terminal (or open a new wezterm window) to start using cst.")
		return nil
	},
}

func init() {
	setupCmd.Flags().BoolVar(&flagSetupNoSystemd, "no-systemd", false,
		"Skip the systemd-user supervision step")
	setupCmd.Flags().BoolVar(&flagSetupUninstall, "uninstall", false,
		"Reverse all setup steps")
}

// --- Daemon Status Command ---

var daemonStatusCmd = &cobra.Command{
	Use:   "daemon-status",
	Short: "Print daemon status, socket path, PID",
	RunE: func(cmd *cobra.Command, args []string) error {
		sock := daemon.SocketPath()
		pid, _ := daemon.ReadPIDFile()
		reachable := daemon.IsDaemonReachable()

		fmt.Printf("socket:    %s\n", sock)
		fmt.Printf("pid file:  %s\n", daemon.PIDFilePath())
		fmt.Printf("pid:       %d\n", pid)
		fmt.Printf("reachable: %t\n", reachable)
		if !reachable {
			fmt.Println("\nDaemon does not appear to be running. To start:")
			fmt.Println("  systemctl --user start cst-daemon    # if installed via cst setup-daemon")
			fmt.Println("  cst daemon &                          # one-off foreground (testing)")
		}
		return nil
	},
}
