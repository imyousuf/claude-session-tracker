// Package shellsetup installs cst's shell integration: writes a precmd/preexec
// snippet to ~/.bashrc / ~/.zshrc / ~/.config/fish/config.fish, idempotently.
//
// The snippet pushes events to the cst daemon on every prompt cycle so the
// daemon can track per-pane state without polling.
package shellsetup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Shell enumerates supported shells.
type Shell string

const (
	ShellBash Shell = "bash"
	ShellZsh  Shell = "zsh"
	ShellFish Shell = "fish"
)

// Block markers — used to detect and replace the cst section idempotently.
// Format chosen to match the conda/pyenv convention so users recognize it.
const (
	BlockStart = "# >>> cst shell integration (do not edit) >>>"
	BlockEnd   = "# <<< cst shell integration <<<"
)

// Installer is the per-shell setup orchestrator.
type Installer interface {
	// Shell returns the shell this installer targets.
	Shell() Shell
	// RCFilePath returns the path of the rc file to modify (~/.bashrc, etc.).
	RCFilePath() string
	// Snippet returns the lines that go between BlockStart and BlockEnd.
	Snippet() string
	// EnsureDependencies downloads/verifies any extra files (e.g. bash-preexec).
	// May be a no-op for shells that don't need extras.
	EnsureDependencies() error
}

// Install applies the cst block to the shell's rc file. Idempotent — running
// it again replaces the block in place; no duplication.
func Install(i Installer) (changed bool, err error) {
	if err := i.EnsureDependencies(); err != nil {
		return false, fmt.Errorf("ensure deps for %s: %w", i.Shell(), err)
	}
	path := i.RCFilePath()
	existing, err := readOrEmpty(path)
	if err != nil {
		return false, fmt.Errorf("read rc: %w", err)
	}
	stripped := stripBlock(existing)
	block := fmt.Sprintf("\n%s\n%s%s\n", BlockStart, ensureTrailingNewline(i.Snippet()), BlockEnd)
	updated := stripped + block

	if updated == existing {
		return false, nil
	}
	if err := atomicWrite(path, []byte(updated), 0o644); err != nil {
		return false, fmt.Errorf("write rc: %w", err)
	}
	return true, nil
}

// Uninstall removes the cst block from the shell's rc file. Returns whether
// the file was modified.
func Uninstall(i Installer) (changed bool, err error) {
	path := i.RCFilePath()
	existing, err := readOrEmpty(path)
	if err != nil {
		return false, fmt.Errorf("read rc: %w", err)
	}
	stripped := stripBlock(existing)
	if stripped == existing {
		return false, nil
	}
	if err := atomicWrite(path, []byte(stripped), 0o644); err != nil {
		return false, fmt.Errorf("write rc: %w", err)
	}
	return true, nil
}

// stripBlock removes any existing cst block from content, returning the
// content without it. If no block is present, returns content unchanged
// (possibly with a normalized trailing newline).
func stripBlock(content string) string {
	startIdx := strings.Index(content, BlockStart)
	if startIdx < 0 {
		return content
	}
	// Find the matching end marker AFTER startIdx.
	rest := content[startIdx+len(BlockStart):]
	endRelative := strings.Index(rest, BlockEnd)
	if endRelative < 0 {
		// No end marker; conservatively leave file alone.
		return content
	}
	endIdx := startIdx + len(BlockStart) + endRelative + len(BlockEnd)

	// Also swallow the newline immediately after BlockEnd, and the newline
	// immediately before BlockStart, so we don't accumulate blank lines on
	// repeated install/uninstall cycles.
	before := content[:startIdx]
	after := content[endIdx:]

	before = strings.TrimRight(before, "\n")
	after = strings.TrimLeft(after, "\n")

	if before == "" && after == "" {
		return ""
	}
	if before == "" {
		return ensureTrailingNewline(after)
	}
	if after == "" {
		return ensureTrailingNewline(before)
	}
	return ensureTrailingNewline(before + "\n" + after)
}

// readOrEmpty reads path or returns "" if it doesn't exist.
func readOrEmpty(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// atomicWrite writes data to path via a temp file in the same directory + rename.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cst-tmp-*")
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// --- bash-preexec download (used by bash installer) ---

// Bash-preexec pinned release. Pinned because the snippet relies on specific
// hook function names; if the upstream API ever shifts, we want to know
// before silently breaking users.
//
// Exposed as vars (not const) so tests can swap to an httptest server.
var (
	BashPreexecURL    = "https://raw.githubusercontent.com/rcaloras/bash-preexec/0.5.0/bash-preexec.sh"
	BashPreexecSHA256 = "" // empty = skip verification; set to pin a known SHA
)

// BashPreexecDownloader is the http client used to fetch bash-preexec.
// Exposed for tests.
var BashPreexecDownloader = http.DefaultClient

// EnsureBashPreexec downloads bash-preexec.sh into ~/.cst/ if not already present
// (or if SHA mismatch). Returns the file path on success.
func EnsureBashPreexec(destDir string) (string, error) {
	dest := filepath.Join(destDir, "bash-preexec.sh")

	if existing, err := os.ReadFile(dest); err == nil {
		if BashPreexecSHA256 == "" || hex.EncodeToString(sha256Hash(existing)) == BashPreexecSHA256 {
			return dest, nil
		}
		// SHA mismatch — re-download.
	}

	req, err := http.NewRequest("GET", BashPreexecURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := BashPreexecDownloader.Do(req)
	if err != nil {
		return "", fmt.Errorf("download bash-preexec: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download bash-preexec: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if BashPreexecSHA256 != "" {
		got := hex.EncodeToString(sha256Hash(body))
		if got != BashPreexecSHA256 {
			return "", fmt.Errorf("bash-preexec SHA mismatch: got %s, want %s", got, BashPreexecSHA256)
		}
	}
	if err := atomicWrite(dest, body, 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

func sha256Hash(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// HomeDir resolves $HOME with a sensible fallback.
func HomeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

// CstDir returns ~/.cst, creating it if needed.
func CstDir() (string, error) {
	d := filepath.Join(HomeDir(), ".cst")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// SilentLogPath is a place callers can drop status info to without forcing a
// dependency on a logger. Currently unused; reserved for future "install log".
func SilentLogPath() string {
	return filepath.Join(HomeDir(), ".cst", "setup.log")
}

// freshen returns time.Now in RFC3339 — used as a sentinel in logs.
func freshen() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// silence the "unused" warning for the helper while we don't have a log path yet.
var _ = freshen
