package rootfs

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A junction (directory reparse point) swapped in for a folder must not carry
// writes, trashes or reads outside the root.
func TestJunctionCannotRedirect(t *testing.T) {
	root, outside, id := enrolled(t)
	link := filepath.Join(root, "notes")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v: %s", err, out)
	}
	f := open(t, root, id)
	if err := f.WriteFile("notes/victim.txt", []byte("pwned"), time.Time{}); err == nil {
		t.Fatal("write through a junction succeeded")
	}
	if err := f.WriteFile("notes/new.txt", []byte("pwned"), time.Time{}); err == nil {
		t.Fatal("create through a junction succeeded")
	}
	if err := f.Trash("notes/victim.txt"); err == nil {
		t.Fatal("trash through a junction succeeded")
	}
	if _, err := f.ReadFile("notes/victim.txt"); err == nil {
		t.Fatal("read through a junction succeeded")
	}
	assertOutsideUntouched(t, outside)
}
