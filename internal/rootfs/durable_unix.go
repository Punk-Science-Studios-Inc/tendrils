//go:build !windows

package rootfs

// A directory fsync makes a rename in it durable on Linux and the BSDs.
const dirSyncSupported = true

func isSharingViolation(error) bool { return false }
