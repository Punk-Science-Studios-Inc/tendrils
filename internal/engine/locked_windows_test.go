package engine

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/rootfs"
)

func lockExclusive(t *testing.T, abs string) syscall.Handle {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func shortRenameRetries(t *testing.T, attempts int) {
	attemptsWas, backoffWas := rootfs.RenameAttempts, rootfs.RenameBackoff
	rootfs.RenameAttempts, rootfs.RenameBackoff = attempts, 20*time.Millisecond
	t.Cleanup(func() { rootfs.RenameAttempts, rootfs.RenameBackoff = attemptsWas, backoffWas })
}

// A lock released within the retry window does not fail the pull.
func TestBriefSharingViolationIsWaitedOut(t *testing.T) {
	shortRenameRetries(t, 6)
	eng, root, ev, bl := syncedFile(t, "a.md", "v1", t0)
	seedRemote(t, ev, bl, eng.id, "a.md", "v2", t0.Add(time.Minute))
	h := lockExclusive(t, filepath.Join(root, "a.md"))
	go func() {
		time.Sleep(50 * time.Millisecond)
		syscall.CloseHandle(h)
	}()

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got, _ := readFile(t, root, "a.md"); got != "v2" {
		t.Errorf("a.md = %q, want v2", got)
	}
}

// A lock that outlasts the retries leaves the original, keeps the verified
// download journaled, and the pull completes once the lock is gone.
func TestPersistentSharingViolationStaysPending(t *testing.T) {
	shortRenameRetries(t, 2)
	eng, root, ev, bl := syncedFile(t, "a.md", "v1", t0)
	seedRemote(t, ev, bl, eng.id, "a.md", "v2", t0.Add(time.Minute))
	h := lockExclusive(t, filepath.Join(root, "a.md"))

	if err := eng.Sync(context.Background()); err == nil {
		t.Fatal("sync reported success while the destination was locked")
	}
	syscall.CloseHandle(h)
	if got, _ := readFile(t, root, "a.md"); got != "v1" {
		t.Errorf("a.md = %q, want the original", got)
	}
	if ops, _ := eng.idx.Journal(); len(ops) != 1 {
		t.Fatalf("journal = %v, want the pull held", ops)
	}
	assertBase(t, eng.idx, "a.md", "v1")

	eng.idx.ClearRetry("a.md")
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync after unlock: %v", err)
	}
	if got, _ := readFile(t, root, "a.md"); got != "v2" {
		t.Errorf("a.md = %q, want v2 once unlocked", got)
	}
	assertBase(t, eng.idx, "a.md", "v2")
}
