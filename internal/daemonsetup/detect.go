package daemonsetup

import (
	"os/exec"
)

// SystemdAvailable reports whether systemd-user is usable on this host.
// True if:
//   - `systemctl` binary is on PATH
//   - `systemctl --user is-system-running` exits 0 OR returns "running"/"degraded"
//     (a stopped system bus produces a non-zero exit and we fall through to false)
func SystemdAvailable() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	cmd := exec.Command("systemctl", "--user", "is-system-running", "--quiet")
	if err := cmd.Run(); err != nil {
		// is-system-running exits non-zero for "degraded" / "starting" too; treat
		// as available since those still let us enable units. Fall back to a
		// stricter check.
		return systemdSocketReachable()
	}
	return true
}

// systemdSocketReachable returns true if a user systemd manager appears to
// be reachable (used as a fallback when is-system-running returns non-zero).
func systemdSocketReachable() bool {
	cmd := exec.Command("systemctl", "--user", "list-units", "--no-pager", "--no-legend")
	return cmd.Run() == nil
}
