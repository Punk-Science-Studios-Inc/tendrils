package rootfs

import (
	"errors"
	"syscall"
)

// Windows cannot flush a directory handle; NTFS journals the rename itself, but
// without write-through a power cut can still lose it. See AGENTS.md.
const dirSyncSupported = false

const (
	errorSharingViolation syscall.Errno = 32
	errorLockViolation    syscall.Errno = 33
)

// isSharingViolation reports another process holding the file open in a way
// that forbids the rename. ERROR_ACCESS_DENIED is included because that is what
// MoveFileEx reports for a destination open without delete sharing.
func isSharingViolation(err error) bool {
	return errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation) || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
