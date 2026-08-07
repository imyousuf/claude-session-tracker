package daemonsetup

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type capturedCall struct {
	bin  string
	args []string
}

func captureSystemctl(t *testing.T) *[]capturedCall {
	t.Helper()
	var mu sync.Mutex
	calls := []capturedCall{}
	restore := SetSystemctlRunner(func(bin string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, capturedCall{bin: bin, args: append([]string{}, args...)})
		return nil
	})
	t.Cleanup(restore)
	return &calls
}

func TestInstallWritesUnitWithBinaryPath(t *testing.T) {
	dir := t.TempDir()
	calls := captureSystemctl(t)

	res, err := Install(Options{
		UnitDir: dir,
		CstBin:  "/usr/local/bin/cst",
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Wrote {
		t.Error("expected Wrote=true on first install")
	}
	if !res.DaemonReload {
		t.Error("expected DaemonReload after write")
	}

	body, _ := os.ReadFile(res.UnitPath)
	if !strings.Contains(string(body), "ExecStart=/usr/local/bin/cst daemon --idle-timeout=0") {
		t.Errorf("unit missing absolute ExecStart:\n%s", body)
	}
	if !strings.Contains(string(body), "MemoryMax=64M") {
		t.Error("unit missing memory cap")
	}
	if !strings.Contains(string(body), "WantedBy=default.target") {
		t.Error("unit missing WantedBy")
	}

	// Should have called daemon-reload exactly once.
	if len(*calls) != 1 {
		t.Fatalf("expected 1 systemctl call, got %d", len(*calls))
	}
	if (*calls)[0].args[len((*calls)[0].args)-1] != "daemon-reload" {
		t.Errorf("first call args: %v", (*calls)[0].args)
	}
}

func TestInstallIdempotent(t *testing.T) {
	dir := t.TempDir()
	calls := captureSystemctl(t)

	if _, err := Install(Options{UnitDir: dir, CstBin: "/usr/bin/cst"}); err != nil {
		t.Fatal(err)
	}
	res, err := Install(Options{UnitDir: dir, CstBin: "/usr/bin/cst"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Wrote {
		t.Error("second install should not have rewritten unit file")
	}
	if res.DaemonReload {
		t.Error("second install should not have triggered daemon-reload")
	}
	// Only the first install triggers a single daemon-reload.
	if len(*calls) != 1 {
		t.Errorf("expected 1 systemctl call total, got %d", len(*calls))
	}
}

func TestInstallEnableNow(t *testing.T) {
	dir := t.TempDir()
	calls := captureSystemctl(t)

	res, err := Install(Options{
		UnitDir:   dir,
		CstBin:    "/usr/bin/cst",
		EnableNow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Enabled {
		t.Error("expected Enabled=true")
	}
	if !res.Restarted {
		t.Error("expected Restarted=true")
	}
	// Expect: daemon-reload, enable --now, then restart to load the new binary.
	if len(*calls) != 3 {
		t.Fatalf("expected 3 systemctl calls, got %d", len(*calls))
	}
	wantEnable := []string{"--user", "enable", "--now", "cst-daemon.service"}
	got := (*calls)[1].args
	if len(got) != len(wantEnable) {
		t.Fatalf("enable args = %v", got)
	}
	for i := range got {
		if got[i] != wantEnable[i] {
			t.Errorf("enable args[%d] = %q, want %q", i, got[i], wantEnable[i])
		}
	}
	wantRestart := []string{"--user", "restart", "cst-daemon.service"}
	got = (*calls)[2].args
	if len(got) != len(wantRestart) {
		t.Fatalf("restart args = %v", got)
	}
	for i := range got {
		if got[i] != wantRestart[i] {
			t.Errorf("restart args[%d] = %q, want %q", i, got[i], wantRestart[i])
		}
	}
}

func TestUninstallRemovesAndDisables(t *testing.T) {
	dir := t.TempDir()
	// Seed an existing unit file.
	path := filepath.Join(dir, ServiceName)
	if err := os.WriteFile(path, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := captureSystemctl(t)

	if _, err := Uninstall(Options{UnitDir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("unit file not removed: %v", err)
	}

	// Should have called disable --now and daemon-reload.
	if len(*calls) != 2 {
		t.Fatalf("expected 2 systemctl calls, got %d", len(*calls))
	}
	if (*calls)[0].args[1] != "disable" {
		t.Errorf("first call: %v", (*calls)[0].args)
	}
}
