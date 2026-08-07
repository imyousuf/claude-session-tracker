package codexsetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallMergesPreservesAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	existing := `{
  "custom": {"keep": true},
  "hooks": {
    "SessionStart": [
      {"matcher":"startup","hooks":[{"type":"command","command":"other-tool start","timeout":9}]}
    ],
    "Stop": [
      {"hooks":[{"type":"command","command":"other-tool stop"}]}
    ]
  }
}
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Install(Options{Path: path, Executable: "/opt/CST Bin/cst"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Changed {
		t.Fatal("first install should report Changed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"other-tool start",
		"other-tool stop",
		"'/opt/CST Bin/cst' hook session-start --provider codex",
		"'/opt/CST Bin/cst' hook prompt --provider codex",
		"'/opt/CST Bin/cst' hook session-end --provider codex",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("installed hooks missing %q:\n%s", want, text)
		}
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("installed JSON invalid: %v", err)
	}
	if _, ok := root["custom"]; !ok {
		t.Error("custom top-level field was not preserved")
	}

	res, err = Install(Options{Path: path, Executable: "/opt/CST Bin/cst"})
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if res.Changed {
		t.Error("second install should be idempotent")
	}
}

func TestUninstallRemovesOnlyCSTHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	if _, err := Install(Options{Path: path, Executable: "/usr/local/bin/cst"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	hooks := root["hooks"].(map[string]any)
	hooks["Stop"] = []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "keep-me"}},
	}}
	data, _ = json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Uninstall(Options{Path: path})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !res.Changed {
		t.Fatal("uninstall should report Changed")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "--provider codex") {
		t.Errorf("CST handlers remain after uninstall:\n%s", text)
	}
	if !strings.Contains(text, "keep-me") {
		t.Errorf("unrelated hook was removed:\n%s", text)
	}
}

func TestDefaultPathUsesCodexHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if got, want := DefaultPath(), filepath.Join(dir, "hooks.json"); got != want {
		t.Fatalf("DefaultPath = %q, want %q", got, want)
	}
}
