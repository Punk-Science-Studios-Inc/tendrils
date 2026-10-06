//go:build !unix

package scan

import "io/fs"

// sameDevice is always true here: on Windows a mounted folder is a reparse point,
// which the walk reports as an unsupported entry and never descends into.
func sameDevice(fs.FileInfo, fs.FileInfo) bool { return true }
