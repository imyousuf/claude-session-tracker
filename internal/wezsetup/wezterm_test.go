package wezsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lastMeaningfulLine returns the last non-blank, non-comment line of a Lua
// chunk. Used to assert the chunk's final statement is a `return` (so wezterm
// receives a Config, never nil).
func lastMeaningfulLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		return t
	}
	return ""
}

func TestInstallCreatesBothFiles(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		WeztermLuaPath: filepath.Join(dir, ".wezterm.lua"),
		CstModulePath:  filepath.Join(dir, ".config", "wezterm", "cst.lua"),
	}

	changed, err := Install(opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}

	// .wezterm.lua should have the loader block and require cst.
	got, _ := os.ReadFile(opts.WeztermLuaPath)
	if !strings.Contains(string(got), BlockStart) {
		t.Error("wezterm.lua missing BlockStart")
	}
	if !strings.Contains(string(got), "require('cst')") {
		t.Error("wezterm.lua missing require('cst')")
	}

	// cst.lua should have the full module.
	mod, _ := os.ReadFile(opts.CstModulePath)
	if !strings.Contains(string(mod), "wezterm.on('gui-startup'") {
		t.Error("cst.lua missing gui-startup handler")
	}
	if !strings.Contains(string(mod), "new-tab-button-click") {
		t.Error("cst.lua missing new-tab-button-click handler")
	}
	if !strings.Contains(string(mod), "function M.apply_to_config") {
		t.Error("cst.lua missing apply_to_config")
	}
	// gui-startup must invoke restore with --spawn-if-empty and point at the log.
	if !strings.Contains(string(mod), "'restore', '--spawn-if-empty'") {
		t.Error("cst.lua gui-startup missing restore --spawn-if-empty")
	}
	if !strings.Contains(string(mod), "~/.cst/restore.log") {
		t.Error("cst.lua missing restore.log pointer in gui-startup")
	}
}

// TestInstallEmptyReturnsConfig is the regression test for the
// "Cannot convert Null to Config" wezterm error: a freshly-created
// ~/.wezterm.lua must end by returning a config, not nil.
func TestInstallEmptyReturnsConfig(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		WeztermLuaPath: filepath.Join(dir, ".wezterm.lua"),
		CstModulePath:  filepath.Join(dir, "cst.lua"),
	}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(opts.WeztermLuaPath)
	if last := lastMeaningfulLine(string(got)); last != "return cst_config" {
		t.Errorf("expected file to end by returning a config, last line = %q\n---\n%s", last, got)
	}
	if !strings.Contains(string(got), "wezterm.config_builder()") {
		t.Error("expected a config_builder() to be created")
	}
}

func TestInstallPreservesExistingWeztermConfig(t *testing.T) {
	dir := t.TempDir()
	weztermLua := filepath.Join(dir, ".wezterm.lua")
	original := `local wezterm = require 'wezterm'
local config = wezterm.config_builder()
config.color_scheme = 'Tokyo Night'
return config
`
	_ = os.WriteFile(weztermLua, []byte(original), 0o644)

	opts := Options{
		WeztermLuaPath: weztermLua,
		CstModulePath:  filepath.Join(dir, "cst.lua"),
	}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(weztermLua)
	if !strings.Contains(string(got), "Tokyo Night") {
		t.Error("user color scheme was clobbered")
	}
	if !strings.Contains(string(got), BlockStart) {
		t.Error("loader block missing")
	}
	// Keybindings should merge into the user's own config variable.
	if !strings.Contains(string(got), "apply_to_config(config)") {
		t.Error("expected apply_to_config(config) to merge keybindings")
	}
	// The user's `return config` must remain the final statement (valid Lua:
	// no code after a top-level return).
	if last := lastMeaningfulLine(string(got)); last != "return config" {
		t.Errorf("expected `return config` to stay last, got %q\n---\n%s", last, got)
	}
}

// TestInstallComplexReturn covers a config that returns a non-identifier
// expression: we can't merge keybindings, but we must still register cst's
// handlers and keep the file valid (return stays last).
func TestInstallComplexReturn(t *testing.T) {
	dir := t.TempDir()
	weztermLua := filepath.Join(dir, ".wezterm.lua")
	original := `local wezterm = require 'wezterm'
return wezterm.config_builder()
`
	_ = os.WriteFile(weztermLua, []byte(original), 0o644)

	opts := Options{
		WeztermLuaPath: weztermLua,
		CstModulePath:  filepath.Join(dir, "cst.lua"),
	}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(weztermLua)
	if !strings.Contains(string(got), "require 'cst'") {
		t.Error("expected require 'cst' for handler registration")
	}
	if last := lastMeaningfulLine(string(got)); last != "return wezterm.config_builder()" {
		t.Errorf("expected complex return to stay last, got %q\n---\n%s", last, got)
	}
}

// TestInstallNoReturnAppendsConfig: a user file with no top-level return (which
// would otherwise yield nil) gets a self-contained config appended.
func TestInstallNoReturnAppendsConfig(t *testing.T) {
	dir := t.TempDir()
	weztermLua := filepath.Join(dir, ".wezterm.lua")
	original := `local wezterm = require 'wezterm'
wezterm.log_info('hi')
`
	_ = os.WriteFile(weztermLua, []byte(original), 0o644)

	opts := Options{
		WeztermLuaPath: weztermLua,
		CstModulePath:  filepath.Join(dir, "cst.lua"),
	}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(weztermLua)
	if !strings.Contains(string(got), "wezterm.log_info('hi')") {
		t.Error("user content was clobbered")
	}
	if last := lastMeaningfulLine(string(got)); last != "return cst_config" {
		t.Errorf("expected appended config return, got %q\n---\n%s", last, got)
	}
}

func TestInstallIdempotent(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		WeztermLuaPath: filepath.Join(dir, ".wezterm.lua"),
		CstModulePath:  filepath.Join(dir, "cst.lua"),
	}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(opts.WeztermLuaPath)

	changed, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second install should be no-op")
	}
	second, _ := os.ReadFile(opts.WeztermLuaPath)
	if string(first) != string(second) {
		t.Error("idempotent install changed file")
	}
	// BlockStart should appear exactly once.
	if strings.Count(string(second), BlockStart) != 1 {
		t.Errorf("BlockStart appears %d times", strings.Count(string(second), BlockStart))
	}
}

func TestInstallIdempotentWithExistingConfig(t *testing.T) {
	dir := t.TempDir()
	weztermLua := filepath.Join(dir, ".wezterm.lua")
	original := `local wezterm = require 'wezterm'
local config = wezterm.config_builder()
return config
`
	_ = os.WriteFile(weztermLua, []byte(original), 0o644)
	opts := Options{
		WeztermLuaPath: weztermLua,
		CstModulePath:  filepath.Join(dir, "cst.lua"),
	}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(weztermLua)
	changed, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second install over existing config should be no-op")
	}
	second, _ := os.ReadFile(weztermLua)
	if string(first) != string(second) {
		t.Errorf("idempotent install changed file:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if strings.Count(string(second), BlockStart) != 1 {
		t.Errorf("BlockStart appears %d times", strings.Count(string(second), BlockStart))
	}
}

func TestUninstallRemovesBlockAndFile(t *testing.T) {
	dir := t.TempDir()
	weztermLua := filepath.Join(dir, ".wezterm.lua")
	original := `local wezterm = require 'wezterm'
local config = wezterm.config_builder()
return config
`
	_ = os.WriteFile(weztermLua, []byte(original), 0o644)
	opts := Options{
		WeztermLuaPath: weztermLua,
		CstModulePath:  filepath.Join(dir, "cst.lua"),
	}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}

	changed, err := Uninstall(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected changed on uninstall")
	}
	got, _ := os.ReadFile(opts.WeztermLuaPath)
	if strings.Contains(string(got), BlockStart) {
		t.Error("block not removed from wezterm.lua")
	}
	// The user's original config must survive uninstall intact.
	if strings.TrimSpace(string(got)) != strings.TrimSpace(original) {
		t.Errorf("uninstall did not restore original config:\n%s", got)
	}
	if _, err := os.Stat(opts.CstModulePath); !os.IsNotExist(err) {
		t.Errorf("cst.lua not removed: %v", err)
	}

	// Uninstall again: no-op.
	changed, err = Uninstall(opts)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second uninstall should be no-op")
	}
}

func TestStripBlock(t *testing.T) {
	in := "before\n" + BlockStart + "\nstuff\n" + BlockEnd + "\nafter\n"
	got := stripBlock(in)
	if got != "before\nafter\n" {
		t.Errorf("got %q", got)
	}
}
