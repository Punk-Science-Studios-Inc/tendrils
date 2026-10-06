//go:build unix

package scan

import (
	"os"
	"path/filepath"
	"testing"
)

// unreadable removes all permissions from a path for the rest of the test.
func unreadable(t *testing.T, root, rel string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions do not restrict root")
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, info.Mode().Perm()|0o700) })
}
