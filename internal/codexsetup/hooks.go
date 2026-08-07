// Package codexsetup installs CST lifecycle hooks into the user-level Codex
// hooks.json without overwriting hooks owned by the user or other tools.
package codexsetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const description = "CST coding-session lifecycle tracking hooks"

type Options struct {
	Path       string
	Executable string
}

type Result struct {
	Path    string
	Changed bool
}

type hookSpec struct {
	event   string
	subcmd  string
	timeout int
}

var hookSpecs = []hookSpec{
	{event: "SessionStart", subcmd: "session-start", timeout: 5},
	{event: "UserPromptSubmit", subcmd: "prompt", timeout: 5},
	{event: "SessionEnd", subcmd: "session-end", timeout: 3},
}

// DefaultPath returns $CODEX_HOME/hooks.json, falling back to
// ~/.codex/hooks.json when CODEX_HOME is unset.
func DefaultPath() string {
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		return filepath.Join(dir, "hooks.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".codex", "hooks.json")
}

// Install merges CST's Codex hooks into hooks.json. Existing non-CST hooks and
// unknown top-level fields are preserved.
func Install(opts Options) (Result, error) {
	path, executable, err := resolveOptions(opts)
	if err != nil {
		return Result{}, err
	}
	root, before, err := readRoot(path)
	if err != nil {
		return Result{}, err
	}
	if _, ok := root["description"]; !ok {
		root["description"] = description
	}
	hooks, err := hooksObject(root)
	if err != nil {
		return Result{}, err
	}
	for _, spec := range hookSpecs {
		groups, err := eventGroups(hooks, spec.event)
		if err != nil {
			return Result{}, err
		}
		groups = removeCSTHandlers(groups)
		groups = append(groups, generatedGroup(executable, spec))
		hooks[spec.event] = groups
	}
	root["hooks"] = hooks
	return writeIfChanged(path, root, before)
}

// Uninstall removes only CST-owned Codex hook handlers.
func Uninstall(opts Options) (Result, error) {
	path := opts.Path
	if path == "" {
		path = DefaultPath()
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return Result{Path: path}, nil
	} else if err != nil {
		return Result{}, fmt.Errorf("stat Codex hooks: %w", err)
	}
	root, before, err := readRoot(path)
	if err != nil {
		return Result{}, err
	}
	hooks, err := hooksObject(root)
	if err != nil {
		return Result{}, err
	}
	for _, spec := range hookSpecs {
		groups, err := eventGroups(hooks, spec.event)
		if err != nil {
			return Result{}, err
		}
		groups = removeCSTHandlers(groups)
		if len(groups) == 0 {
			delete(hooks, spec.event)
		} else {
			hooks[spec.event] = groups
		}
	}
	root["hooks"] = hooks
	return writeIfChanged(path, root, before)
}

// Render returns the standalone CST hooks document used by --print.
func Render(executable string) ([]byte, error) {
	_, executable, err := resolveOptions(Options{Path: "hooks.json", Executable: executable})
	if err != nil {
		return nil, err
	}
	root := map[string]any{"description": description}
	hooks := map[string]any{}
	for _, spec := range hookSpecs {
		hooks[spec.event] = []any{generatedGroup(executable, spec)}
	}
	root["hooks"] = hooks
	return marshalRoot(root)
}

func resolveOptions(opts Options) (path, executable string, err error) {
	path = opts.Path
	if path == "" {
		path = DefaultPath()
	}
	executable = opts.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return "", "", fmt.Errorf("locate cst executable: %w", err)
		}
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", "", fmt.Errorf("resolve cst executable: %w", err)
	}
	return path, executable, nil
}

func readRoot(path string) (map[string]any, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read Codex hooks: %w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("parse Codex hooks: %w", err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, data, nil
}

func hooksObject(root map[string]any) (map[string]any, error) {
	value, ok := root["hooks"]
	if !ok || value == nil {
		return map[string]any{}, nil
	}
	hooks, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("codex hooks field must be an object")
	}
	return hooks, nil
}

func eventGroups(hooks map[string]any, event string) ([]any, error) {
	value, ok := hooks[event]
	if !ok || value == nil {
		return nil, nil
	}
	groups, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("codex hooks.%s must be an array", event)
	}
	return groups, nil
}

func removeCSTHandlers(groups []any) []any {
	result := make([]any, 0, len(groups))
	for _, value := range groups {
		group, ok := value.(map[string]any)
		if !ok {
			result = append(result, value)
			continue
		}
		handlerValue, ok := group["hooks"]
		if !ok {
			result = append(result, value)
			continue
		}
		handlers, ok := handlerValue.([]any)
		if !ok {
			result = append(result, value)
			continue
		}
		kept := make([]any, 0, len(handlers))
		for _, handlerValue := range handlers {
			handler, ok := handlerValue.(map[string]any)
			if !ok || !isCSTCommand(handler["command"]) {
				kept = append(kept, handlerValue)
			}
		}
		if len(kept) == 0 {
			continue
		}
		group["hooks"] = kept
		result = append(result, group)
	}
	return result
}

func isCSTCommand(value any) bool {
	command, ok := value.(string)
	if !ok || !strings.HasSuffix(strings.TrimSpace(command), "--provider codex") {
		return false
	}
	for _, spec := range hookSpecs {
		if strings.Contains(command, " hook "+spec.subcmd+" ") {
			return true
		}
	}
	return false
}

func generatedGroup(executable string, spec hookSpec) map[string]any {
	command := shellQuote(executable) + " hook " + spec.subcmd + " --provider codex"
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
				"timeout": spec.timeout,
			},
		},
	}
}

func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`;&|<>(){}[]*?!") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func writeIfChanged(path string, root map[string]any, before []byte) (Result, error) {
	after, err := marshalRoot(root)
	if err != nil {
		return Result{}, err
	}
	if bytes.Equal(before, after) {
		return Result{Path: path}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Result{}, fmt.Errorf("create Codex config directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cst-codex-hooks-*")
	if err != nil {
		return Result{}, err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(after); err != nil {
		_ = tmp.Close()
		cleanup()
		return Result{}, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return Result{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return Result{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return Result{}, err
	}
	return Result{Path: path, Changed: true}, nil
}

func marshalRoot(root map[string]any) ([]byte, error) {
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal Codex hooks: %w", err)
	}
	return append(data, '\n'), nil
}
