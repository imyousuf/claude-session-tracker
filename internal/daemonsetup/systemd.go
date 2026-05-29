// Package daemonsetup installs cst's systemd-user service for the daemon.
package daemonsetup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const ServiceName = "cst-daemon.service"

// UnitTemplate is the systemd unit file written to
// ~/.config/systemd/user/cst-daemon.service.
//
// Placeholder: %s is replaced with the absolute path to the cst binary.
const UnitTemplate = `[Unit]
Description=CST snapshot daemon (per-user)
Documentation=https://github.com/imyousuf/claude-session-tracker
After=graphical-session.target

[Service]
Type=simple
ExecStart=%s daemon
Restart=on-failure
RestartSec=5s
# The daemon is intentionally tiny; cap its blast radius.
MemoryMax=64M
CPUQuota=10%%
# Ensure the daemon socket goes in the user runtime dir.
Environment=XDG_RUNTIME_DIR=%%t

[Install]
WantedBy=default.target
`

// Options configures the install.
type Options struct {
	// UnitDir overrides ~/.config/systemd/user (for tests).
	UnitDir string
	// CstBin overrides the path to the cst binary written into ExecStart.
	// Defaults to os.Executable() (the running cst binary).
	CstBin string
	// SystemctlBin overrides "systemctl" (for tests).
	SystemctlBin string
	// EnableNow runs `systemctl --user enable --now cst-daemon` after install.
	EnableNow bool
}

// Result describes what Install did.
type Result struct {
	UnitPath     string
	Wrote        bool // true if the unit file content changed
	DaemonReload bool // true if we ran systemctl --user daemon-reload
	Enabled      bool // true if we ran enable --now
}

func defaultedOptions(o Options) (Options, error) {
	if o.UnitDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return o, err
		}
		o.UnitDir = filepath.Join(home, ".config", "systemd", "user")
	}
	if o.CstBin == "" {
		exe, err := os.Executable()
		if err != nil {
			return o, err
		}
		// Resolve symlinks so we get the real binary path (not a symlink that
		// could be removed later).
		if real, err := os.Readlink(exe); err == nil && real != "" && filepath.IsAbs(real) {
			exe = real
		}
		o.CstBin = exe
	}
	if o.SystemctlBin == "" {
		o.SystemctlBin = "systemctl"
	}
	return o, nil
}

// Install writes the unit file and (optionally) enables it.
func Install(o Options) (Result, error) {
	o, err := defaultedOptions(o)
	if err != nil {
		return Result{}, err
	}
	res := Result{UnitPath: filepath.Join(o.UnitDir, ServiceName)}

	if err := os.MkdirAll(o.UnitDir, 0o755); err != nil {
		return res, fmt.Errorf("create unit dir: %w", err)
	}

	content := fmt.Sprintf(UnitTemplate, o.CstBin)
	existing, _ := os.ReadFile(res.UnitPath)
	if string(existing) != content {
		if err := atomicWrite(res.UnitPath, []byte(content), 0o644); err != nil {
			return res, fmt.Errorf("write unit file: %w", err)
		}
		res.Wrote = true
	}

	if res.Wrote {
		if err := runSystemctl(o.SystemctlBin, "--user", "daemon-reload"); err != nil {
			return res, fmt.Errorf("systemctl daemon-reload: %w", err)
		}
		res.DaemonReload = true
	}

	if o.EnableNow {
		if err := runSystemctl(o.SystemctlBin, "--user", "enable", "--now", ServiceName); err != nil {
			return res, fmt.Errorf("systemctl enable --now: %w", err)
		}
		res.Enabled = true
	}
	return res, nil
}

// Uninstall disables the unit and removes the file.
func Uninstall(o Options) (Result, error) {
	o, err := defaultedOptions(o)
	if err != nil {
		return Result{}, err
	}
	res := Result{UnitPath: filepath.Join(o.UnitDir, ServiceName)}

	// Best-effort disable+stop; ignore errors (unit may not be enabled).
	_ = runSystemctl(o.SystemctlBin, "--user", "disable", "--now", ServiceName)

	if err := os.Remove(res.UnitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("remove unit file: %w", err)
	}
	_ = runSystemctl(o.SystemctlBin, "--user", "daemon-reload")
	res.Wrote = true
	res.DaemonReload = true
	return res, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cst-tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// systemctlRunner is the function used to invoke systemctl. Tests substitute
// a fake; production uses runSystemctlReal.
var systemctlRunner = runSystemctlReal

func runSystemctl(bin string, args ...string) error {
	return systemctlRunner(bin, args...)
}

func runSystemctlReal(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetSystemctlRunner installs a custom runner (for tests). Returns a restorer.
func SetSystemctlRunner(fn func(bin string, args ...string) error) func() {
	prev := systemctlRunner
	systemctlRunner = fn
	return func() { systemctlRunner = prev }
}
