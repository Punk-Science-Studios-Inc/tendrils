//go:build linux && mountintegration

// Real-mount scenarios. They need CAP_SYS_ADMIN, so they are opt-in; the
// harness is a throwaway user+mount namespace, needing no real root:
//
//	unshare --user --map-root-user --mount go test -tags mountintegration -run Mount ./internal/engine
package engine

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"ca.punkscience.tendrils/internal/rootid"
)

func mountTmpfs(t *testing.T, dir string) {
	t.Helper()
	if err := syscall.Mount("tendrils-test", dir, "tmpfs", 0, "size=8m"); err != nil {
		t.Skipf("cannot mount (run under the documented unshare harness): %v", err)
	}
	t.Cleanup(func() { syscall.Unmount(dir, syscall.MNT_DETACH) })
}

// The real thing the root marker exists for: a drive unmounted from under an
// enrolled root leaves the empty mountpoint at the configured path.
func TestMountUnmountedRootPausesWithoutTombstones(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drive")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mountTmpfs(t, root)
	writeFile(t, root, "a.md", "a", t0)
	writeFile(t, root, "notes/b.md", "b", t0)

	ev, bl := newFakeEvents(), newFakeBlobs()
	eng := newEngine(t, root, mustID(t), ev, bl)
	var stats Stats
	eng.OnStats(func(s Stats) { stats = s })
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Unmount(root, 0); err != nil {
		t.Fatal(err)
	}
	before := ev.publishes
	assertPaused(t, eng.Sync(context.Background()), &stats, rootid.ErrNoMarker)
	if ev.publishes != before || len(tombstones(t, ev)) != 0 {
		t.Fatal("an unmounted drive's mountpoint was synced as deletions")
	}
}

// A filesystem mounted inside the root is held back, and stays held back once
// unmounted, so its previously-visible files never read as deleted.
func TestMountSubordinateMountIsGuarded(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.md", "a", t0)
	sub := filepath.Join(root, "media")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mountTmpfs(t, sub)
	writeFile(t, root, "media/film.mkv", "f", t0)

	ev, bl := newFakeEvents(), newFakeBlobs()
	eng := newEngine(t, root, mustID(t), ev, bl)
	var stats Stats
	eng.OnStats(func(s Stats) { stats = s })
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := ev.byPath["media/film.mkv"]; ok {
		t.Fatal("crossed into a subordinate mount")
	}
	if stats.Unavailable != 1 {
		t.Errorf("unavailable = %d, want the mount reported", stats.Unavailable)
	}

	if err := syscall.Unmount(sub, 0); err != nil {
		t.Fatal(err)
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := tombstones(t, ev); len(got) != 0 {
		t.Fatalf("tombstoned %v after the subordinate mount went away", got)
	}
	if stats.Unavailable != 1 {
		t.Errorf("unavailable = %d after unmount, want the boundary still held", stats.Unavailable)
	}
}
