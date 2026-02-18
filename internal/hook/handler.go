package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/store"
)

// HookInput represents the JSON payload sent to hook commands via stdin.
type HookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source,omitempty"`
	Model          string `json:"model,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

const maxPromptLen = 200

// ReadInput reads and parses the hook input JSON from the given reader.
func ReadInput(r io.Reader) (HookInput, error) {
	var input HookInput
	if err := json.NewDecoder(r).Decode(&input); err != nil {
		return input, fmt.Errorf("decode hook input: %w", err)
	}
	return input, nil
}

// HandleSessionStart processes a SessionStart hook event.
// It creates or activates the session in the store.
func HandleSessionStart(s *store.Store, input HookInput) error {
	now := time.Now().UnixMilli()
	pid := os.Getppid()

	// Try to activate an existing session first
	err := s.Activate(input.SessionID, pid, input.Model, input.CWD)
	if err != nil {
		// Session doesn't exist yet — create it
		sess := store.Session{
			ID:           input.SessionID,
			Project:      input.CWD,
			CWD:          input.CWD,
			StartedAt:    now,
			LastActivity: now,
			PID:          &pid,
			Active:       true,
			Model:        input.Model,
		}
		if err := s.UpsertSession(sess); err != nil {
			return fmt.Errorf("upsert session: %w", err)
		}
	}

	// Enforce session cap
	if err := s.EnforceCap(store.DefaultMaxCap); err != nil {
		return fmt.Errorf("enforce cap: %w", err)
	}

	return nil
}

// HandlePrompt processes a UserPromptSubmit hook event.
// It records the user's prompt and updates the session's last activity.
func HandlePrompt(s *store.Store, input HookInput) error {
	prompt := strings.TrimSpace(input.Prompt)

	// Skip slash commands and empty prompts
	if prompt == "" || strings.HasPrefix(prompt, "/") {
		return nil
	}

	// Truncate long prompts
	if len(prompt) > maxPromptLen {
		prompt = prompt[:maxPromptLen-3] + "..."
	}

	now := time.Now().UnixMilli()

	if err := s.AddPrompt(input.SessionID, prompt, now); err != nil {
		return fmt.Errorf("add prompt: %w", err)
	}

	if err := s.UpdateActivity(input.SessionID, input.CWD, now); err != nil {
		return fmt.Errorf("update activity: %w", err)
	}

	return nil
}

// HandleSessionEnd processes a SessionEnd hook event.
// It marks the session as inactive.
func HandleSessionEnd(s *store.Store, input HookInput) error {
	if err := s.Deactivate(input.SessionID); err != nil {
		return fmt.Errorf("deactivate session: %w", err)
	}
	return nil
}
