package wezsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

	// .wezterm.lua should have the loader block.
	got, _ := os.ReadFile(opts.WeztermLuaPath)
	if !strings.Contains(string(got), BlockStart) {
		t.Error("wezterm.lua missing BlockStart")
	}
	if !strings.Contains(string(got), "require 'cst'") {
		t.Error("wezterm.lua missing require")
	}

	// cst.lua should have the full module.
	mod, _ := os.ReadFile(opts.CstModulePath)
	if !strings.Contains(string(mod), "wezterm.on('gui-startup'") {
		t.Error("cst.lua missing gui-startup handler")
	}
	if !strings.Contains(string(mod), "new-tab-button-click") {
		t.Error("cst.lua missing new-tab-button-click handler")
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

func TestUninstallRemovesBlockAndFile(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		WeztermLuaPath: filepath.Join(dir, ".wezterm.lua"),
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
