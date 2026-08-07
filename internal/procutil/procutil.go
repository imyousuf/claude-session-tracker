package procutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// AgentAttachment identifies the interactive provider CLI rather than a
// long-lived background service that happens to launch hooks.
type AgentAttachment struct {
	PID       int
	MuxSocket string
	PaneID    int64
}

// FindAgentAttachment finds the interactive provider process associated with
// a hook event. Codex hooks may be launched by a persistent app-server whose
// WEZTERM_PANE points at an unrelated pane, so Linux resolves the frontend by
// scanning terminal-backed provider processes instead of trusting ancestry.
func FindAgentAttachment(provider, sessionID, cwd string) (AgentAttachment, bool) {
	if runtime.GOOS != "linux" {
		return AgentAttachment{}, false
	}
	return findAgentAttachmentLinuxAt("/proc", provider, sessionID, cwd)
}

type agentCandidate struct {
	attachment AgentAttachment
	score      int
	started    uint64
}

func findAgentAttachmentLinuxAt(procRoot, provider, sessionID, cwd string) (AgentAttachment, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return AgentAttachment{}, false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	wantCWD := canonicalPath(cwd)
	var best agentCandidate
	found := false

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		base := filepath.Join(procRoot, entry.Name())
		cmdline, err := os.ReadFile(filepath.Join(base, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if !interactiveProviderArgs(args, provider) {
			continue
		}
		environ, err := os.ReadFile(filepath.Join(base, "environ"))
		if err != nil {
			continue
		}
		muxSocket, paneID := weztermAttachmentFromEnvironment(environ)
		if muxSocket == "" || paneID == 0 {
			continue
		}

		hasSessionID := sessionID != "" && argsContain(args, sessionID)
		processCWD, _ := os.Readlink(filepath.Join(base, "cwd"))
		sameCWD := wantCWD != "" && canonicalPath(processCWD) == wantCWD
		if !hasSessionID && !sameCWD {
			continue
		}

		score := 0
		if hasSessionID {
			score += 1000
		}
		if sameCWD {
			score += 100
		}
		if len(args) > 0 && strings.Contains(strings.ToLower(filepath.Base(args[0])), provider) {
			score += 10
		}
		candidate := agentCandidate{
			attachment: AgentAttachment{PID: pid, MuxSocket: muxSocket, PaneID: paneID},
			score:      score,
			started:    linuxStartTicks(filepath.Join(base, "stat")),
		}
		if !found || candidate.score > best.score ||
			(candidate.score == best.score && candidate.started > best.started) {
			best = candidate
			found = true
		}
	}
	return best.attachment, found
}

func interactiveProviderArgs(args []string, provider string) bool {
	if len(args) == 0 || provider == "" {
		return false
	}
	joined := strings.ToLower(strings.Join(args, " "))
	for _, auxiliary := range []string{"app-server", "code-mode-host", " mcp", "cst hook"} {
		if strings.Contains(joined, auxiliary) {
			return false
		}
	}
	for _, arg := range args {
		if strings.Contains(strings.ToLower(arg), provider) {
			return true
		}
	}
	return false
}

func weztermAttachmentFromEnvironment(data []byte) (string, int64) {
	var muxSocket string
	var paneID int64
	for _, item := range strings.Split(string(data), "\x00") {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch key {
		case "WEZTERM_UNIX_SOCKET":
			muxSocket = value
		case "WEZTERM_PANE":
			paneID, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	return muxSocket, paneID
}

func argsContain(args []string, value string) bool {
	for _, arg := range args {
		if strings.Contains(arg, value) {
			return true
		}
	}
	return false
}

func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func linuxStartTicks(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0
	}
	fields := strings.Fields(string(data[end+1:]))
	// After removing pid/comm, index 19 is field 22 (starttime).
	if len(fields) <= 19 {
		return 0
	}
	started, _ := strconv.ParseUint(fields[19], 10, 64)
	return started
}

// FindAgentPID walks upward from the hook process and returns the first
// ancestor that looks like the requested coding-agent provider. Codex and
// Claude command hooks are commonly launched through a short-lived shell, so
// os.Getppid() alone would record that shell and make active sessions appear
// inactive as soon as the hook exits.
func FindAgentPID(provider string) int {
	provider = strings.ToLower(strings.TrimSpace(provider))
	pid := os.Getppid()
	if runtime.GOOS != "linux" {
		return pid
	}
	for range 32 {
		if pid <= 1 {
			break
		}
		if processLooksLikeProvider(pid, provider) {
			return pid
		}
		parent, err := linuxParentPID(pid)
		if err != nil || parent == pid {
			break
		}
		pid = parent
	}
	return os.Getppid()
}

// IsProcessAlive checks if a process with the given PID is still running
// and appears to be a supported coding-agent process.
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

	return isCodingAgent(pid)
}

// isCodingAgent checks whether the given PID belongs to a supported CLI.
func isCodingAgent(pid int) bool {
	switch runtime.GOOS {
	case "linux":
		return isCodingAgentLinux(pid)
	case "darwin":
		return isCodingAgentDarwin(pid)
	default:
		// On unknown OS, assume alive if signal(0) passed
		return true
	}
}

// isCodingAgentLinux reads /proc/<pid>/cmdline to check for a supported CLI.
func isCodingAgentLinux(pid int) bool {
	data, err := os.ReadFile(procCmdlinePath(pid))
	if err != nil {
		return false
	}
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")
	cmdline = strings.ToLower(cmdline)
	return strings.Contains(cmdline, "claude") || strings.Contains(cmdline, "codex")
}

func processLooksLikeProvider(pid int, provider string) bool {
	data, err := os.ReadFile(procCmdlinePath(pid))
	if err != nil {
		return false
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(parts) == 0 {
		return false
	}
	base := strings.ToLower(filepath.Base(parts[0]))
	switch base {
	case "sh", "bash", "dash", "zsh", "fish", "cst":
		return false
	}
	if strings.Contains(base, provider) {
		return true
	}
	// Node-based distributions often have executable "node" and put the CLI's
	// package/script path in argv[1].
	for _, arg := range parts[1:] {
		if strings.Contains(strings.ToLower(arg), provider) {
			return true
		}
	}
	return false
}

func linuxParentPID(pid int) (int, error) {
	data, err := os.ReadFile("/proc/" + itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, os.ErrInvalid
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 2 {
		return 0, os.ErrInvalid
	}
	return strconv.Atoi(fields[1])
}

// isCodingAgentDarwin is a placeholder for macOS. On macOS, /proc doesn't exist
// but we could use `ps -p <pid> -o command=`. For now, if the process is alive, return true.
func isCodingAgentDarwin(pid int) bool {
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
