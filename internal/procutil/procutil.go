package procutil

import (
	"os"
	"runtime"
	"strings"
	"syscall"
)

// IsProcessAlive checks if a process with the given PID is still running
// and appears to be a Claude Code process.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// On Unix, FindProcess always succeeds. Use signal 0 to probe.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}

	return isClaude(pid)
}

// isClaude checks whether the given PID belongs to a Claude Code process.
func isClaude(pid int) bool {
	switch runtime.GOOS {
	case "linux":
		return isClaudeLinux(pid)
	case "darwin":
		return isClaudeDarwin(pid)
	default:
		// On unknown OS, assume alive if signal(0) passed
		return true
	}
}

// isClaudeLinux reads /proc/<pid>/cmdline to check for "claude".
func isClaudeLinux(pid int) bool {
	data, err := os.ReadFile(procCmdlinePath(pid))
	if err != nil {
		return false
	}
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")
	return strings.Contains(strings.ToLower(cmdline), "claude")
}

// isClaudeDarwin is a placeholder for macOS. On macOS, /proc doesn't exist
// but we could use `ps -p <pid> -o command=`. For now, if the process is alive, return true.
func isClaudeDarwin(pid int) bool {
	// On macOS, if signal(0) succeeded the process is alive.
	// We could shell out to `ps` but that adds complexity. Accept the PID as valid.
	return true
}

func procCmdlinePath(pid int) string {
	return "/proc/" + itoa(pid) + "/cmdline"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
