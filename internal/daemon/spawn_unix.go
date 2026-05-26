//go:build linux || darwin

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

// spawnDetachedExec runs `exe args...` in a fully detached process:
//   - stdout/stderr go to logFile (inherited fd)
//   - stdin is /dev/null
//   - new session via Setsid so the child survives parent exit
//   - process is "released" so Go's GC doesn't keep state about it
//
// Returns nil if exec was successfully started.
func spawnDetachedExec(exe string, args []string, logFile *os.File) error {
	devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = devNull.Close()
		_ = logFile.Close()
		return err
	}
	// Release: don't wait, don't keep handles.
	_ = cmd.Process.Release()
	_ = devNull.Close()
	// logFile stays open in child; ours can close.
	_ = logFile.Close()
	return nil
}
