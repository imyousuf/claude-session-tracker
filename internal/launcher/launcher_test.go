package launcher

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/imyousuf/claude-session-tracker/internal/store"
)

func TestEnterRechecksStaleActiveStateBeforeBlockingResume(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.UpsertSession(store.Session{
		ID: "thr_123", Provider: store.ProviderCodex, Project: "/proj", CWD: "/proj",
		StartedAt: 1, LastActivity: 2, Active: false,
	}); err != nil {
		t.Fatal(err)
	}

	m := New(s, "/proj", true)
	// Simulate a precmd event landing after the TUI loaded its first copy.
	m.sessions = []store.Session{{
		ID: "thr_123", Provider: store.ProviderCodex, Project: "/proj", Active: true,
	}}
	m.filtered = []int{0}
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.result == nil || got.result.SessionID != "thr_123" {
		t.Fatalf("stale ACTIVE row still blocked resume: result=%+v status=%q", got.result, got.statusMsg)
	}
}
