package snapshot

import (
	"reflect"
	"testing"

	"github.com/imyousuf/claude-session-tracker/internal/store"
)

func TestResolveClaudeWithLinkedSession(t *testing.T) {
	r := NewResolver([]string{"claude", "tomoe"})
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

func TestResolveClaudeNoSessionFallsBackToLiteral(t *testing.T) {
	r := NewResolver([]string{"claude"})
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
	r := NewResolver([]string{"claude", "tomoe"})
	plan := r.Resolve(store.WezPane{
		CWD:        "/audio",
		CurrentCmd: "tomoe start --device hw:0,0",
	})
	want := []string{"sh", "-c", "exec tomoe start --device hw:0,0"}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Errorf("command = %v", plan.Command)
	}
}

func TestResolveUsesLastCmdWhenCurrentEmpty(t *testing.T) {
	r := NewResolver([]string{"claude"})
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
	r := NewResolver([]string{"claude", "tomoe"})
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
	r := NewResolver([]string{"claude"})
	plan := r.Resolve(store.WezPane{CWD: "/proj"})
	if len(plan.Command) != 0 {
		t.Errorf("no capture should get plain shell")
	}
}

func TestResolveStripsEnvPrefixes(t *testing.T) {
	r := NewResolver([]string{"tomoe"})
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
