//go:build unix

package scan

import (
	"io/fs"
	"syscall"
)

// sameDevice reports whether two entries live on the same filesystem, which is
// how a directory that is really a mount point is told apart from an ordinary one.
func sameDevice(a, b fs.FileInfo) bool {
	sa, okA := a.Sys().(*syscall.Stat_t)
	sb, okB := b.Sys().(*syscall.Stat_t)
	if !okA || !okB {
		return true
	}
	return uint64(sa.Dev) == uint64(sb.Dev)
}
