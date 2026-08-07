package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/snapshot"
	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

// Client-side timing constants. Exposed for tests.
var (
	DialTimeout         = 200 * time.Millisecond
	SpawnRetryWait      = 150 * time.Millisecond
	AutoSpawnDaemonFunc = SpawnDetached // override in tests
)

// PushEvent connects to the daemon socket, writes one event, and exits.
// If the socket isn't reachable, spawns a daemon and retries once. If that
// also fails, applies the event inline so we never silently lose data.
func PushEvent(ev Event) error {
	sock := SocketPath()

	conn, err := net.DialTimeout("unix", sock, DialTimeout)
	if err != nil {
		if err := AutoSpawnDaemonFunc(); err != nil {
			return inlineFallback(ev, fmt.Errorf("auto-spawn daemon: %w", err))
		}
		time.Sleep(SpawnRetryWait)
		conn, err = net.DialTimeout("unix", sock, DialTimeout)
		if err != nil {
			return inlineFallback(ev, fmt.Errorf("dial after spawn: %w", err))
		}
	}
	defer func() { _ = conn.Close() }()

	if err := WriteEvent(conn, ev); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// IsDaemonReachable returns true if a daemon is listening on the per-user socket.
func IsDaemonReachable() bool {
	sock := SocketPath()
	conn, err := net.DialTimeout("unix", sock, DialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// inlineFallback handles an event in-process when the daemon is unreachable.
// Used as a last-resort safety net so events are never silently dropped.
func inlineFallback(ev Event, dialErr error) error {
	w, err := store.OpenWez(store.DefaultWezDBPath())
	if err != nil {
		return fmt.Errorf("inline fallback (after %v): open wez: %w", dialErr, err)
	}
	defer func() { _ = w.Close() }()

	switch ev.Type {
	case EventSnapshotRequest, EventShutdown:
		raw, panes, err := wezterm.ListWithRaw()
		if err != nil {
			return fmt.Errorf("inline list: %w", err)
		}
		_, err = snapshot.Sync(w, raw, panes, wezterm.MuxSocket(), time.Now())
		return err

	case EventPreexec:
		if ev.MuxSocket == "" || ev.PaneID == 0 {
			return nil
		}
		return w.ApplyPreexec(ev.MuxSocket, ev.PaneID, ev.Command, ev.CWD, ev.Timestamp)

	case EventPrecmd:
		if ev.MuxSocket == "" || ev.PaneID == 0 {
			return nil
		}
		linked, linkedOK, err := w.LookupPaneRuntime(ev.MuxSocket, ev.PaneID)
		if err != nil {
			return err
		}
		if err := w.ApplyPrecmd(ev.MuxSocket, ev.PaneID, ev.CWD, ev.Timestamp); err != nil {
			return err
		}
		if !linkedOK || linked.SessionID == "" {
			return nil
		}
		sessions, err := store.Open(store.DefaultDBPath())
		if err != nil {
			return err
		}
		_, detachErr := sessions.DetachSession(
			linked.SessionID, linked.SessionProvider, ev.MuxSocket, ev.PaneID, ev.Timestamp,
		)
		closeErr := sessions.Close()
		if detachErr != nil {
			return detachErr
		}
		if closeErr != nil {
			return closeErr
		}
		return w.UnbindSession(ev.MuxSocket, ev.PaneID, linked.SessionProvider, linked.SessionID)

	case EventSessionStart:
		if ev.MuxSocket == "" || ev.PaneID == 0 || ev.SessionID == "" {
			return nil
		}
		return w.BindSession(ev.MuxSocket, ev.PaneID, ev.Provider, ev.SessionID, ev.CWD)

	case EventSessionEnd:
		if ev.MuxSocket == "" || ev.PaneID == 0 {
			return nil
		}
		return w.UnbindSession(ev.MuxSocket, ev.PaneID, ev.Provider, ev.SessionID)
	}
	return nil
}

// SpawnDetached forks a new `cst daemon` process, fully detached from the
// caller (so the caller can exit immediately and the daemon survives).
// Uses /proc/self/exe so we run the same cst binary the caller is running.
//
// Returns nil if the spawn submitted successfully (it can still die later).
// Returns error only if exec preparation failed before fork.
func SpawnDetached() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate cst executable: %w", err)
	}
	// Resolve symlinks in case the user installed cst via ln -s.
	if real, err := os.Readlink(exe); err == nil && real != "" {
		exe = real
	}

	logPath := LogPath()
	if err := os.MkdirAll(parentDir(logPath), 0o700); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	// We don't defer close — the spawned process inherits the fd.

	return spawnDetachedExec(exe, []string{"daemon"}, logFile)
}

func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// ErrDaemonNotRunning is returned by some helpers when we need a live daemon
// but couldn't reach one. Currently informational only.
var ErrDaemonNotRunning = errors.New("cst daemon not running")
