package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// SocketPath returns the per-user daemon socket path.
//
//	$XDG_RUNTIME_DIR/cst-daemon-$UID.sock  (preferred)
//	/tmp/cst-daemon-$UID.sock              (fallback)
//
// Each user gets their own socket, isolated by UNIX file permissions.
func SocketPath() string {
	uid := os.Getuid()
	name := fmt.Sprintf("cst-daemon-%d.sock", uid)
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, name)
	}
	return filepath.Join("/tmp", name)
}

// PIDFilePath returns the path to the daemon's PID file (~/.cst/daemon.pid).
func PIDFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".cst", "daemon.pid")
}

// LogPath returns the path to the daemon's log file when not under systemd
// (~/.cst/daemon.log). Under systemd, the daemon writes to stderr and the
// supervisor handles logging.
func LogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".cst", "daemon.log")
}

// CheckSocketDirOwnership returns an error if the directory containing the
// daemon socket exists but is not owned by the current user. Defense-in-depth
// against socket-stealing on shared machines.
func CheckSocketDirOwnership(sockPath string) error {
	dir := filepath.Dir(sockPath)
	info, err := os.Stat(dir)
	if err != nil {
		// Directory doesn't exist yet; we'll create it (with our own ownership).
		return nil
	}
	sys, err := statSysOwner(info)
	if err != nil {
		return err
	}
	if sys != uint32(os.Getuid()) {
		return fmt.Errorf("socket directory %s is owned by uid %d, not current user %d",
			dir, sys, os.Getuid())
	}
	return nil
}

// ReadPIDFile reads and parses the PID file. Returns 0 and no error if the
// file doesn't exist; returns the PID otherwise.
func ReadPIDFile() (int, error) {
	data, err := os.ReadFile(PIDFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	pid, err := strconv.Atoi(string(stripNewline(data)))
	if err != nil {
		return 0, fmt.Errorf("parse pid file: %w", err)
	}
	return pid, nil
}

// WritePIDFile writes the current process PID to the daemon PID file.
func WritePIDFile() error {
	path := PIDFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// RemovePIDFile deletes the daemon PID file on graceful shutdown, but only if
// it still names this process. During an upgrade, a replacement daemon may
// already have written its own PID before the old daemon finishes draining.
func RemovePIDFile() error {
	pid, err := ReadPIDFile()
	if err != nil {
		return err
	}
	if pid != 0 && pid != os.Getpid() {
		return nil
	}
	err = os.Remove(PIDFilePath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func stripNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
