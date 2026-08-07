package hook

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/procutil"
	"github.com/imyousuf/claude-session-tracker/internal/store"
)

// HookInput represents the JSON payload sent to hook commands via stdin.
type HookInput struct {
	SessionID      string `json:"session_id"`
	Provider       string `json:"-"`
	AgentPID       int    `json:"-"`
	MuxSocket      string `json:"-"`
	PaneID         int64  `json:"-"`
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
	provider := store.NormalizeProvider(input.Provider)
	var pid *int
	if input.AgentPID > 0 {
		trackedPID := input.AgentPID
		pid = &trackedPID
	} else if provider != store.ProviderCodex {
		trackedPID := procutil.FindAgentPID(provider)
		pid = &trackedPID
	}

	// Try to activate an existing session first
	err := s.ActivateAttached(input.SessionID, provider, pid, input.Model, input.CWD, input.MuxSocket, input.PaneID)
	if errors.Is(err, sql.ErrNoRows) {
		// Session doesn't exist yet — create it
		sess := store.Session{
			ID:              input.SessionID,
			Provider:        provider,
			Project:         input.CWD,
			CWD:             input.CWD,
			StartedAt:       now,
			LastActivity:    now,
			PID:             pid,
			Active:          true,
			Model:           input.Model,
			ActiveMuxSocket: input.MuxSocket,
		}
		if input.PaneID != 0 {
			paneID := input.PaneID
			sess.ActivePaneID = &paneID
		}
		if err := s.UpsertSession(sess); err != nil {
			return fmt.Errorf("upsert session: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("activate session: %w", err)
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

// HandleSessionEnd records the provider lifecycle end. The session row remains
// available to resume; foreground CLI detachment is tracked separately.
func HandleSessionEnd(s *store.Store, input HookInput) error {
	if err := s.RecordLifecycleEnd(input.SessionID, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	return nil
}
