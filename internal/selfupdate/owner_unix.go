//go:build unix

package selfupdate

import (
	"os"
	"syscall"
)

// preserveOwner copies the installed binary's uid/gid onto the staged one, so
// an upgrade run with sudo does not silently take ownership of a binary that
// belonged to the user. Best effort: an unprivileged process cannot chown, and
// that is the normal case, not a failure.
func preserveOwner(installed, staged string) {
	fi, err := os.Stat(installed)
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	_ = os.Chown(staged, int(st.Uid), int(st.Gid))
}
