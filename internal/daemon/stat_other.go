//go:build !linux && !darwin

package daemon

import "os"

// On platforms without UNIX ownership semantics, skip the owner check.
func statSysOwner(_ os.FileInfo) (uint32, error) {
	return uint32(os.Getuid()), nil
}
