package shellsetup

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAndUninstallBash(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, ".bashrc")
	cstDir := filepath.Join(dir, ".cst")

	// Seed an existing rc with some user content.
	if err := os.WriteFile(rc, []byte("# existing user content\nexport FOO=1\n"), 0o644); err != nil {
		t.Fatalf("seed rc: %v", err)
	}

	// Fake bash-preexec server so EnsureDependencies doesn't hit the real URL.
	prevURL := BashPreexecURL
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "# fake bash-preexec\n")
	}))
	defer server.Close()
	overrideBashPreexecURL(t, server.URL)
	_ = prevURL

	bi := BashInstaller{RCPath: rc, CstDirPath: cstDir}

	changed, err := Install(bi)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on first install")
	}

	content, _ := os.ReadFile(rc)
	if !strings.Contains(string(content), BlockStart) {
		t.Error("rc file missing BlockStart marker")
	}
	if !strings.Contains(string(content), BlockEnd) {
		t.Error("rc file missing BlockEnd marker")
	}
	if !strings.Contains(string(content), "# existing user content") {
		t.Error("user content was clobbered")
	}

	// bash-preexec was downloaded.
	if _, err := os.Stat(filepath.Join(cstDir, "bash-preexec.sh")); err != nil {
		t.Errorf("bash-preexec.sh not created: %v", err)
	}

	// Re-install: should be no-op (same content, no marker duplication).
	changed, err = Install(bi)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if changed {
		t.Error("second install should be no-op")
	}
	content2, _ := os.ReadFile(rc)
	if string(content2) != string(content) {
		t.Error("second install changed content")
	}
	if strings.Count(string(content), BlockStart) != 1 {
		t.Error("BlockStart appears more than once")
	}

	// Uninstall: block goes away, user content stays.
	changed, err = Uninstall(bi)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !changed {
		t.Error("expected changed=true on uninstall")
	}
	content3, _ := os.ReadFile(rc)
	if strings.Contains(string(content3), BlockStart) {
		t.Error("block not removed")
	}
	if !strings.Contains(string(content3), "# existing user content") {
		t.Error("user content lost on uninstall")
	}

	// Uninstall again: no-op.
	changed, err = Uninstall(bi)
	if err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
	if changed {
		t.Error("second uninstall should be no-op")
	}
}

func TestInstallIntoMissingRC(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, ".zshrc")
	zi := ZshInstaller{RCPath: rc}
	changed, err := Install(zi)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !changed {
		t.Error("expected install to create file")
	}
	content, _ := os.ReadFile(rc)
	if !strings.Contains(string(content), BlockStart) {
		t.Error("BlockStart missing in new file")
	}
}

func TestZshSnippetHasNativeHookRegistration(t *testing.T) {
	zi := ZshInstaller{}
	s := zi.Snippet()
	if !strings.Contains(s, "preexec_functions") {
		t.Error("zsh snippet should use preexec_functions")
	}
	if !strings.Contains(s, "precmd_functions") {
		t.Error("zsh snippet should use precmd_functions")
	}
}

func TestFishSnippetUsesOnEvent(t *testing.T) {
	fi := FishInstaller{}
	s := fi.Snippet()
	if !strings.Contains(s, "--on-event fish_preexec") {
		t.Error("fish snippet should use --on-event fish_preexec")
	}
	if !strings.Contains(s, "--on-event fish_prompt") {
		t.Error("fish snippet should use --on-event fish_prompt")
	}
}

func TestNewInstallerForShell(t *testing.T) {
	for _, s := range []Shell{ShellBash, ShellZsh, ShellFish} {
		i, err := NewInstallerForShell(s)
		if err != nil {
			t.Errorf("NewInstallerForShell(%s): %v", s, err)
		}
		if i.Shell() != s {
			t.Errorf("shell mismatch: %s != %s", i.Shell(), s)
		}
	}
	if _, err := NewInstallerForShell("nushell"); err == nil {
		t.Error("expected error for unknown shell")
	}
}

func TestStripBlockHandlesMissingEnd(t *testing.T) {
	// Pathological input: marker start but no end. Preserve the file untouched.
	content := "hello\n" + BlockStart + "\noops no end marker\nworld\n"
	got := stripBlock(content)
	if got != content {
		t.Errorf("missing end marker should be conservative: got %q", got)
	}
}

func TestStripBlockRoundTrip(t *testing.T) {
	input := "before\n" + BlockStart + "\ninside\n" + BlockEnd + "\nafter\n"
	got := stripBlock(input)
	if got != "before\nafter\n" {
		t.Errorf("strip output = %q", got)
	}
}

func TestEnsureBashPreexecSkipsRedownloadWhenPresent(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "bash-preexec.sh")
	if err := os.WriteFile(dest, []byte("cached content"), 0o644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not have hit the network")
	}))
	defer server.Close()
	overrideBashPreexecURL(t, server.URL)

	got, err := EnsureBashPreexec(dir)
	if err != nil {
		t.Fatalf("EnsureBashPreexec: %v", err)
	}
	if got != dest {
		t.Errorf("path = %q", got)
	}
}

func overrideBashPreexecURL(t *testing.T, url string) {
	t.Helper()
	prev := BashPreexecURL
	BashPreexecURL = url
	t.Cleanup(func() { BashPreexecURL = prev })
}
