//go:build linux && mountintegration

// Real-mount scenarios. They need CAP_SYS_ADMIN, so they are opt-in; the
// harness is a throwaway user+mount namespace, needing no real root:
//
//	unshare --user --map-root-user --mount go test -tags mountintegration -run Mount ./internal/engine
package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/rootid"
)

func mountTmpfs(t *testing.T, dir string) { mountTmpfsSized(t, dir, "8m") }

func mountTmpfsSized(t *testing.T, dir, size string) {
	t.Helper()
	if err := syscall.Mount("tendrils-test", dir, "tmpfs", 0, "size="+size); err != nil {
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

// A full disk while staging a download leaves the original, no journal record
// and no staging file: nothing was promised, so nothing needs recovering.
func TestMountDiskFullWhileStaging(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drive")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mountTmpfsSized(t, root, "1m")
	ev, bl := newFakeEvents(), newFakeBlobs()
	writeFile(t, root, "a.md", "v1", t0)
	eng := newEngine(t, root, mustID(t), ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, ev, bl, eng.id, "a.md", strings.Repeat("x", 2<<20), t0.Add(time.Minute))

	err := eng.Sync(context.Background())
	if err == nil || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("sync = %v, want ENOSPC", err)
	}
	if got, _ := readFile(t, root, "a.md"); got != "v1" {
		t.Errorf("a.md = %.20q, want the original", got)
	}
	if ops, _ := eng.idx.Journal(); len(ops) != 0 {
		t.Errorf("journal = %v, want nothing recorded", ops)
	}
	assertBase(t, eng.idx, "a.md", "v1")
	assertNoStagingIn(t, root)
}

// A full disk while preserving the losing version leaves the original and the
// verified download in place; the pull finishes once there is room.
func TestMountDiskFullWhilePreserving(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drive")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mountTmpfsSized(t, root, "1m")
	ev, bl := newFakeEvents(), newFakeBlobs()
	writeFile(t, root, "a.md", "v1", t0)
	eng := newEngine(t, root, mustID(t), ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	loser := strings.Repeat("l", 400<<10)
	winner := strings.Repeat("w", 400<<10)
	writeFile(t, root, "a.md", loser, t0.Add(10*time.Second))
	seedRemote(t, ev, bl, eng.id, "a.md", winner, t0.Add(time.Minute))

	if err := eng.Sync(context.Background()); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("sync = %v, want ENOSPC", err)
	}
	if got, _ := readFile(t, root, "a.md"); got != loser {
		t.Error("the losing version was replaced before it was preserved")
	}
	if ops, _ := eng.idx.Journal(); len(ops) != 1 {
		t.Fatalf("journal = %v, want the pull held", ops)
	}

	if err := syscall.Mount("tendrils-test", root, "tmpfs", syscall.MS_REMOUNT, "size=4m"); err != nil {
		t.Fatal(err)
	}
	eng.idx.ClearRetry("a.md")
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync with room: %v", err)
	}
	if got, _ := readFile(t, root, "a.md"); got != winner {
		t.Error("a.md is not the winner once there was room")
	}
	if got, _ := readFile(t, root, onlyConflictCopy(t, root, "a.md")); got != loser {
		t.Error("conflict copy does not hold the loser")
	}
	assertNoStagingIn(t, root)
}
