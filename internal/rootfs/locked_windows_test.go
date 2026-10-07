package rootfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A file another process holds open without delete sharing cannot be replaced
// on Windows. The commit fails, and the original stays as it was.
func TestLockedDestinationIsLeftInPlace(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	abs := filepath.Join(root, "locked.md")
	os.WriteFile(abs, []byte("held open"), 0o600)
	want := observe(t, abs)

	name, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)

	p, err := f.Create("locked.md")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Abort()
	p.Write([]byte("incoming"))
	if err := p.CommitIf(time.Time{}, want); err == nil {
		t.Fatal("replaced a file held open without delete sharing")
	}
	syscall.CloseHandle(h)
	if got, _ := os.ReadFile(abs); string(got) != "held open" {
		t.Errorf("locked.md = %q, want the original", got)
	}
}
