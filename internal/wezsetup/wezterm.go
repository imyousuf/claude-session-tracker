// Package wezsetup installs cst's wezterm Lua integration.
//
// Layout:
//   - The full event-handler module is written to ~/.config/wezterm/cst.lua.
//   - A loader block is woven into ~/.wezterm.lua (between markers) that requires
//     the module (registering its event handlers) and merges cst's keybindings
//     into the wezterm config.
//
// Crucially, ~/.wezterm.lua must always `return` a wezterm Config object —
// returning nil makes wezterm fail with "Cannot convert Null to Config". So the
// loader block is responsible for ensuring a config is returned:
//
//   - No existing config  -> the block builds a config_builder(), applies cst,
//     and returns it.
//   - Existing `return X`  -> the block is inserted *before* that return; if X is
//     where X is an         a simple identifier we call apply_to_config(X) so the
//     identifier            keybindings merge into the user's own config.
//   - Existing `return`    -> we can't reference the returned value, so we only
//     of a complex expr     `require 'cst'` (event handlers still register); the
//     user's config is returned unchanged.
//
// Both files are idempotent: re-running setup replaces the loader block and
// rewrites cst.lua. Uninstall removes both.
package wezsetup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	BlockStart = "-- >>> cst integration (do not edit) >>>"
	BlockEnd   = "-- <<< cst integration <<<"
)

// loaderPath puts ~/.config/wezterm on Lua's package path so `require 'cst'`
// resolves regardless of where wezterm was launched from.
const loaderPath = `package.path = package.path .. ';' .. (os.getenv('HOME') or '') .. '/.config/wezterm/?.lua'`

// selfContainedLoader is the loader body used when the user has no existing
// wezterm config: it builds a config, applies cst's keybindings, and returns it.
// This is what guarantees ~/.wezterm.lua returns a valid Config (and not nil).
const selfContainedLoader = loaderPath + `
local wezterm = require 'wezterm'
local cst_config = wezterm.config_builder()
require('cst').apply_to_config(cst_config)
return cst_config`

// LoaderSnippet is the canonical loader shown by ` + "`cst setup-wezterm --print`" + `.
// Install() picks the right variant per the existing config; this is the
// no-existing-config form.
const LoaderSnippet = selfContainedLoader

// identRe matches a bare Lua identifier (the common `return config` case).
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RenderLuaModule produces the content of ~/.config/wezterm/cst.lua, pinning
// the absolute path to the cst binary so wezterm's background_child_process
// doesn't depend on the wezterm GUI process inheriting the right $PATH (a
// common silent failure when wezterm is launched from a desktop entry).
//
// Requiring the module registers the event handlers (snapshot + restore) as a
// side effect. apply_to_config(config) merges cst's keybindings into a config.
//
// Also emits wezterm.log_info(...) on module load and inside each handler so
// users can verify wiring via wezterm's debug overlay (Ctrl+Shift+L).
func RenderLuaModule(cstBinary string) string {
	if cstBinary == "" {
		cstBinary = "cst"
	}
	return fmt.Sprintf(luaModuleTemplate, cstBinary)
}

const luaModuleTemplate = `-- ~/.config/wezterm/cst.lua — installed by ` + "`cst setup-wezterm`" + `.
-- Requiring this module registers wezterm event handlers (snapshot + restore)
-- as a side effect. Call require('cst').apply_to_config(config) to merge cst's
-- keybindings into your wezterm config; the loader block in ~/.wezterm.lua does
-- this for you.

local wezterm = require 'wezterm'
local mux = wezterm.mux

-- Absolute path to cst, pinned by setup-wezterm. Avoids PATH-inheritance
-- pitfalls when wezterm is launched from a .desktop entry.
local CST = %q

wezterm.log_info('cst.lua loaded; binary=' .. CST)

local function snap(reason)
  wezterm.log_info('cst snap: ' .. (reason or 'unknown'))
  wezterm.background_child_process({ CST, 'snapshot' })
end

-- 1. Restore on launch.
--    If wezterm was started with an explicit command (wezterm start -- prog),
--    honor it and skip restore. Otherwise rebuild the saved layout; restore
--    opens a default window itself when there's no snapshot (--spawn-if-empty),
--    so we must NOT pre-spawn one here (that would drop the first saved pane —
--    e.g. a claude session — and leave a stray blank window).
wezterm.on('gui-startup', function(cmd)
  wezterm.log_info('cst gui-startup')
  wezterm.background_child_process({ CST, 'daemon' })
  if cmd then
    mux.spawn_window(cmd)
    return
  end
  wezterm.background_child_process({ CST, 'restore', '--spawn-if-empty' })
end)

-- 2. Tab creation via the "+" button in the tab bar.
wezterm.on('new-tab-button-click', function(window, pane, button, default_action)
  snap('new-tab-button-click')
  return default_action
end)

-- 3. Pane / tab close (X button, process exit, keyboard close, etc.).
--    mux-is-process-stateful fires right before wezterm kills a pane.
wezterm.on('mux-is-process-stateful', function(proc)
  snap('mux-is-process-stateful')
  return nil
end)

-- 4. Focus changes — catches stragglers like a pane created by
--    ` + "`wezterm cli spawn`" + ` that the user just clicked onto.
wezterm.on('window-focus-changed', function()
  snap('window-focus-changed')
end)

-- Keybinding wrappers (snapshot before any user-initiated split/spawn/close).
local act = wezterm.action

local function snap_then(action, reason)
  return act.Multiple {
    action,
    wezterm.action_callback(function() snap(reason) end),
  }
end

local M = {}

M.keys = {
  { key = 't', mods = 'CTRL|SHIFT', action = snap_then(act.SpawnTab 'CurrentPaneDomain', 'key:spawn-tab') },
  { key = 'w', mods = 'CTRL|SHIFT', action = snap_then(act.CloseCurrentTab { confirm = true }, 'key:close-tab') },
  { key = 'd', mods = 'CTRL|SHIFT', action = snap_then(act.SplitHorizontal { domain = 'CurrentPaneDomain' }, 'key:split-h') },
  { key = 'D', mods = 'CTRL|SHIFT', action = snap_then(act.SplitVertical   { domain = 'CurrentPaneDomain' }, 'key:split-v') },
}

M.snap = snap

-- apply_to_config appends cst's keybindings to a wezterm config (a
-- config_builder() result or a plain table) and returns it for chaining.
-- A nil config is replaced with a fresh config_builder() so callers always get
-- a valid Config back.
function M.apply_to_config(config)
  if config == nil then
    config = wezterm.config_builder()
  end
  config.keys = config.keys or {}
  for _, k in ipairs(M.keys) do
    table.insert(config.keys, k)
  end
  return config
end

return M
`

// LuaModule is the rendered module with no binary path — kept for callers
// (and tests) that want the default. Setup callers should prefer
// RenderLuaModule with the discovered absolute path.
var LuaModule = RenderLuaModule("cst")

// Options configures the wezterm install.
type Options struct {
	// WeztermLua path; defaults to ~/.wezterm.lua.
	WeztermLuaPath string
	// CstModulePath; defaults to ~/.config/wezterm/cst.lua.
	CstModulePath string
	// CstBinaryPath is baked into the rendered cst.lua so wezterm's
	// background_child_process doesn't rely on $PATH. Defaults to the path
	// of the currently-running cst binary (via os.Executable).
	CstBinaryPath string
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
	if o.CstBinaryPath == "" {
		if exe, err := os.Executable(); err == nil {
			o.CstBinaryPath = exe
		} else {
			o.CstBinaryPath = "cst"
		}
	}
	return o
}

// Install writes the cst.lua module and weaves the loader block into the user's
// .wezterm.lua. Idempotent.
func Install(o Options) (changed bool, err error) {
	o = defaultedOptions(o)

	// 1. Write cst.lua (always overwrite — the module content is authoritative).
	if err := os.MkdirAll(filepath.Dir(o.CstModulePath), 0o755); err != nil {
		return false, fmt.Errorf("create module dir: %w", err)
	}
	moduleContent := RenderLuaModule(o.CstBinaryPath)
	existingModule, _ := os.ReadFile(o.CstModulePath)
	if string(existingModule) != moduleContent {
		if err := atomicWrite(o.CstModulePath, []byte(moduleContent), 0o644); err != nil {
			return false, fmt.Errorf("write cst.lua: %w", err)
		}
		changed = true
	}

	// 2. Weave loader block into .wezterm.lua so it always returns a Config.
	existing, err := readOrEmpty(o.WeztermLuaPath)
	if err != nil {
		return changed, fmt.Errorf("read wezterm config: %w", err)
	}
	updated := weaveLoader(stripBlock(existing))

	if updated == existing {
		return changed, nil
	}
	if err := atomicWrite(o.WeztermLuaPath, []byte(updated), 0o644); err != nil {
		return changed, fmt.Errorf("write wezterm config: %w", err)
	}
	return true, nil
}

// weaveLoader produces the new ~/.wezterm.lua content from the user's existing
// content (already stripped of any prior cst block). The result is guaranteed
// to be syntactically valid and to return a wezterm Config.
func weaveLoader(stripped string) string {
	trimmed := strings.TrimSpace(stripped)

	// Case A: no user config — write a self-contained config.
	if trimmed == "" {
		return wrapBlock(selfContainedLoader) + "\n"
	}

	before, returnLine, after, found, ident := splitFinalReturn(stripped)
	if !found {
		// Case D: user config has no top-level return (it returned nil already,
		// or relied on side effects). Append a self-contained config so wezterm
		// gets a valid Config and cst's handlers register.
		return ensureTrailingNewline(strings.TrimRight(stripped, "\n")) +
			"\n" + wrapBlock(selfContainedLoader) + "\n"
	}

	var body string
	if ident != "" {
		// Case B: `return <ident>` — merge keybindings into the user's config.
		body = loaderPath + "\nrequire('cst').apply_to_config(" + ident + ")"
	} else {
		// Case C: `return <complex expr>` — we can't reference the value, so we
		// only register cst's event handlers. The user's config is returned
		// unchanged (still valid). Keybindings aren't merged in this case.
		body = loaderPath + "\nrequire 'cst'"
	}

	// Insert the block *before* the return so the return stays last (valid Lua)
	// and so the block is removable on re-install (idempotent).
	out := ""
	if strings.TrimSpace(before) != "" {
		out += ensureTrailingNewline(strings.TrimRight(before, "\n")) + "\n"
	}
	out += wrapBlock(body) + "\n"
	out += returnLine
	if strings.TrimRight(after, "\n") != "" {
		out += "\n" + strings.TrimRight(after, "\n")
	}
	return ensureTrailingNewline(out)
}

// wrapBlock surrounds a loader body with the cst markers.
func wrapBlock(body string) string {
	return BlockStart + "\n" + ensureTrailingNewline(body) + BlockEnd
}

// splitFinalReturn finds the last top-level `return` statement in the chunk.
// It returns the content before that line, the return line itself, any trailing
// content after it (comments/blank lines), whether a return was found, and — if
// the returned expression is a bare identifier — that identifier.
//
// Heuristic: scan from the bottom past blank and comment-only lines; the first
// real line must start with `return` to count. A trailing `end`/`}` (i.e. a
// return buried inside a function body) is correctly treated as "no top-level
// return".
func splitFinalReturn(s string) (before, returnLine, after string, found bool, ident string) {
	lines := strings.Split(s, "\n")
	idx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		if t == "return" || strings.HasPrefix(t, "return ") || strings.HasPrefix(t, "return\t") {
			idx = i
		}
		break
	}
	if idx == -1 {
		return s, "", "", false, ""
	}

	before = strings.Join(lines[:idx], "\n")
	returnLine = lines[idx]
	after = strings.Join(lines[idx+1:], "\n")

	expr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(returnLine), "return"))
	// Drop a trailing inline comment, e.g. `return config -- my config`.
	if c := strings.Index(expr, "--"); c >= 0 {
		expr = strings.TrimSpace(expr[:c])
	}
	if identRe.MatchString(expr) {
		ident = expr
	}
	return before, returnLine, after, true, ident
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
