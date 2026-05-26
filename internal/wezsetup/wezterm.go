// Package wezsetup installs cst's wezterm Lua integration.
//
// Layout:
//   - The full event-handler module is written to ~/.config/wezterm/cst.lua.
//   - A short loader block is appended to ~/.wezterm.lua (between markers).
//
// Both files are idempotent: re-running setup replaces the loader block and
// rewrites cst.lua. Uninstall removes both.
package wezsetup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	BlockStart = "-- >>> cst integration (do not edit) >>>"
	BlockEnd   = "-- <<< cst integration <<<"
)

// LoaderSnippet is the short stub appended to the user's ~/.wezterm.lua.
// It puts ~/.config/wezterm on Lua's package path and requires our module,
// which contains all the actual event handlers.
const LoaderSnippet = `package.path = package.path .. ';' .. (os.getenv('HOME') or '') .. '/.config/wezterm/?.lua'
require 'cst'
`

// LuaModule is the content of ~/.config/wezterm/cst.lua. Contains all the
// event handlers and keybinding wrappers that drive snapshots + restore.
const LuaModule = `-- ~/.config/wezterm/cst.lua — installed by ` + "`cst setup-wezterm`" + `.
-- All event handlers below push a single event to the cst daemon (over its
-- per-user Unix socket) and return immediately. The daemon does the snapshot
-- coalescing and DB writes.

local wezterm = require 'wezterm'
local mux = wezterm.mux

local function snap()
  wezterm.background_child_process({ 'cst', 'snapshot' })
end

-- 1. Restore on launch.
wezterm.on('gui-startup', function(cmd)
  -- Spawn the default window first so wezterm has something to show.
  local _, _, _ = mux.spawn_window(cmd or {})
  -- Auto-start the daemon if not already running (no-op if socket is bound).
  wezterm.background_child_process({ 'cst', 'daemon' })
  -- Reconstruct the saved layout. --skip-first avoids double-spawning
  -- alongside the default window above.
  wezterm.background_child_process({ 'cst', 'restore', '--skip-first' })
end)

-- 2. Tab creation via the "+" button in the tab bar.
wezterm.on('new-tab-button-click', function(window, pane, button, default_action)
  snap()
  return default_action
end)

-- 3. Pane / tab close (X button, process exit, keyboard close, etc.).
--    mux-is-process-stateful fires right before wezterm kills a pane.
wezterm.on('mux-is-process-stateful', function(proc)
  snap()
  return nil
end)

-- 4. Focus changes — catches stragglers like a pane created by
--    ` + "`wezterm cli spawn`" + ` that the user just clicked onto.
wezterm.on('window-focus-changed', function()
  snap()
end)

-- Keybinding wrappers (snapshot before any user-initiated split/spawn/close).
-- These are appended to the user's existing keys via wezterm.action.Multiple,
-- so existing bindings keep working.
local act = wezterm.action

local function snap_then(action)
  return act.Multiple {
    action,
    wezterm.action_callback(function() snap() end),
  }
end

-- Export so the user's wezterm config can compose if they want:
--     local cst = require 'cst'
--     for _, k in ipairs(cst.keys) do table.insert(config.keys, k) end
return {
  keys = {
    { key = 't', mods = 'CTRL|SHIFT', action = snap_then(act.SpawnTab 'CurrentPaneDomain') },
    { key = 'w', mods = 'CTRL|SHIFT', action = snap_then(act.CloseCurrentTab { confirm = true }) },
    { key = 'd', mods = 'CTRL|SHIFT', action = snap_then(act.SplitHorizontal { domain = 'CurrentPaneDomain' }) },
    { key = 'D', mods = 'CTRL|SHIFT', action = snap_then(act.SplitVertical   { domain = 'CurrentPaneDomain' }) },
  },
  snap = snap,
}
`

// Options configures the wezterm install.
type Options struct {
	// WeztermLua path; defaults to ~/.wezterm.lua.
	WeztermLuaPath string
	// CstModulePath; defaults to ~/.config/wezterm/cst.lua.
	CstModulePath string
}

func defaultedOptions(o Options) Options {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	if o.WeztermLuaPath == "" {
		o.WeztermLuaPath = filepath.Join(home, ".wezterm.lua")
	}
	if o.CstModulePath == "" {
		o.CstModulePath = filepath.Join(home, ".config", "wezterm", "cst.lua")
	}
	return o
}

// Install writes the cst.lua module and appends the loader block to the user's
// .wezterm.lua. Idempotent.
func Install(o Options) (changed bool, err error) {
	o = defaultedOptions(o)

	// 1. Write cst.lua (always overwrite — the module content is authoritative).
	if err := os.MkdirAll(filepath.Dir(o.CstModulePath), 0o755); err != nil {
		return false, fmt.Errorf("create module dir: %w", err)
	}
	existingModule, _ := os.ReadFile(o.CstModulePath)
	if string(existingModule) != LuaModule {
		if err := atomicWrite(o.CstModulePath, []byte(LuaModule), 0o644); err != nil {
			return false, fmt.Errorf("write cst.lua: %w", err)
		}
		changed = true
	}

	// 2. Append loader block to .wezterm.lua.
	existing, err := readOrEmpty(o.WeztermLuaPath)
	if err != nil {
		return changed, fmt.Errorf("read wezterm config: %w", err)
	}
	stripped := stripBlock(existing)
	loader := fmt.Sprintf("\n%s\n%s%s\n", BlockStart, ensureTrailingNewline(LoaderSnippet), BlockEnd)
	updated := stripped + loader

	if updated == existing {
		return changed, nil
	}
	if err := atomicWrite(o.WeztermLuaPath, []byte(updated), 0o644); err != nil {
		return changed, fmt.Errorf("write wezterm config: %w", err)
	}
	return true, nil
}

// Uninstall removes the loader block from .wezterm.lua and deletes cst.lua.
func Uninstall(o Options) (changed bool, err error) {
	o = defaultedOptions(o)

	existing, err := readOrEmpty(o.WeztermLuaPath)
	if err != nil {
		return false, fmt.Errorf("read wezterm config: %w", err)
	}
	stripped := stripBlock(existing)
	if stripped != existing {
		if err := atomicWrite(o.WeztermLuaPath, []byte(stripped), 0o644); err != nil {
			return false, fmt.Errorf("write wezterm config: %w", err)
		}
		changed = true
	}

	if err := os.Remove(o.CstModulePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return changed, fmt.Errorf("remove cst.lua: %w", err)
	}
	return changed, nil
}

// PathsForDisplay returns the resolved install paths so the CLI can show users
// what was/would be written.
func PathsForDisplay(o Options) (weztermLua, cstModule string) {
	o = defaultedOptions(o)
	return o.WeztermLuaPath, o.CstModulePath
}

// --- helpers (kept local to avoid coupling to shellsetup) ---

func stripBlock(content string) string {
	startIdx := strings.Index(content, BlockStart)
	if startIdx < 0 {
		return content
	}
	rest := content[startIdx+len(BlockStart):]
	endRel := strings.Index(rest, BlockEnd)
	if endRel < 0 {
		return content
	}
	endIdx := startIdx + len(BlockStart) + endRel + len(BlockEnd)
	before := strings.TrimRight(content[:startIdx], "\n")
	after := strings.TrimLeft(content[endIdx:], "\n")
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

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cst-tmp-*")
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

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
