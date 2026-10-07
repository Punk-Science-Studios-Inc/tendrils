package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/syncpath"
	"ca.punkscience.tendrils/internal/tree"
)

// confinedTree returns an enrolled empty root beside an "outside" directory
// holding one file, an engine for it, and its captured stats.
func confinedTree(t *testing.T, ev *fakeEvents, bl *fakeBlobs) (eng *Engine, root, outside string, stats *Stats) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "vault")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng = newEngine(t, root, mustID(t), ev, bl)
	stats = &Stats{}
	eng.OnStats(func(s Stats) { *stats = s })
	return eng, root, outside, stats
}

func assertUntouched(t *testing.T, outside string) {
	t.Helper()
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("outside directory now holds %d entries", len(entries))
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim.txt")); string(b) != "untouched" {
		t.Fatalf("outside file changed to %q", b)
	}
}

func withPlatform(e *Engine, p syncpath.Platform) { e.platform = &p }

// Signed events naming paths outside the root, absolute or drive paths, or
// Tendrils' own bookkeeping change nothing on disk and are reported as blocked.
func TestTraversalEventsAreBlockedNotWritten(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng, root, outside, stats := confinedTree(t, ev, bl)
	marker, err := os.ReadFile(filepath.Join(root, rootid.MarkerName))
	if err != nil {
		t.Fatal(err)
	}
	hostile := []string{
		"../outside/victim.txt",
		"a/../../outside/victim.txt",
		"/abs.txt",
		"C:/Windows/evil.txt",
		".tendrils-root",
		".tendrils-trash/a.md",
	}
	for _, p := range hostile {
		seedRemote(t, ev, bl, eng.id, p, "pwned", t0)
	}
	seedRemote(t, ev, bl, eng.id, "fine.md", "ok", t0)
	before := ev.publishes

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	assertUntouched(t, outside)
	if got, _ := os.ReadFile(filepath.Join(root, rootid.MarkerName)); string(got) != string(marker) {
		t.Fatal("root marker was overwritten")
	}
	if got, ok := readFile(t, root, "fine.md"); !ok || got != "ok" {
		t.Fatalf("legitimate file not pulled: %q %v", got, ok)
	}
	if stats.Blocked != len(hostile) {
		t.Errorf("blocked = %d, want %d", stats.Blocked, len(hostile))
	}
	if ev.publishes != before {
		t.Errorf("published %d events in response to hostile paths", ev.publishes-before)
	}
	recorded, err := eng.idx.Blocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != len(hostile) {
		t.Errorf("index recorded %d blocked paths, want %d", len(recorded), len(hostile))
	}
}

// A name another device can hold but this platform cannot is left alone: not
// written, not tombstoned, and still reported as outstanding.
func TestWindowsReservedNamesAreBlockedNotTombstoned(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng, root, _, stats := confinedTree(t, ev, bl)
	withPlatform(eng, syncpath.Platform{Windows: true, CaseInsensitive: true})
	for _, p := range []string{"CON.txt", "notes/report.txt:stream", "trailing.", "ok.md"} {
		seedRemote(t, ev, bl, eng.id, p, "from linux", t0)
	}
	for pass := range 2 {
		before := ev.publishes
		if err := eng.Sync(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if stats.Blocked != 3 {
			t.Errorf("pass %d: blocked = %d, want 3", pass, stats.Blocked)
		}
		if ev.publishes != before {
			t.Errorf("pass %d published %d events", pass, ev.publishes-before)
		}
	}
	if _, ok := readFile(t, root, "ok.md"); !ok {
		t.Error("representable file was not pulled")
	}
	if got := tombstones(t, ev); len(got) != 0 {
		t.Fatalf("tombstoned %v", got)
	}
}

// Two remote names that differ only in case would be one file on a
// case-insensitive root, so neither is written there.
func TestCaseCollisionBlocksBothNames(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng, root, _, stats := confinedTree(t, ev, bl)
	withPlatform(eng, syncpath.Platform{CaseInsensitive: true})
	seedRemote(t, ev, bl, eng.id, "README.md", "upper", t0)
	seedRemote(t, ev, bl, eng.id, "Readme.md", "mixed", t0)
	seedRemote(t, ev, bl, eng.id, "Docs/a.md", "a", t0)
	seedRemote(t, ev, bl, eng.id, "docs/b.md", "b", t0)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"README.md", "Readme.md", "Docs/a.md", "docs/b.md"} {
		if _, ok := readFile(t, root, p); ok {
			t.Errorf("%s was written despite colliding", p)
		}
	}
	if stats.Blocked != 4 {
		t.Errorf("blocked = %d, want 4", stats.Blocked)
	}
}

// A rename that only changes case arrives as a tombstone for one spelling and a
// live entry for the other. It is not a collision, and it applies.
func TestCaseOnlyRenameApplies(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng, root, _, stats := confinedTree(t, ev, bl)
	withPlatform(eng, syncpath.Platform{CaseInsensitive: true})
	seedRemote(t, ev, bl, eng.id, "a.txt", "v1", t0)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	ev.Publish(context.Background(), signEntryAt(t, eng.id, &tree.Entry{Path: "a.txt", Deleted: true, ModTime: later}, later.Unix()))
	seedRemote(t, ev, bl, eng.id, "A.txt", "v1", later)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats.Blocked != 0 {
		t.Errorf("case-only rename reported %d blocked", stats.Blocked)
	}
	if got, ok := readFile(t, root, "A.txt"); !ok || got != "v1" {
		t.Errorf("renamed file: %q %v", got, ok)
	}
	if _, ok := readFile(t, root, ".tendrils-trash/a.txt"); !ok {
		t.Error("old spelling was not moved to the trash")
	}
}

// A sync folder replaced by a link to somewhere else is never written through.
func TestParentSymlinkIsNotWrittenThrough(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng, root, outside, _ := confinedTree(t, ev, bl)
	if err := os.Symlink(outside, filepath.Join(root, "notes")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	seedRemote(t, ev, bl, eng.id, "notes/victim.txt", "pwned", t0)
	seedRemote(t, ev, bl, eng.id, "notes/new.txt", "pwned", t0)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, outside)
	if got := tombstones(t, ev); len(got) != 0 {
		t.Fatalf("tombstoned %v", got)
	}
}

// Legitimate non-ASCII and long names sync between devices unchanged.
func TestUnicodeAndLongPathsConverge(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	id := mustID(t)
	long := "música/日本語/ファイル-" + strings.Repeat("x", 200) + ".flac"
	rootA, rootB := t.TempDir(), t.TempDir()
	writeFile(t, rootA, long, "audio", t0)
	engA := newEngine(t, rootA, id, ev, bl)
	engB := newEngine(t, rootB, id, ev, bl)
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := readFile(t, rootB, long); !ok || got != "audio" {
		t.Fatalf("B has %q %v", got, ok)
	}
}

// The same rename made on this device: the old spelling is tombstoned and the
// new one published, with nothing blocked.
func TestCaseOnlyRenameOriginatesLocally(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng, root, _, stats := confinedTree(t, ev, bl)
	withPlatform(eng, syncpath.Platform{CaseInsensitive: true})
	writeFile(t, root, "a.txt", "v1", t0)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "a.txt"), filepath.Join(root, "A.txt")); err != nil {
		t.Fatal(err)
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats.Blocked != 0 {
		t.Fatalf("locally originated case-only rename reported %d blocked", stats.Blocked)
	}
	if got := tombstones(t, ev); len(got) != 1 || got[0] != "a.txt" {
		t.Errorf("tombstones = %v, want [a.txt]", got)
	}
	if _, ok := ev.byPath["A.txt"]; !ok {
		t.Error("new spelling was not published")
	}
}

// A path under a temp-named directory would be pruned by the next scan and
// then read as deleted, so it is never pulled.
func TestPathUnderTempNameIsNotPulled(t *testing.T) {
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng, root, _, stats := confinedTree(t, ev, bl)
	seedRemote(t, ev, bl, eng.id, ".tendrils-tmp-dir/file.txt", "x", t0)
	for range 2 {
		if err := eng.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := readFile(t, root, ".tendrils-tmp-dir/file.txt"); ok {
		t.Error("file under a temp-named directory was written")
	}
	if got := tombstones(t, ev); len(got) != 0 {
		t.Errorf("tombstoned %v", got)
	}
	if stats.Blocked != 1 {
		t.Errorf("blocked = %d, want 1", stats.Blocked)
	}
}
