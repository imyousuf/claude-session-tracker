// Package wezterm wraps the `wezterm cli` subcommand.
//
// It does not depend on any other cst internal package — pure adapter over
// wezterm's RPC. Imports allowed: stdlib only.
package wezterm

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Runner abstracts the actual `wezterm cli ...` invocation so tests can inject fakes.
type Runner interface {
	Run(args ...string) ([]byte, error)
	// RunStdin runs `wezterm cli args...` with stdin fed from the given string.
	// Used by send-text so arbitrary command text bypasses shell-arg quoting.
	RunStdin(stdin string, args ...string) ([]byte, error)
}

// CmdRunner runs `wezterm cli ...` via os/exec. Used in production.
type CmdRunner struct {
	Bin string // override binary; defaults to "wezterm" via $PATH
}

func (r CmdRunner) Run(args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "wezterm"
	}
	cmd := exec.Command(bin, append([]string{"cli"}, args...)...)
	return cmd.Output()
}

func (r CmdRunner) RunStdin(stdin string, args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "wezterm"
	}
	cmd := exec.Command(bin, append([]string{"cli"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.Output()
}

// DefaultRunner is the package-level runner; tests substitute via SetRunner.
var DefaultRunner Runner = CmdRunner{}

// SetRunner installs a custom Runner. Returns a restorer that callers should defer.
func SetRunner(r Runner) func() {
	prev := DefaultRunner
	DefaultRunner = r
	return func() { DefaultRunner = prev }
}

// --- list ---

// RawPane mirrors a single record from `wezterm cli list --format json`.
// Field set is intentionally narrow — only what cst actually reads.
type RawPane struct {
	WindowID    int64  `json:"window_id"`
	TabID       int64  `json:"tab_id"`
	PaneID      int64  `json:"pane_id"`
	Workspace   string `json:"workspace"`
	Title       string `json:"title"`
	TabTitle    string `json:"tab_title"`
	WindowTitle string `json:"window_title"`
	CWD         string `json:"cwd"`      // "file://hostname/path/"
	TTYName     string `json:"tty_name"` // "/dev/pts/N" on Linux
	IsActive    bool   `json:"is_active"`
	IsZoomed    bool   `json:"is_zoomed"`
	LeftCol     int    `json:"left_col"`
	TopRow      int    `json:"top_row"`
	Size        Size   `json:"size"`
}

// Size describes a pane's dimensions in cells.
type Size struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// List runs `wezterm cli list --format json` and returns parsed panes.
func List() ([]RawPane, error) {
	_, panes, err := ListWithRaw()
	return panes, err
}

// ListWithRaw returns both the raw JSON bytes (for content-hash skip) and
// the parsed panes. The bytes are the authoritative input for hashing —
// re-marshalling parsed structs would produce a different byte sequence.
func ListWithRaw() ([]byte, []RawPane, error) {
	raw, err := DefaultRunner.Run("list", "--format", "json")
	if err != nil {
		return nil, nil, fmt.Errorf("wezterm cli list: %w", err)
	}
	panes, err := ParseList(raw)
	return raw, panes, err
}

// ParseList decodes raw bytes from `wezterm cli list --format json`.
func ParseList(raw []byte) ([]RawPane, error) {
	var panes []RawPane
	if err := json.Unmarshal(raw, &panes); err != nil {
		return nil, fmt.Errorf("parse wezterm list output: %w", err)
	}
	return panes, nil
}

// ParseCWD converts a wezterm CWD URL ("file://hostname/path/") to a local path.
// Returns the input string unchanged if parsing fails.
func ParseCWD(s string) string {
	if s == "" {
		return s
	}
	if !strings.HasPrefix(s, "file://") {
		// Already a plain path.
		return s
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	p := u.Path
	if p == "" {
		return s
	}
	// Trim trailing slash that wezterm includes for directory CWDs (but keep "/").
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return filepath.Clean(p)
}

// --- spawn ---

// SpawnArgs configures a `wezterm cli spawn` call.
type SpawnArgs struct {
	CWD       string   // working directory for the new pane; empty = inherit
	Workspace string   // target workspace; empty = current
	NewWindow bool     // if true, spawns into a new window instead of new tab
	WindowID  int64    // if set (and !NewWindow), spawns a tab in this window
	Command   []string // empty = default shell
}

// Spawn runs `wezterm cli spawn` and returns the pane ID of the new pane.
func Spawn(args SpawnArgs) (int64, error) {
	a := []string{"spawn"}
	if args.NewWindow {
		a = append(a, "--new-window")
	}
	if !args.NewWindow && args.WindowID != 0 {
		a = append(a, "--window-id", strconv.FormatInt(args.WindowID, 10))
	}
	if args.CWD != "" {
		a = append(a, "--cwd", args.CWD)
	}
	if args.Workspace != "" {
		a = append(a, "--workspace", args.Workspace)
	}
	if len(args.Command) > 0 {
		a = append(a, "--")
		a = append(a, args.Command...)
	}
	out, err := DefaultRunner.Run(a...)
	if err != nil {
		return 0, fmt.Errorf("wezterm cli spawn: %w", err)
	}
	return parsePaneID(out)
}

// --- split-pane ---

// SplitDirection is the direction in which to split, relative to PaneID.
type SplitDirection string

const (
	SplitRight  SplitDirection = "right"
	SplitLeft   SplitDirection = "left"
	SplitTop    SplitDirection = "top"
	SplitBottom SplitDirection = "bottom"
)

// SplitArgs configures a `wezterm cli split-pane` call.
type SplitArgs struct {
	PaneID    int64
	Direction SplitDirection
	CWD       string
	Command   []string
}

// SplitPane runs `wezterm cli split-pane` and returns the new pane ID.
func SplitPane(args SplitArgs) (int64, error) {
	a := []string{"split-pane", "--pane-id", strconv.FormatInt(args.PaneID, 10)}
	switch args.Direction {
	case SplitRight:
		a = append(a, "--right")
	case SplitLeft:
		a = append(a, "--left")
	case SplitTop:
		a = append(a, "--top")
	case SplitBottom:
		a = append(a, "--bottom")
	default:
		return 0, fmt.Errorf("invalid split direction: %q", args.Direction)
	}
	if args.CWD != "" {
		a = append(a, "--cwd", args.CWD)
	}
	if len(args.Command) > 0 {
		a = append(a, "--")
		a = append(a, args.Command...)
	}
	out, err := DefaultRunner.Run(a...)
	if err != nil {
		return 0, fmt.Errorf("wezterm cli split-pane: %w", err)
	}
	return parsePaneID(out)
}

func parsePaneID(out []byte) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// --- send-text ---

// SendTextArgs configures a `wezterm cli send-text` call.
type SendTextArgs struct {
	PaneID  int64
	Text    string // sent verbatim; include a trailing "\r" to submit a shell line
	NoPaste bool   // --no-paste: deliver as plain keystrokes, not a bracketed paste
}

// SendText types Text into the given pane via `wezterm cli send-text`. The text
// is fed on stdin (not as an argument) so arbitrary command lines aren't subject
// to shell-arg quoting. With NoPaste set and a trailing carriage return, the
// shell in the target pane executes the line (verified empirically: bracketed
// paste does NOT submit, plain keystrokes + "\r" do).
func SendText(args SendTextArgs) error {
	a := []string{"send-text", "--pane-id", strconv.FormatInt(args.PaneID, 10)}
	if args.NoPaste {
		a = append(a, "--no-paste")
	}
	if _, err := DefaultRunner.RunStdin(args.Text, a...); err != nil {
		return fmt.Errorf("wezterm cli send-text: %w", err)
	}
	return nil
}
