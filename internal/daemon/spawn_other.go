//go:build !linux && !darwin

package daemon

import (
	"fmt"
	"os"
)

func spawnDetachedExec(_ string, _ []string, _ *os.File) error {
	return fmt.Errorf("daemon auto-spawn not implemented on this platform")
}
