// Package daemon implements the cst snapshot daemon.
//
// The daemon listens on a per-user Unix socket and accepts events from observers
// (shell hooks, wezterm Lua callbacks, cst's own SessionStart/End hooks).
// Snapshot requests are coalesced into a single `wezterm cli list` call per
// ~100ms window; preexec/precmd/session events are applied immediately to
// pane_runtime_state.
package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// EventType is the discriminator for daemon protocol events.
type EventType string

const (
	// EventSnapshotRequest asks the daemon to schedule a snapshot.
	// Coalesced into a single snapshot per ~100ms window.
	EventSnapshotRequest EventType = "snapshot_request"

	// EventPreexec — bash-preexec preexec hook fired. Updates pane_runtime_state
	// with the command about to run.
	EventPreexec EventType = "preexec"

	// EventPrecmd — bash-preexec precmd hook fired. Clears current_command,
	// promotes it to last_command.
	EventPrecmd EventType = "precmd"

	// EventSessionStart — a coding-agent SessionStart hook fired. Binds the pane
	// to the provider/session ID.
	EventSessionStart EventType = "session_start"

	// EventSessionEnd — a coding-agent SessionEnd hook fired. Unbinds the pane.
	EventSessionEnd EventType = "session_end"

	// EventShutdown — graceful shutdown request (used internally and by SIGTERM).
	EventShutdown EventType = "shutdown"
)

// Event is the single message type sent over the daemon socket.
// Fields are optional; only those relevant to the event type are populated.
type Event struct {
	Type EventType `json:"type"`

	// Pane scoping (preexec, precmd, session_start, session_end).
	MuxSocket string `json:"mux_socket,omitempty"`
	PaneID    int64  `json:"pane_id,omitempty"`

	// preexec.
	Command string `json:"command,omitempty"`

	// preexec, precmd.
	CWD       string `json:"cwd,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"` // ms epoch

	// session_start, session_end.
	Provider  string `json:"provider,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	PID       int    `json:"pid,omitempty"`
}

// WriteEvent writes a single event as NDJSON (one line of JSON + '\n').
func WriteEvent(w io.Writer, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// ReadEvents returns a function that reads one Event per call, returning
// io.EOF when the input stream is closed.
func ReadEvents(r io.Reader) func() (Event, error) {
	scanner := bufio.NewScanner(r)
	// Allow large-ish events (1 MB buffer; events are usually <1 KB).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	return func() (Event, error) {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return Event{}, err
			}
			return Event{}, io.EOF
		}
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return Event{}, fmt.Errorf("parse event: %w", err)
		}
		return ev, nil
	}
}
