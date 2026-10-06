package rootid

import (
	"fmt"
	"syscall"
)

// FilesystemID is the statfs f_fsid of the filesystem holding path. ext4, btrfs
// and others derive it from the filesystem UUID, so it is stable across reboots;
// FAT, exFAT and XFS derive it from the device number, which can change when a
// removable drive is enumerated in a different order — adopt again if so.
func FilesystemID(path string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", fmt.Errorf("rootid: statfs %s: %w", path, err)
	}
	return fmt.Sprintf("linux-fsid:%08x%08x", uint32(st.Fsid.X__val[0]), uint32(st.Fsid.X__val[1])), nil
}
