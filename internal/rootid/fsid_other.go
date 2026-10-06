//go:build !linux && !windows

package rootid

// FilesystemID has no portable source on other platforms; an empty identity is
// recorded and never compared, leaving the marker as the only check.
func FilesystemID(string) (string, error) { return "", nil }
