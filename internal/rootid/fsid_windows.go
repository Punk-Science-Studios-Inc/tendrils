package rootid

import (
	"fmt"
	"syscall"
)

// FilesystemID is the serial number of the volume holding path.
func FilesystemID(path string) (string, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", fmt.Errorf("rootid: %s: %w", path, err)
	}
	h, err := syscall.CreateFile(p, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", fmt.Errorf("rootid: open %s: %w", path, err)
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return "", fmt.Errorf("rootid: volume of %s: %w", path, err)
	}
	return fmt.Sprintf("windows-volume:%08x", info.VolumeSerialNumber), nil
}
