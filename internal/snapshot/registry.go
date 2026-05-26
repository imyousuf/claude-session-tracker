package snapshot

import (
	"strings"

	"github.com/imyousuf/claude-session-tracker/internal/store"
)

// ReplayPlan describes what `cst restore` will spawn in a single pane.
type ReplayPlan struct {
	CWD     string   // working directory for the new pane
	Command []string // empty = default shell
	Matched string   // diagnostic: which registry name (if any) matched
	Reason  string   // human-readable reason for the plan, useful for --dry-run
}

// Resolver maps a stored pane to a spawn plan using the user's ReplayCommands.
type Resolver struct {
	registry map[string]struct{}
}

// NewResolver builds a Resolver from a list of replay-able command names.
func NewResolver(replayCommands []string) *Resolver {
	r := &Resolver{registry: make(map[string]struct{}, len(replayCommands))}
	for _, n := range replayCommands {
		r.registry[strings.TrimSpace(n)] = struct{}{}
	}
	return r
}

// Resolve produces the spawn plan for a pane, implementing the
// "absolute last command" rule:
//
//  1. Inspect the pane's `current_cmd` (was running at snapshot time); if empty,
//     fall back to `last_cmd` (most recently finished); if both empty, no replay.
//  2. Extract the command name = first token of the resolved string, after
//     skipping leading VAR=value env prefixes (e.g. `FOO=bar tomoe` → `tomoe`).
//  3. If that name is in ReplayCommands, replay:
//     - `claude` special case: when claude_session_id is set on the pane,
//       spawn `claude --resume <id>` regardless of the captured literal.
//     - default: spawn `sh -c "exec <captured>"` so any user quoting works.
//  4. Otherwise: open a plain shell in the saved CWD.
func (r *Resolver) Resolve(pane store.WezPane) ReplayPlan {
	plan := ReplayPlan{CWD: pane.CWD}

	activeCmd := pane.CurrentCmd
	if activeCmd == "" {
		activeCmd = pane.LastCmd
	}
	if activeCmd == "" {
		plan.Reason = "no command captured for pane; opening default shell"
		return plan
	}

	name := firstTokenSkippingEnv(activeCmd)
	if _, ok := r.registry[name]; !ok {
		plan.Reason = "command " + quoteForDisplay(name) + " not in replay registry; opening default shell"
		return plan
	}
	plan.Matched = name

	// Claude special case.
	if name == "claude" && pane.ClaudeSessionID != "" {
		plan.Command = []string{"claude", "--resume", pane.ClaudeSessionID}
		plan.Reason = "claude session " + pane.ClaudeSessionID + " linked; spawning with --resume"
		return plan
	}

	// Default: replay literal via `sh -c "exec ..."` for quote-safety.
	plan.Command = []string{"sh", "-c", "exec " + activeCmd}
	plan.Reason = "replaying literal: " + activeCmd
	return plan
}

// firstTokenSkippingEnv returns the first non-VAR=value token in cmd.
// "FOO=1 BAR=2 tomoe start" → "tomoe"
func firstTokenSkippingEnv(cmd string) string {
	for _, part := range strings.Fields(cmd) {
		eq := strings.IndexByte(part, '=')
		if eq > 0 && isLikelyEnvKey(part[:eq]) {
			continue
		}
		return part
	}
	return ""
}

func isLikelyEnvKey(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		isUnderscore := c == '_'
		if !(isUpper || isDigit || isUnderscore) {
			return false
		}
	}
	// Must start with uppercase or underscore.
	c := s[0]
	return (c >= 'A' && c <= 'Z') || c == '_'
}

func quoteForDisplay(s string) string {
	return `"` + s + `"`
}
