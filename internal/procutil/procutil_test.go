package procutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeFakeProcess(t *testing.T, root string, pid int, args []string, cwd, mux string, pane int64, started uint64) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := fmt.Sprintf("WEZTERM_UNIX_SOCKET=%s\x00WEZTERM_PANE=%d\x00", mux, pane)
	if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(environ), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cwd, filepath.Join(dir, "cwd")); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[19] = strconv.FormatUint(started, 10)
	stat := fmt.Sprintf("%d (codex) %s\n", pid, strings.Join(fields, " "))
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindAgentAttachmentUsesInteractiveCodexFrontend(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	writeFakeProcess(t, root, 100,
		[]string{"codex", "app-server", "--listen", "unix://"}, cwd, "/run/mux", 18, 100)
	writeFakeProcess(t, root, 200,
		[]string{"codex", "resume", "thread-123"}, cwd, "/run/mux", 4, 200)

	got, ok := findAgentAttachmentLinuxAt(root, "codex", "thread-123", cwd)
	if !ok {
		t.Fatal("interactive Codex frontend not found")
	}
	if got.PID != 200 || got.PaneID != 4 || got.MuxSocket != "/run/mux" {
		t.Fatalf("attachment = %+v, want frontend pid 200 in pane 4", got)
	}
}

func TestFindAgentAttachmentUsesNewestMatchingCWDForNewThread(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	writeFakeProcess(t, root, 100, []string{"codex"}, cwd, "/run/mux", 3, 100)
	writeFakeProcess(t, root, 200, []string{"codex"}, cwd, "/run/mux", 4, 200)

	got, ok := findAgentAttachmentLinuxAt(root, "codex", "generated-thread-id", cwd)
	if !ok || got.PID != 200 || got.PaneID != 4 {
		t.Fatalf("attachment = %+v ok=%v, want newest frontend", got, ok)
	}
}
