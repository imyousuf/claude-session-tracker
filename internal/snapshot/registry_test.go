package snapshot

import (
	"reflect"
	"testing"

	"github.com/imyousuf/claude-session-tracker/internal/store"
)

func TestResolveClaudeWithLinkedSession(t *testing.T) {
	r := NewResolver([]string{"claude", "tomoe"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:             "/proj",
		CurrentCmd:      "claude",
		ClaudeSessionID: "sess-abc",
	})
	want := []string{"claude", "--resume", "sess-abc"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want %v", plan.Command, want)
	}
	if plan.Matched != "claude" {
		t.Errorf("Matched = %q", plan.Matched)
	}
}

func TestResolveClaudeWithLinkedSessionAppendsClaudeArgs(t *testing.T) {
	r := NewResolver([]string{"claude"}, []string{"--dangerously-skip-permissions", "--foo"})
	plan := r.Resolve(store.WezPane{
		CWD:             "/proj",
		CurrentCmd:      "claude",
		ClaudeSessionID: "sess-abc",
	})
	want := []string{"claude", "--resume", "sess-abc", "--dangerously-skip-permissions", "--foo"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want %v", plan.Command, want)
	}
}

func TestResolveCodexWithLinkedSession(t *testing.T) {
	r := NewResolver([]string{"codex"}, []string{"--dangerously-skip-permissions"})
	plan := r.Resolve(store.WezPane{
		CWD:             "/proj",
		CurrentCmd:      "codex --model gpt-5.4",
		SessionProvider: store.ProviderCodex,
		SessionID:       "thr_123",
	})
	want := []string{"codex", "resume", "thr_123"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want %v", plan.Command, want)
	}
	if plan.SendLine != "codex resume thr_123" {
		t.Errorf("SendLine = %q", plan.SendLine)
	}
	if plan.Matched != "codex" {
		t.Errorf("Matched = %q, want codex", plan.Matched)
	}
}

func TestResolveCodexHookBeforePreexecResumesLinkedSession(t *testing.T) {
	r := NewResolver([]string{"codex"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:             "/proj",
		SessionProvider: store.ProviderCodex,
		SessionID:       "thr_early",
	})
	want := []string{"codex", "resume", "thr_early"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want %v", plan.Command, want)
	}
}

func TestResolveCstLauncherWithLinkedCodexSessionResumes(t *testing.T) {
	r := NewResolver([]string{"codex"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:             "/proj",
		CurrentCmd:      "cst",
		SessionProvider: store.ProviderCodex,
		SessionID:       "thr_via_cst",
	})
	want := []string{"codex", "resume", "thr_via_cst"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want %v", plan.Command, want)
	}
}

func TestSosukeDoesNotUseLinkedProviderResumeWithoutHooks(t *testing.T) {
	r := NewResolver([]string{"sosuke"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:             "/proj",
		CurrentCmd:      "sosuke --profile work",
		SessionProvider: store.ProviderCodex,
		SessionID:       "thr_stale",
	})
	want := []string{"sh", "-c", "exec sosuke --profile work"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want literal replay %v", plan.Command, want)
	}
}

func TestResolveCstLauncherWithLinkedSessionResumes(t *testing.T) {
	// A pane running Claude via the `cst` wrapper records current_cmd="cst".
	// `cst` is NOT in the replay registry, but a linked claude_session_id must
	// still resume the Claude session (the binding is stronger than the captured
	// command name).
	r := NewResolver([]string{"claude", "tomoe"}, []string{"--dangerously-skip-permissions"})
	plan := r.Resolve(store.WezPane{
		CWD:             "/proj",
		CurrentCmd:      "cst",
		LastCmd:         "wezterm cli split-pane --right",
		ClaudeSessionID: "sess-cst",
	})
	want := []string{"claude", "--resume", "sess-cst", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want %v", plan.Command, want)
	}
	if plan.Matched != "cst" {
		t.Errorf("Matched = %q, want cst", plan.Matched)
	}
}

func TestResolveCstLauncherWithoutSessionGetsPlainShell(t *testing.T) {
	// `cst` with no linked session is not a replayable command — plain shell.
	r := NewResolver([]string{"claude", "tomoe"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:        "/proj",
		CurrentCmd: "cst",
	})
	if len(plan.Command) != 0 {
		t.Errorf("cst without session should get plain shell, got %v", plan.Command)
	}
}

func TestResolveCstResumeBeatsRegistryGate(t *testing.T) {
	// Even with an empty registry, a cst-launched pane with a linked session
	// resumes Claude — the special case is checked before the registry gate.
	r := NewResolver(nil, nil)
	plan := r.Resolve(store.WezPane{
		CWD:             "/proj",
		CurrentCmd:      "cst",
		ClaudeSessionID: "sess-xyz",
	})
	want := []string{"claude", "--resume", "sess-xyz"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want %v", plan.Command, want)
	}
}

func TestResolveClaudeArgsOnlyApplyToResume(t *testing.T) {
	// The literal-replay fallback (no linked session) must NOT get claude args.
	r := NewResolver([]string{"claude"}, []string{"--dangerously-skip-permissions"})
	plan := r.Resolve(store.WezPane{
		CWD:        "/proj",
		CurrentCmd: "claude --some-flag",
	})
	want := []string{"sh", "-c", "exec claude --some-flag"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want %v", plan.Command, want)
	}
}

func TestResolveClaudeNoSessionFallsBackToLiteral(t *testing.T) {
	r := NewResolver([]string{"claude"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:        "/proj",
		CurrentCmd: "claude --some-flag",
	})
	want := []string{"sh", "-c", "exec claude --some-flag"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v, want %v", plan.Command, want)
	}
}

func TestResolveTomoeLiteralReplay(t *testing.T) {
	r := NewResolver([]string{"claude", "tomoe"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:        "/audio",
		CurrentCmd: "tomoe start --device hw:0,0",
	})
	want := []string{"sh", "-c", "exec tomoe start --device hw:0,0"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v", plan.Command)
	}
}

func TestResolveAgentCommandsLiteralReplay(t *testing.T) {
	r := NewResolver([]string{"codex", "sosuke"}, nil)
	tests := []struct {
		name string
		cmd  string
	}{
		{name: "codex", cmd: "codex --model gpt-5.4"},
		{name: "sosuke", cmd: "sosuke --profile work"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := r.Resolve(store.WezPane{CWD: "/proj", CurrentCmd: tc.cmd})
			want := []string{"sh", "-c", "exec " + tc.cmd}
			if !reflect.DeepEqual(plan.Command, want) {
				t.Errorf("command = %v, want %v", plan.Command, want)
			}
			if plan.SendLine != tc.cmd {
				t.Errorf("SendLine = %q, want %q", plan.SendLine, tc.cmd)
			}
			if plan.Matched != tc.name {
				t.Errorf("Matched = %q, want %q", plan.Matched, tc.name)
			}
		})
	}
}

func TestResolveUsesLastCmdWhenCurrentEmpty(t *testing.T) {
	r := NewResolver([]string{"claude"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:     "/proj",
		LastCmd: "claude --resume xyz",
	})
	want := []string{"sh", "-c", "exec claude --resume xyz"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v", plan.Command)
	}
}

func TestResolveUnknownCommandGetsPlainShell(t *testing.T) {
	r := NewResolver([]string{"claude", "tomoe"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:     "/proj",
		LastCmd: "vim foo.go",
	})
	if len(plan.Command) != 0 {
		t.Errorf("unknown should get plain shell, got %v", plan.Command)
	}
	if plan.CWD != "/proj" {
		t.Errorf("CWD should be preserved: %s", plan.CWD)
	}
}

func TestResolveNoCommandCaptured(t *testing.T) {
	r := NewResolver([]string{"claude"}, nil)
	plan := r.Resolve(store.WezPane{CWD: "/proj"})
	if len(plan.Command) != 0 {
		t.Errorf("no capture should get plain shell")
	}
}

func TestResolveStripsEnvPrefixes(t *testing.T) {
	r := NewResolver([]string{"tomoe"}, nil)
	plan := r.Resolve(store.WezPane{
		CWD:        "/audio",
		CurrentCmd: "FOO=1 BAR=baz tomoe start",
	})
	if plan.Matched != "tomoe" {
		t.Errorf("Matched = %q, want tomoe", plan.Matched)
	}
}

func TestFirstTokenSkippingEnv(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"claude", "claude"},
		{"tomoe start --device hw:0,0", "tomoe"},
		{"FOO=bar claude", "claude"},
		{"FOO=1 BAR=baz tomoe start", "tomoe"},
		{"lowercase=value something", "lowercase=value"}, // not env-like
		{"=invalid first", "=invalid"},
		{"", ""},
	}
	for _, c := range cases {
		if got := firstTokenSkippingEnv(c.in); got != c.want {
			t.Errorf("firstTokenSkippingEnv(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsLikelyEnvKey(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"FOO", true},
		{"FOO_BAR", true},
		{"_PRIVATE", true},
		{"FOO123", true},
		{"foo", false},
		{"Foo", false},
		{"123FOO", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isLikelyEnvKey(c.in); got != c.want {
			t.Errorf("isLikelyEnvKey(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- SendLine (Fix B) ---

func TestResolveSendLineClaude(t *testing.T) {
	r := NewResolver([]string{"claude"}, []string{"--dangerously-skip-permissions"})
	plan := r.Resolve(store.WezPane{CWD: "/p", CurrentCmd: "cst", ClaudeSessionID: "abc"})
	want := "claude --resume abc --dangerously-skip-permissions"
	if plan.SendLine != want {
		t.Errorf("SendLine = %q, want %q", plan.SendLine, want)
	}
}

func TestResolveSendLineLiteralVerbatim(t *testing.T) {
	// Literal replay sends the captured line verbatim — no sh -c "exec" wrapper.
	r := NewResolver([]string{"tomoe"}, nil)
	plan := r.Resolve(store.WezPane{CWD: "/a", CurrentCmd: "tomoe start --device hw:0,0"})
	if plan.SendLine != "tomoe start --device hw:0,0" {
		t.Errorf("SendLine = %q, want verbatim", plan.SendLine)
	}
}

func TestResolveSendLineEmptyForPlainShell(t *testing.T) {
	r := NewResolver([]string{"claude"}, nil)
	// not in registry → plain shell → no SendLine
	plan := r.Resolve(store.WezPane{CWD: "/p", CurrentCmd: "vim foo.go"})
	if plan.SendLine != "" {
		t.Errorf("SendLine = %q, want empty (plain shell)", plan.SendLine)
	}
	// no command captured → plain shell → no SendLine
	plan = r.Resolve(store.WezPane{CWD: "/p"})
	if plan.SendLine != "" {
		t.Errorf("SendLine = %q, want empty (no capture)", plan.SendLine)
	}
}

func TestShellJoin(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"claude", "--resume", "abc"}, "claude --resume abc"},
		{[]string{"a b", "c"}, "'a b' c"},
		{[]string{"it's"}, `'it'\''s'`},
		{[]string{""}, "''"},
		{[]string{"--flag=x,y", "/p/q"}, "--flag=x,y /p/q"},
	}
	for _, c := range cases {
		if got := shellJoin(c.in); got != c.want {
			t.Errorf("shellJoin(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
