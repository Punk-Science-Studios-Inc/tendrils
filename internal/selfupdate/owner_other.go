//go:build !unix

package selfupdate

// preserveOwner is a no-op where there is no uid/gid to preserve. Windows
// inherits ACLs from the destination directory, which is the behaviour an owner
// expects there.
func preserveOwner(installed, staged string) {}
