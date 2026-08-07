package hook

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/imyousuf/claude-session-tracker/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestHandleSessionStartNew(t *testing.T) {
	s := testStore(t)

	input := HookInput{
		SessionID:     "sess-1",
		CWD:           "/home/user/project",
		HookEventName: "SessionStart",
		Source:        "startup",
		Model:         "claude-sonnet-4-6",
	}

	if err := HandleSessionStart(s, input); err != nil {
		t.Fatalf("HandleSessionStart: %v", err)
	}

	sessions, err := s.ListByProject("/home/user/project")
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].ID != "sess-1" {
		t.Errorf("ID = %q, want %q", sessions[0].ID, "sess-1")
	}
	if !sessions[0].Active {
		t.Error("session should be active")
	}
	if sessions[0].Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want %q", sessions[0].Model, "claude-sonnet-4-6")
	}
}

func TestHandleCodexSessionStartRecordsProvider(t *testing.T) {
	s := testStore(t)
	input := HookInput{
		SessionID:     "thr_codex_123",
		Provider:      store.ProviderCodex,
		AgentPID:      4242,
		MuxSocket:     "/run/mux",
		PaneID:        18,
		CWD:           "/home/user/project",
		HookEventName: "SessionStart",
		Source:        "startup",
		Model:         "gpt-5.4",
	}
	if err := HandleSessionStart(s, input); err != nil {
		t.Fatalf("HandleSessionStart: %v", err)
	}
	sessions, err := s.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if sessions[0].Provider != store.ProviderCodex {
		t.Errorf("provider = %q, want codex", sessions[0].Provider)
	}
	if sessions[0].ID != "thr_codex_123" {
		t.Errorf("id = %q", sessions[0].ID)
	}
	if sessions[0].PID == nil || *sessions[0].PID != 4242 {
		t.Errorf("Codex frontend PID = %v, want 4242", sessions[0].PID)
	}
	if sessions[0].ActiveMuxSocket != "/run/mux" || sessions[0].ActivePaneID == nil || *sessions[0].ActivePaneID != 18 {
		t.Errorf("Codex attachment = %q/%v", sessions[0].ActiveMuxSocket, sessions[0].ActivePaneID)
	}
}

func TestHandleSessionStartResume(t *testing.T) {
	s := testStore(t)

	// Create initial session
	input := HookInput{
		SessionID: "sess-1", CWD: "/proj",
		HookEventName: "SessionStart", Source: "startup",
		Model: "sonnet",
	}
	if err := HandleSessionStart(s, input); err != nil {
		t.Fatalf("HandleSessionStart: %v", err)
	}

	// Deactivate it
	if err := s.Deactivate("sess-1"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	// Resume
	input.Source = "resume"
	input.Model = "opus"
	if err := HandleSessionStart(s, input); err != nil {
		t.Fatalf("HandleSessionStart resume: %v", err)
	}

	sessions, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if !sessions[0].Active {
		t.Error("session should be active after resume")
	}
	if sessions[0].Model != "opus" {
		t.Errorf("Model = %q, want %q", sessions[0].Model, "opus")
	}
}

func TestHandlePrompt(t *testing.T) {
	s := testStore(t)

	// Create session first
	if err := HandleSessionStart(s, HookInput{
		SessionID: "sess-1", CWD: "/proj",
		HookEventName: "SessionStart", Source: "startup", Model: "sonnet",
	}); err != nil {
		t.Fatalf("HandleSessionStart: %v", err)
	}

	// Submit a prompt
	if err := HandlePrompt(s, HookInput{
		SessionID: "sess-1", CWD: "/proj",
		HookEventName: "UserPromptSubmit", Prompt: "fix the bug",
	}); err != nil {
		t.Fatalf("HandlePrompt: %v", err)
	}

	prompts, err := s.GetPrompts("sess-1", 10)
	if err != nil {
		t.Fatalf("GetPrompts: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(prompts))
	}
	if prompts[0].Text != "fix the bug" {
		t.Errorf("prompt = %q, want %q", prompts[0].Text, "fix the bug")
	}
}

func TestHandlePromptSkipsSlashCommands(t *testing.T) {
	s := testStore(t)

	if err := HandleSessionStart(s, HookInput{
		SessionID: "sess-1", CWD: "/proj",
		HookEventName: "SessionStart", Source: "startup", Model: "sonnet",
	}); err != nil {
		t.Fatalf("HandleSessionStart: %v", err)
	}

	for _, cmd := range []string{"/exit", "/model", "/compact", "/help"} {
		if err := HandlePrompt(s, HookInput{
			SessionID: "sess-1", CWD: "/proj",
			HookEventName: "UserPromptSubmit", Prompt: cmd,
		}); err != nil {
			t.Fatalf("HandlePrompt %q: %v", cmd, err)
		}
	}

	prompts, err := s.GetPrompts("sess-1", 10)
	if err != nil {
		t.Fatalf("GetPrompts: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("expected 0 prompts (all slash commands), got %d", len(prompts))
	}
}

func TestHandlePromptSkipsEmpty(t *testing.T) {
	s := testStore(t)

	if err := HandleSessionStart(s, HookInput{
		SessionID: "sess-1", CWD: "/proj",
		HookEventName: "SessionStart", Source: "startup", Model: "sonnet",
	}); err != nil {
		t.Fatalf("HandleSessionStart: %v", err)
	}

	for _, p := range []string{"", "   ", "\t\n"} {
		if err := HandlePrompt(s, HookInput{
			SessionID: "sess-1", CWD: "/proj",
			HookEventName: "UserPromptSubmit", Prompt: p,
		}); err != nil {
			t.Fatalf("HandlePrompt empty: %v", err)
		}
	}

	prompts, err := s.GetPrompts("sess-1", 10)
	if err != nil {
		t.Fatalf("GetPrompts: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("expected 0 prompts (all empty), got %d", len(prompts))
	}
}

func TestHandlePromptTruncatesLong(t *testing.T) {
	s := testStore(t)

	if err := HandleSessionStart(s, HookInput{
		SessionID: "sess-1", CWD: "/proj",
		HookEventName: "SessionStart", Source: "startup", Model: "sonnet",
	}); err != nil {
		t.Fatalf("HandleSessionStart: %v", err)
	}

	longPrompt := strings.Repeat("a", 300)
	if err := HandlePrompt(s, HookInput{
		SessionID: "sess-1", CWD: "/proj",
		HookEventName: "UserPromptSubmit", Prompt: longPrompt,
	}); err != nil {
		t.Fatalf("HandlePrompt: %v", err)
	}

	prompts, err := s.GetPrompts("sess-1", 10)
	if err != nil {
		t.Fatalf("GetPrompts: %v", err)
	}
	if len(prompts[0].Text) != maxPromptLen {
		t.Errorf("prompt length = %d, want %d", len(prompts[0].Text), maxPromptLen)
	}
	if !strings.HasSuffix(prompts[0].Text, "...") {
		t.Error("truncated prompt should end with ...")
	}
}

func TestHandleSessionEnd(t *testing.T) {
	s := testStore(t)

	if err := HandleSessionStart(s, HookInput{
		SessionID: "sess-1", CWD: "/proj",
		HookEventName: "SessionStart", Source: "startup", Model: "sonnet",
	}); err != nil {
		t.Fatalf("HandleSessionStart: %v", err)
	}

	if err := HandleSessionEnd(s, HookInput{
		SessionID: "sess-1", HookEventName: "SessionEnd", Reason: "other",
	}); err != nil {
		t.Fatalf("HandleSessionEnd: %v", err)
	}

	sessions, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if !sessions[0].Active {
		t.Error("SessionEnd should not change client attachment state")
	}
	if sessions[0].LifecycleEndedAt == nil {
		t.Error("SessionEnd should record lifecycle-ended timestamp")
	}
}

func TestStaleSessionEndDoesNotClearResumedAttachment(t *testing.T) {
	s := testStore(t)

	if err := HandleSessionStart(s, HookInput{
		SessionID: "codex-1", Provider: store.ProviderCodex, CWD: "/proj",
		HookEventName: "SessionStart", MuxSocket: "/run/old", PaneID: 18,
	}); err != nil {
		t.Fatalf("old HandleSessionStart: %v", err)
	}
	if err := HandleSessionStart(s, HookInput{
		SessionID: "codex-1", Provider: store.ProviderCodex, CWD: "/proj",
		HookEventName: "SessionStart", MuxSocket: "/run/new", PaneID: 22,
	}); err != nil {
		t.Fatalf("resumed HandleSessionStart: %v", err)
	}

	if err := HandleSessionEnd(s, HookInput{
		SessionID: "codex-1", Provider: store.ProviderCodex,
		HookEventName: "SessionEnd", MuxSocket: "/run/old", PaneID: 18,
	}); err != nil {
		t.Fatalf("stale HandleSessionEnd: %v", err)
	}

	sessions, err := s.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	got := sessions[0]
	if !got.Active || got.ActivePaneID == nil || *got.ActivePaneID != 22 {
		t.Fatalf("stale SessionEnd changed resumed attachment: %+v", got)
	}
	if got.LifecycleEndedAt == nil {
		t.Fatalf("SessionEnd did not record the separate lifecycle event: %+v", got)
	}
}

func TestReadInput(t *testing.T) {
	json := `{"session_id":"abc","cwd":"/proj","hook_event_name":"SessionStart","source":"startup","model":"sonnet"}`
	input, err := ReadInput(strings.NewReader(json))
	if err != nil {
		t.Fatalf("ReadInput: %v", err)
	}
	if input.SessionID != "abc" {
		t.Errorf("SessionID = %q, want %q", input.SessionID, "abc")
	}
	if input.Source != "startup" {
		t.Errorf("Source = %q, want %q", input.Source, "startup")
	}
}
