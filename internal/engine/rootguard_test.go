package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/ignore"
	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/rootid"
)

var t0 = time.Unix(1_700_000_000, 0)

// tombstones returns the paths the relay currently holds a deletion for.
func tombstones(t *testing.T, ev *fakeEvents) []string {
	t.Helper()
	var out []string
	for _, evt := range ev.byPath {
		ent, err := nostrevent.Parse(evt)
		if err != nil {
			t.Fatal(err)
		}
		if ent.Deleted {
			out = append(out, ent.Path)
		}
	}
	return out
}

// syncedTree enrolls a root holding files, syncs it once, and returns the
// engine with its stats captured.
func syncedTree(t *testing.T, files ...string) (eng *Engine, root string, ev *fakeEvents, stats *Stats) {
	t.Helper()
	ev, bl := newFakeEvents(), newFakeBlobs()
	root = filepath.Join(t.TempDir(), "vault")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		writeFile(t, root, f, "content of "+f, t0)
	}
	eng = newEngine(t, root, mustID(t), ev, bl)
	stats = &Stats{}
	eng.OnStats(func(s Stats) { *stats = s })
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	return eng, root, ev, stats
}

func assertPaused(t *testing.T, err error, stats *Stats, cause error) {
	t.Helper()
	var p *Paused
	if !errors.As(err, &p) {
		t.Fatalf("sync error = %v, want a pause", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("pause cause = %v, want %v", err, cause)
	}
	if stats.Paused == "" {
		t.Fatal("stats did not report the pause")
	}
}

func TestMissingRootPausesWithoutTombstones(t *testing.T) {
	eng, root, ev, stats := syncedTree(t, "a.md", "notes/b.md")
	if err := os.Rename(root, root+".away"); err != nil {
		t.Fatal(err)
	}
	before := ev.publishes

	assertPaused(t, eng.Sync(context.Background()), stats, rootid.ErrRootMissing)
	if ev.publishes != before || len(tombstones(t, ev)) != 0 {
		t.Fatalf("a missing root published %d events", ev.publishes-before)
	}
}

// An unmounted drive leaves its mountpoint: an empty directory at the
// configured path, indistinguishable by contents from a tree whose every file
// was deleted.
func TestEmptyMountpointPausesWithoutTombstones(t *testing.T) {
	eng, root, ev, stats := syncedTree(t, "a.md", "notes/b.md")
	if err := os.Rename(root, root+".unmounted"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	before := ev.publishes

	assertPaused(t, eng.Sync(context.Background()), stats, rootid.ErrNoMarker)
	if ev.publishes != before || len(tombstones(t, ev)) != 0 {
		t.Fatal("an empty mountpoint was read as a tree of deletions")
	}
}

// A different enrolled folder mounted in place — another device's root, a
// backup — carries a marker, but not this enrollment's.
func TestSubstitutedRootPauses(t *testing.T) {
	eng, root, ev, stats := syncedTree(t, "a.md")
	other := filepath.Join(t.TempDir(), "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := rootid.Establish(other); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+".away"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, root); err != nil {
		t.Fatal(err)
	}
	before := ev.publishes

	assertPaused(t, eng.Sync(context.Background()), stats, rootid.ErrWrongRoot)
	if ev.publishes != before {
		t.Fatal("a substituted root published")
	}
}

// The drive comes back: the pass resumes where it left off, with nothing
// republished and nothing tombstoned.
func TestReturningRootResumesWithoutRepublishing(t *testing.T) {
	eng, root, ev, stats := syncedTree(t, "a.md", "notes/b.md")
	if err := os.Rename(root, root+".away"); err != nil {
		t.Fatal(err)
	}
	_ = eng.Sync(context.Background())
	if err := os.Rename(root+".away", root); err != nil {
		t.Fatal(err)
	}
	before := ev.publishes

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync after the root returned: %v", err)
	}
	if ev.publishes != before {
		t.Fatalf("returning root republished %d events", ev.publishes-before)
	}
	if stats.Paused != "" || stats.Pending != 0 {
		t.Fatalf("stats after resume = %+v, want settled", *stats)
	}
}

// The guard must not cost the real thing: a deletion inside an observed root
// still propagates.
func TestRealDeletionStillPropagates(t *testing.T) {
	eng, root, ev, _ := syncedTree(t, "a.md", "b.md")
	if err := os.Remove(filepath.Join(root, "a.md")); err != nil {
		t.Fatal(err)
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := tombstones(t, ev); len(got) != 1 || got[0] != "a.md" {
		t.Fatalf("tombstones = %v, want [a.md]", got)
	}
}

func TestUnreadableSubtreeIsHeldBackNotDeleted(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions enforced against this user")
	}
	eng, root, ev, stats := syncedTree(t, "a.md", "locked/b.md", "locked/deep/c.md")
	writeFile(t, root, "new.md", "fresh", t0)
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("an unreadable subtree failed the pass: %v", err)
	}
	if got := tombstones(t, ev); len(got) != 0 {
		t.Fatalf("unreadable files were tombstoned: %v", got)
	}
	if _, ok := ev.byPath["new.md"]; !ok {
		t.Error("the readable rest of the tree stopped syncing")
	}
	if stats.Unavailable != 2 {
		t.Errorf("unavailable = %d, want the 2 known paths under the unreadable folder", stats.Unavailable)
	}
}

// A remote change under a folder that could not be read is not written: there
// is no telling what it would overwrite.
func TestRemoteChangeUnderUnreadableFolderIsNotWritten(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions enforced against this user")
	}
	eng, root, ev, stats := syncedTree(t, "locked/b.md")
	seedRemote(t, ev, eng.blobs, eng.id, "locked/new.md", "from elsewhere", t0.Add(time.Hour))
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.Chmod(locked, 0o755)
	if _, ok := readFile(t, root, "locked/new.md"); ok {
		t.Fatal("wrote into a folder the scan could not read")
	}
	if stats.Unavailable == 0 {
		t.Error("held-back remote change was not reported")
	}
}

// Excluded paths are never opened, so one this user cannot read costs nothing.
func TestExcludedUnreadablePathsAreNotTouched(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions enforced against this user")
	}
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "a.md", "a", t0)
	writeFile(t, root, "private/key.pem", "secret", t0)
	if err := os.Chmod(filepath.Join(root, "private"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "private"), 0o755) })

	eng := newEngine(t, root, mustID(t), ev, bl)
	eng.SetExclude([]string{"private/"})
	var stats Stats
	eng.OnStats(func(s Stats) { stats = s })
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats.Unavailable != 0 {
		t.Fatalf("an excluded folder was opened: unavailable = %d", stats.Unavailable)
	}
	if _, ok := ev.byPath["a.md"]; !ok {
		t.Fatal("tree did not sync")
	}
}

func TestUnreadableIgnoreFilePauses(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions enforced against this user")
	}
	eng, root, ev, stats := syncedTree(t, "a.md")
	writeFile(t, root, ignore.FileName, "music/\n", t0)
	writeFile(t, root, "music/song.flac", "audio", t0)
	if err := os.Chmod(filepath.Join(root, ignore.FileName), 0); err != nil {
		t.Fatal(err)
	}
	before := ev.publishes

	err := eng.Sync(context.Background())
	var p *Paused
	if !errors.As(err, &p) || stats.Paused == "" {
		t.Fatalf("sync = %v, stats = %+v, want a pause", err, *stats)
	}
	if ev.publishes != before {
		t.Fatal("published with the ignore rules unknown")
	}
}

// A synced file replaced by a symlink is not something Tendrils syncs, but it
// was not deleted either.
func TestFileReplacedByLinkIsNotTombstoned(t *testing.T) {
	eng, root, ev, stats := syncedTree(t, "a.md", "b.md")
	p := filepath.Join(root, "a.md")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "b.md"), p); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := tombstones(t, ev); len(got) != 0 {
		t.Fatalf("tombstoned %v", got)
	}
	if stats.Unavailable != 1 {
		t.Errorf("unavailable = %d, want the unsupported entry reported", stats.Unavailable)
	}
}

// Nested re-inclusion across shared and local rules survives directory pruning.
func TestNestedNegationSurvivesPruning(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, ignore.FileName, "music/\n", t0)
	writeFile(t, root, "music/a.flac", "a", t0)
	writeFile(t, root, "music/notes/liner.md", "b", t0)
	eng := newEngine(t, root, mustID(t), ev, bl)
	eng.SetExclude([]string{"!music/notes/"})
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := ev.byPath["music/notes/liner.md"]; !ok {
		t.Error("locally re-included file under a shared-ignored folder was not published")
	}
	if _, ok := ev.byPath["music/a.flac"]; ok {
		t.Error("shared-ignored file was published")
	}
}

// The marker is this device's; it must never reach another.
func TestRootMarkerIsNeverPublished(t *testing.T) {
	_, _, ev, _ := syncedTree(t, "a.md")
	if _, ok := ev.byPath[rootid.MarkerName]; ok {
		t.Fatal("root marker was published")
	}
}

func TestUnadoptedRootPauses(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "a.md", "a", t0)
	eng := newEngine(t, root, mustID(t), ev, bl)
	eng.rootID = rootid.Identity{}
	var stats Stats
	eng.OnStats(func(s Stats) { stats = s })
	assertPaused(t, eng.Sync(context.Background()), &stats, rootid.ErrNotAdopted)
	if ev.publishes != 0 {
		t.Fatal("an unadopted root synced")
	}
}

// Each destructive action re-checks the root, so a drive pulled mid-pass stops
// the pass rather than letting it finish against an empty folder.
func TestRootLostMidPassStopsDestructiveWork(t *testing.T) {
	eng, root, ev, stats := syncedTree(t, "a.md", "b.md", "c.md")
	for _, f := range []string{"b.md", "c.md"} {
		if err := os.Remove(filepath.Join(root, f)); err != nil {
			t.Fatal(err)
		}
	}
	pulled := false
	eng.OnProgress(func(p Progress) {
		if p.Path != "" && !pulled {
			pulled = true
			if err := os.Rename(root, root+".away"); err != nil {
				t.Fatal(err)
			}
		}
	})

	assertPaused(t, eng.Sync(context.Background()), stats, rootid.ErrRootMissing)
	if got := tombstones(t, ev); len(got) != 1 {
		t.Fatalf("tombstones = %v, want only the one already under way", got)
	}
}
