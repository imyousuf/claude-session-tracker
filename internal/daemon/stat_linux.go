//go:build linux || darwin

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

func statSysOwner(info os.FileInfo) (uint32, error) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected FileInfo.Sys type %T", info.Sys())
	}
	return sys.Uid, nil
}
