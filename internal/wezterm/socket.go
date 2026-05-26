package wezterm

import (
	"os"
	"strconv"
)

// MuxSocket returns the wezterm mux socket path from the current process env.
// Empty string if the caller is not running inside a wezterm pane.
func MuxSocket() string {
	return os.Getenv("WEZTERM_UNIX_SOCKET")
}

// PaneID returns the current pane ID from $WEZTERM_PANE, or 0 if not set.
func PaneID() int64 {
	s := os.Getenv("WEZTERM_PANE")
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// InWezterm reports whether the current process is running inside a wezterm pane.
func InWezterm() bool {
	return MuxSocket() != "" && PaneID() != 0
}
