package snapshot

import (
	"strings"

	"github.com/imyousuf/claude-session-tracker/internal/store"
)

// ReplayPlan describes what `cst restore` will spawn in a single pane.
type ReplayPlan struct {
	CWD     string   // working directory for the new pane
	Command []string // empty = default shell (used by spawn-with-command callers + dry-run)
	// SendLine is the literal shell line to TYPE into the pane via send-text
	// (skeleton-first restore spawns plain shells, then sends this). Empty means
	// send nothing (plain shell). For linked sessions this is the provider's
	// resume command (`claude --resume <id>` or `codex resume <id>`);
	// for literal replay it's the verbatim captured command (no sh -c wrapper —
	// we type into an interactive shell).
	SendLine string
	Matched  string // diagnostic: which registry name (if any) matched
	Reason   string // human-readable reason for the plan, useful for --dry-run
}

// Resolver maps a stored pane to a spawn plan using the user's ReplayCommands.
type Resolver struct {
	registry map[string]struct{}
	// claudeArgs are appended to `claude --resume <id>` (e.g.
	// --dangerously-skip-permissions and any extra_args from config). They are
	// NOT applied to the literal-replay fallback, which already carries whatever
	// flags the user originally typed.
	claudeArgs []string
}

// NewResolver builds a Resolver from a list of replay-able command names and the
// extra args to append when resuming a linked claude session.
func NewResolver(replayCommands []string, claudeArgs []string) *Resolver {
	r := &Resolver{
		registry:   make(map[string]struct{}, len(replayCommands)),
		claudeArgs: claudeArgs,
	}
	for _, n := range replayCommands {
		r.registry[strings.TrimSpace(n)] = struct{}{}
	}
	return r
}

// Resolve produces the spawn plan for a pane, implementing the
// "absolute last command" rule:
//
//  1. Inspect the pane's `current_cmd` (was running at snapshot time); if empty,
//     fall back to `last_cmd` (most recently finished).
//  2. Extract the command name = first token of the resolved string, after
//     skipping leading VAR=value env prefixes (e.g. `FOO=bar tomoe` → `tomoe`).
//  3. Provider resume special case (checked BEFORE the registry gate): if a
//     provider/session ID is linked to the pane and the captured command is a
//     known launcher, use the provider's resume command. Claude gets
//     `claude --resume <id>` plus configured Claude args; Codex gets
//     `codex resume <id>`. The linked session is a stronger signal than the
//     registry, so this fires even when the launcher isn't registered.
//  4. Otherwise, if the command name is in ReplayCommands, replay the literal
//     via `sh -c "exec <captured>"` so any user quoting works.
//  5. Otherwise (no command, or not in registry): open a plain shell.
func (r *Resolver) Resolve(pane store.WezPane) ReplayPlan {
	plan := ReplayPlan{CWD: pane.CWD}

	activeCmd := pane.CurrentCmd
	if activeCmd == "" {
		activeCmd = pane.LastCmd
	}

	name := firstTokenSkippingEnv(activeCmd)

	provider := store.NormalizeProvider(pane.SessionProvider)
	sessionID := pane.SessionID
	if sessionID == "" && pane.ClaudeSessionID != "" {
		provider = store.ProviderClaude
		sessionID = pane.ClaudeSessionID
	}
	// A lifecycle hook can arrive before the shell's asynchronous preexec event.
	// In that window the provider/session link is authoritative even though no
	// launcher command has been captured yet. A different captured command still
	// blocks provider resume so stale links cannot replace unrelated commands.
	if sessionID != "" && (name == "" || isSessionLauncher(provider, name)) {
		var cmd []string
		switch provider {
		case store.ProviderClaude:
			cmd = []string{"claude", "--resume", sessionID}
			cmd = append(cmd, r.claudeArgs...)
		case store.ProviderCodex:
			cmd = []string{"codex", "resume", sessionID}
		}
		if len(cmd) == 0 {
			// Unknown providers remain eligible for literal registry replay.
			goto literalReplay
		}
		plan.Command = cmd
		plan.SendLine = shellJoin(cmd)
		plan.Matched = name
		plan.Reason = provider + " session " + sessionID + " linked"
		if name != "" {
			plan.Reason += " (via " + name + ")"
		}
		plan.Reason += "; resuming"
		if provider == store.ProviderClaude && len(r.claudeArgs) > 0 {
			plan.Reason += " " + strings.Join(r.claudeArgs, " ")
		}
		return plan
	}

literalReplay:
	if activeCmd == "" {
		plan.Reason = "no command captured for pane; opening default shell"
		return plan
	}

	if _, ok := r.registry[name]; !ok {
		plan.Reason = "command " + quoteForDisplay(name) + " not in replay registry; opening default shell"
		return plan
	}
	plan.Matched = name

	// Default: replay the captured command verbatim. Command keeps the
	// quote-safe sh -c form for spawn-with-command/dry-run callers; SendLine is
	// the raw line we type into an interactive shell (no wrapper needed).
	plan.Command = []string{"sh", "-c", "exec " + activeCmd}
	plan.SendLine = activeCmd
	plan.Reason = "replaying literal: " + activeCmd
	return plan
}

// shellJoin joins argv into a single shell line, quoting any token that contains
// whitespace or shell metacharacters. Used to turn the claude resume argv into a
// line we can type via send-text.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// shellQuote single-quotes s if it contains anything outside a safe set, so it
// survives being typed into an interactive shell verbatim.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, c := range s {
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlnum && c != '-' && c != '_' && c != '.' && c != '/' && c != ':' && c != '=' && c != ',' {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	// Wrap in single quotes, escaping embedded single quotes as '\''.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sessionLaunchers maps linked session providers to commands that can launch
// them. Sosuke is intentionally absent until it exposes lifecycle hooks and a
// stable provider-specific resume command.
var sessionLaunchers = map[string]map[string]struct{}{
	store.ProviderClaude: {
		"claude": {},
		"cst":    {},
	},
	store.ProviderCodex: {
		"codex": {},
		"cst":   {},
	},
}

func isSessionLauncher(provider, name string) bool {
	launchers, ok := sessionLaunchers[provider]
	if !ok {
		return false
	}
	_, ok = launchers[name]
	return ok
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
		if !isUpper && !isDigit && !isUnderscore {
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
