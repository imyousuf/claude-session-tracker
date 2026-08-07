package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/imyousuf/claude-session-tracker/internal/snapshot"
	"github.com/imyousuf/claude-session-tracker/internal/store"
	"github.com/imyousuf/claude-session-tracker/internal/wezterm"
)

// Default tuning constants. Exposed for tests.
const (
	DefaultCoalesceWindow = 100 * time.Millisecond
	DefaultIdleTimeout    = 30 * time.Minute
)

// Options configures a Daemon.
type Options struct {
	SocketPath     string
	WezStorePath   string
	SessionsDBPath string // optional; enables provider reconciliation and attachment updates
	CoalesceWindow time.Duration
	IdleTimeout    time.Duration // zero disables idle exit (used under systemd)
	Logger         *log.Logger
}

// Daemon owns the wezterm.db connection and serves the per-user event socket.
type Daemon struct {
	opts     Options
	w        *store.WezStore
	sessions *store.Store
	log      *log.Logger

	// muxSocketMu guards lastMuxSocket — set from incoming events so that
	// snapshot runs can derive the mux even when the daemon's own env doesn't
	// have $WEZTERM_UNIX_SOCKET (e.g. when started by systemd-user).
	muxSocketMu   sync.Mutex
	lastMuxSocket string
}

// New creates a Daemon with sensible defaults filled in.
func New(opts Options) (*Daemon, error) {
	if opts.SocketPath == "" {
		opts.SocketPath = SocketPath()
	}
	if opts.WezStorePath == "" {
		opts.WezStorePath = store.DefaultWezDBPath()
	}
	if opts.CoalesceWindow <= 0 {
		opts.CoalesceWindow = DefaultCoalesceWindow
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "[cst-daemon] ", log.LstdFlags|log.Lmicroseconds)
	}

	w, err := store.OpenWez(opts.WezStorePath)
	if err != nil {
		return nil, fmt.Errorf("open wezterm.db: %w", err)
	}
	var sessions *store.Store
	if opts.SessionsDBPath != "" {
		sessions, err = store.Open(opts.SessionsDBPath)
		if err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("open sessions.db: %w", err)
		}
		if err := w.AttachSessionsDB(opts.SessionsDBPath); err != nil {
			opts.Logger.Printf("warn: AttachSessionsDB failed: %v", err)
		}
	}
	return &Daemon{opts: opts, w: w, sessions: sessions, log: opts.Logger}, nil
}

// Close releases the WezStore connection.
func (d *Daemon) Close() error {
	var errs []error
	if d.sessions != nil {
		errs = append(errs, d.sessions.Close())
	}
	errs = append(errs, d.w.Close())
	return errors.Join(errs...)
}

// Serve binds the socket and runs the event loop until ctx is canceled,
// SIGTERM/SIGINT arrives, or the idle timeout fires.
//
// Refuses to start if the socket is already bound — defense against a second
// daemon clobbering the live one. Caller is responsible for shipping a PID
// file (use WritePIDFile from socket.go).
func (d *Daemon) Serve(ctx context.Context) error {
	if err := CheckSocketDirOwnership(d.opts.SocketPath); err != nil {
		return err
	}
	// Remove any stale socket file; if a live daemon owns it, bind will fail.
	_ = os.Remove(d.opts.SocketPath)

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "unix", d.opts.SocketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.opts.SocketPath, err)
	}
	defer func() { _ = listener.Close() }()
	// Tighten perms so only the owning user can connect.
	_ = os.Chmod(d.opts.SocketPath, 0o600)
	d.log.Printf("daemon listening on %s", d.opts.SocketPath)

	events := make(chan Event, 256)
	var connWg sync.WaitGroup

	// Accept loop.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				d.log.Printf("accept: %v", err)
				continue
			}
			connWg.Add(1)
			go d.handleConn(conn, events, &connWg)
		}
	}()

	return d.eventLoop(ctx, events, &connWg)
}

// handleConn reads NDJSON events from one client connection until EOF.
func (d *Daemon) handleConn(conn net.Conn, events chan<- Event, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() { _ = conn.Close() }()
	next := ReadEvents(conn)
	for {
		ev, err := next()
		if err == io.EOF {
			return
		}
		if err != nil {
			d.log.Printf("read event: %v", err)
			return
		}
		select {
		case events <- ev:
		default:
			// Channel full — drop and warn (extremely unlikely; buffer is 256).
			d.log.Printf("event channel full; dropping %s", ev.Type)
		}
	}
}

// eventLoop is the core daemon goroutine. Coalesces snapshot requests;
// applies preexec/precmd/session events immediately.
func (d *Daemon) eventLoop(ctx context.Context, events <-chan Event, wg *sync.WaitGroup) error {
	coalesce := time.NewTimer(time.Hour)
	coalesce.Stop()
	snapshotPending := false

	var idle *time.Timer
	var idleC <-chan time.Time
	if d.opts.IdleTimeout > 0 {
		idle = time.NewTimer(d.opts.IdleTimeout)
		idleC = idle.C
		defer idle.Stop()
	}

	resetIdle := func() {
		if idle == nil {
			return
		}
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(d.opts.IdleTimeout)
	}

	for {
		select {
		case <-ctx.Done():
			d.log.Printf("context done; draining")
			wg.Wait()
			return nil

		case <-idleC:
			d.log.Printf("idle timeout (%s); exiting", d.opts.IdleTimeout)
			return nil

		case ev := <-events:
			resetIdle()
			d.dispatch(ev, &snapshotPending, coalesce)

		case <-coalesce.C:
			if snapshotPending {
				snapshotPending = false
				d.runSnapshot()
			}
		}
	}
}

func (d *Daemon) dispatch(ev Event, pending *bool, coalesce *time.Timer) {
	switch ev.Type {
	case EventSnapshotRequest, EventSessionStart, EventSessionEnd:
		// Schedule a snapshot via the coalesce window.
		d.scheduleSnapshot(pending, coalesce)
		// SessionStart/End also have their own immediate state writes:
		if ev.Type == EventSessionStart {
			d.applySessionStart(ev)
		}
		if ev.Type == EventSessionEnd {
			d.applySessionEnd(ev)
		}

	case EventPreexec:
		d.applyPreexec(ev)

	case EventPrecmd:
		if d.applyPrecmd(ev) {
			d.scheduleSnapshot(pending, coalesce)
		}

	case EventShutdown:
		// Best-effort: trigger a final snapshot synchronously, then exit.
		if *pending {
			*pending = false
			coalesce.Stop()
		}
		d.runSnapshot()

	default:
		d.log.Printf("unknown event type: %q", ev.Type)
	}
}

// rememberMuxSocket caches the mux socket from any incoming event, so that
// later snapshots can use it even if the daemon's own env doesn't carry
// $WEZTERM_UNIX_SOCKET.
func (d *Daemon) rememberMuxSocket(sock string) {
	if sock == "" {
		return
	}
	d.muxSocketMu.Lock()
	d.lastMuxSocket = sock
	d.muxSocketMu.Unlock()
}

func (d *Daemon) cachedMuxSocket() string {
	d.muxSocketMu.Lock()
	defer d.muxSocketMu.Unlock()
	return d.lastMuxSocket
}

func (d *Daemon) applyPreexec(ev Event) {
	if ev.MuxSocket == "" || ev.PaneID == 0 || ev.Command == "" {
		return
	}
	d.rememberMuxSocket(ev.MuxSocket)
	if err := d.w.ApplyPreexec(ev.MuxSocket, ev.PaneID, ev.Command, ev.CWD, ev.Timestamp); err != nil {
		d.log.Printf("ApplyPreexec: %v", err)
	}
}

func (d *Daemon) applyPrecmd(ev Event) bool {
	if ev.MuxSocket == "" || ev.PaneID == 0 {
		return false
	}
	d.rememberMuxSocket(ev.MuxSocket)
	linked, linkedOK, lookupErr := d.w.LookupPaneRuntime(ev.MuxSocket, ev.PaneID)
	if lookupErr != nil {
		d.log.Printf("LookupPaneRuntime before precmd: %v", lookupErr)
	}
	if err := d.w.ApplyPrecmd(ev.MuxSocket, ev.PaneID, ev.CWD, ev.Timestamp); err != nil {
		d.log.Printf("ApplyPrecmd: %v", err)
	}
	if lookupErr != nil || !linkedOK || linked.SessionID == "" {
		return false
	}
	if d.sessions != nil {
		detached, err := d.sessions.DetachSession(
			linked.SessionID, linked.SessionProvider, ev.MuxSocket, ev.PaneID, ev.Timestamp,
		)
		if err != nil {
			d.log.Printf("DetachSession: %v", err)
		} else if detached {
			d.log.Printf("session detached: %s/%s from pane %d", linked.SessionProvider, linked.SessionID, ev.PaneID)
		}
	}
	if err := d.w.UnbindSession(ev.MuxSocket, ev.PaneID, linked.SessionProvider, linked.SessionID); err != nil {
		d.log.Printf("UnbindSession after precmd: %v", err)
	}
	return true
}

func (d *Daemon) scheduleSnapshot(pending *bool, coalesce *time.Timer) {
	if !*pending {
		*pending = true
		coalesce.Reset(d.opts.CoalesceWindow)
	}
}

func (d *Daemon) applySessionStart(ev Event) {
	if ev.MuxSocket == "" || ev.PaneID == 0 || ev.SessionID == "" {
		return
	}
	d.rememberMuxSocket(ev.MuxSocket)
	if err := d.w.BindSession(ev.MuxSocket, ev.PaneID, ev.Provider, ev.SessionID, ev.CWD); err != nil {
		d.log.Printf("BindSession: %v", err)
	}
}

func (d *Daemon) applySessionEnd(ev Event) {
	if ev.MuxSocket == "" || ev.PaneID == 0 {
		return
	}
	d.rememberMuxSocket(ev.MuxSocket)
	if err := d.w.UnbindSession(ev.MuxSocket, ev.PaneID, ev.Provider, ev.SessionID); err != nil {
		d.log.Printf("UnbindSession: %v", err)
	}
}

// runSnapshot calls `wezterm cli list` and syncs the result into the store.
// Errors are logged but not propagated — the daemon stays alive.
func (d *Daemon) runSnapshot() {
	raw, panes, err := wezterm.ListWithRaw()
	if err != nil {
		d.log.Printf("wezterm list: %v", err)
		return
	}
	res, err := snapshot.Sync(d.w, raw, panes, d.resolveMuxSocket(), time.Now())
	if err != nil {
		d.log.Printf("Sync: %v", err)
		return
	}
	if res.DidWork {
		d.log.Printf("snapshot: %d windows, %d tabs, %d panes (%d runtime pruned)",
			res.WindowsWritten, res.TabsWritten, res.PanesWritten, res.RuntimePruned)
	}
}

// resolveMuxSocket returns the wezterm mux socket path the snapshot should use
// when joining pane_runtime_state. Tries in order:
//  1. Daemon's own env ($WEZTERM_UNIX_SOCKET) — only set if the daemon was
//     started from inside a wezterm pane (rare in production).
//  2. Cached value from the most-recent incoming event (preexec/precmd/session).
//  3. Inferred from pane_runtime_state in the DB (most-recently-touched row).
//  4. Empty — snapshot still runs; just skips the join + runtime prune.
func (d *Daemon) resolveMuxSocket() string {
	if s := wezterm.MuxSocket(); s != "" {
		return s
	}
	if s := d.cachedMuxSocket(); s != "" {
		return s
	}
	if s, err := d.w.InferMuxSocket(); err == nil && s != "" {
		// Cache the inferred value so we don't re-query every snapshot.
		d.rememberMuxSocket(s)
		return s
	}
	return ""
}
