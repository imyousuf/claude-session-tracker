package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/imyousuf/claude-session-tracker/internal/config"
	"github.com/imyousuf/claude-session-tracker/internal/hook"
	"github.com/imyousuf/claude-session-tracker/internal/launcher"
	"github.com/imyousuf/claude-session-tracker/internal/store"
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
}

var hookSessionStartCmd = &cobra.Command{
	Use:   "session-start",
	Short: "Handle SessionStart hook event",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runHook(hook.HandleSessionStart)
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
		return runHook(hook.HandleSessionEnd)
	},
}

func runHook(handler func(*store.Store, hook.HookInput) error) error {
	input, err := hook.ReadInput(os.Stdin)
	if err != nil {
		return err
	}

	s, err := store.Open(store.DefaultDBPath())
	if err != nil {
		return err
	}
	defer s.Close()

	return handler(s, input)
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
	defer s.Close()

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
		defer s.Close()

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
		defer s.Close()

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

func init() {
	configCmd.AddCommand(configSetCmd)
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
