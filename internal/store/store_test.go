package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub", "dir", "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := os.Stat(filepath.Join(dir, "sub", "dir")); err != nil {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestUpsertAndListSession(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()
	pid := 12345

	sess := Session{
		ID:           "test-session-1",
		Project:      "/home/user/project",
		CWD:          "/home/user/project",
		StartedAt:    now,
		LastActivity: now,
		PID:          &pid,
		Active:       true,
		Model:        "claude-sonnet-4-6",
	}

	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	sessions, err := s.ListByProject("/home/user/project")
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	got := sessions[0]
	if got.ID != "test-session-1" {
		t.Errorf("ID = %q, want %q", got.ID, "test-session-1")
	}
	if got.Project != "/home/user/project" {
		t.Errorf("Project = %q, want %q", got.Project, "/home/user/project")
	}
	if !got.Active {
		t.Error("Active = false, want true")
	}
	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want %q", got.Model, "claude-sonnet-4-6")
	}
}

func TestUpsertUpdatesExisting(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()
	pid := 100

	sess := Session{
		ID: "s1", Project: "/proj", CWD: "/proj",
		StartedAt: now, LastActivity: now, PID: &pid,
		Active: true, Model: "sonnet",
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	// Update with new model and cwd
	pid2 := 200
	sess.PID = &pid2
	sess.Model = "opus"
	sess.CWD = "/proj/sub"
	sess.LastActivity = now + 1000
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession update: %v", err)
	}

	sessions, err := s.ListByProject("/proj")
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Model != "opus" {
		t.Errorf("Model = %q, want %q", sessions[0].Model, "opus")
	}
	if *sessions[0].PID != 200 {
		t.Errorf("PID = %d, want 200", *sessions[0].PID)
	}
}

func TestActivateAndDeactivate(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()

	sess := Session{
		ID: "s1", Project: "/proj", CWD: "/proj",
		StartedAt: now, LastActivity: now,
		Active: false, Model: "sonnet",
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	if err := s.Activate("s1", 999, "opus", "/proj/new"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	sessions, err := s.ListByProject("/proj")
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if !sessions[0].Active {
		t.Error("expected active after Activate")
	}
	if *sessions[0].PID != 999 {
		t.Errorf("PID = %d, want 999", *sessions[0].PID)
	}

	if err := s.Deactivate("s1"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	sessions, err = s.ListByProject("/proj")
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if sessions[0].Active {
		t.Error("expected inactive after Deactivate")
	}
	if sessions[0].PID != nil {
		t.Errorf("PID should be nil after Deactivate, got %v", sessions[0].PID)
	}
}

func TestActivateNonExistent(t *testing.T) {
	s := testStore(t)
	err := s.Activate("nonexistent", 123, "sonnet", "/proj")
	if err == nil {
		t.Fatal("expected error for non-existent session")
	}
}

func TestAddPromptAndGetPrompts(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()

	sess := Session{
		ID: "s1", Project: "/proj", CWD: "/proj",
		StartedAt: now, LastActivity: now, Model: "sonnet",
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	for i := 0; i < 15; i++ {
		ts := now + int64(i)*1000
		prompt := "prompt " + string(rune('A'+i))
		if err := s.AddPrompt("s1", prompt, ts); err != nil {
			t.Fatalf("AddPrompt %d: %v", i, err)
		}
	}

	prompts, err := s.GetPrompts("s1", 20)
	if err != nil {
		t.Fatalf("GetPrompts: %v", err)
	}

	// Should be capped at DefaultMaxPrompt (10)
	if len(prompts) != DefaultMaxPrompt {
		t.Fatalf("expected %d prompts, got %d", DefaultMaxPrompt, len(prompts))
	}

	// Most recent first
	if prompts[0].Text != "prompt O" {
		t.Errorf("newest prompt = %q, want %q", prompts[0].Text, "prompt O")
	}
}

func TestListIncludesLatestPrompt(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()

	sess := Session{
		ID: "s1", Project: "/proj", CWD: "/proj",
		StartedAt: now, LastActivity: now, Model: "sonnet",
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	if err := s.AddPrompt("s1", "first prompt", now); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}
	if err := s.AddPrompt("s1", "second prompt", now+1000); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}

	sessions, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].LastPrompt != "second prompt" {
		t.Errorf("LastPrompt = %q, want %q", sessions[0].LastPrompt, "second prompt")
	}
}

func TestDeleteSession(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()

	sess := Session{
		ID: "s1", Project: "/proj", CWD: "/proj",
		StartedAt: now, LastActivity: now, Model: "sonnet",
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := s.AddPrompt("s1", "hello", now); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}

	if err := s.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	sessions, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions after delete, got %d", len(sessions))
	}

	// Prompts should be cascade-deleted
	prompts, err := s.GetPrompts("s1", 10)
	if err != nil {
		t.Fatalf("GetPrompts: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("expected 0 prompts after cascade delete, got %d", len(prompts))
	}
}

func TestCleanup(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()
	old := now - 31*24*60*60*1000 // 31 days ago

	for _, tc := range []struct {
		id     string
		active bool
		ts     int64
	}{
		{"old-inactive", false, old},
		{"old-active", true, old},
		{"new-inactive", false, now},
		{"new-active", true, now},
	} {
		active := 0
		if tc.active {
			active = 1
		}
		sess := Session{
			ID: tc.id, Project: "/proj", CWD: "/proj",
			StartedAt: tc.ts, LastActivity: tc.ts,
			Active: active != 0, Model: "sonnet",
		}
		if err := s.UpsertSession(sess); err != nil {
			t.Fatalf("UpsertSession %s: %v", tc.id, err)
		}
	}

	removed, err := s.Cleanup(30)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	sessions, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions remaining, got %d", len(sessions))
	}
}

func TestEnforceCap(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()

	// Insert 5 sessions
	for i := 0; i < 5; i++ {
		sess := Session{
			ID: "s" + string(rune('0'+i)), Project: "/proj", CWD: "/proj",
			StartedAt: now + int64(i)*1000, LastActivity: now + int64(i)*1000,
			Model: "sonnet",
		}
		if err := s.UpsertSession(sess); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	}

	if err := s.EnforceCap(3); err != nil {
		t.Fatalf("EnforceCap: %v", err)
	}

	sessions, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions after cap enforcement, got %d", len(sessions))
	}

	// Should keep the 3 most recent
	if sessions[0].ID != "s4" {
		t.Errorf("most recent session = %q, want %q", sessions[0].ID, "s4")
	}
}

func TestRefreshActive(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()
	pid := 12345

	sess := Session{
		ID: "s1", Project: "/proj", CWD: "/proj",
		StartedAt: now, LastActivity: now,
		PID: &pid, Active: true, Model: "sonnet",
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	// Simulate dead process
	if err := s.RefreshActive(func(pid int) bool { return false }); err != nil {
		t.Fatalf("RefreshActive: %v", err)
	}

	sessions, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if sessions[0].Active {
		t.Error("session should be inactive after RefreshActive with dead PID")
	}
}

func TestUpdateActivity(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()

	sess := Session{
		ID: "s1", Project: "/proj", CWD: "/proj",
		StartedAt: now, LastActivity: now, Model: "sonnet",
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	later := now + 5000
	if err := s.UpdateActivity("s1", "/proj/sub", later); err != nil {
		t.Fatalf("UpdateActivity: %v", err)
	}

	sessions, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if sessions[0].LastActivity != later {
		t.Errorf("LastActivity = %d, want %d", sessions[0].LastActivity, later)
	}
	if sessions[0].CWD != "/proj/sub" {
		t.Errorf("CWD = %q, want %q", sessions[0].CWD, "/proj/sub")
	}
}
