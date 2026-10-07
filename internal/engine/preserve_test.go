package engine

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/tree"
)

// conflictCopies returns every conflict copy of rel in root, sorted.
func conflictCopies(t *testing.T, root, rel string) []string {
	t.Helper()
	ext := filepath.Ext(rel)
	stem := strings.TrimSuffix(rel, ext)
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(stem)+scan.ConflictMarker+"*"+ext))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		r, _ := filepath.Rel(root, m)
		out = append(out, filepath.ToSlash(r))
	}
	sort.Strings(out)
	return out
}

func onlyConflictCopy(t *testing.T, root, rel string) string {
	t.Helper()
	copies := conflictCopies(t, root, rel)
	if len(copies) != 1 {
		t.Fatalf("conflict copies of %s = %v, want exactly one", rel, copies)
	}
	return copies[0]
}

func contents(t *testing.T, root string, rels []string) []string {
	t.Helper()
	out := make([]string, 0, len(rels))
	for _, r := range rels {
		got, _ := readFile(t, root, r)
		out = append(out, got)
	}
	sort.Strings(out)
	return out
}

// syncedFile publishes rel from a fresh engine so the index holds it as base.
func syncedFile(t *testing.T, rel, content string, mtime time.Time) (*Engine, string, *fakeEvents, *fakeBlobs) {
	t.Helper()
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, rel, content, mtime)
	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	return eng, root, ev, bl
}

func assertNoRetry(t *testing.T, eng *Engine, rel string) {
	t.Helper()
	retries, err := eng.idx.Retries()
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := retries[rel]; ok {
		t.Errorf("%s recorded as a failure (%+v); a changed destination is re-planned, not backed off", rel, r)
	}
}

// Scenario: A file is edited while a newer remote version downloads.
func TestEditDuringPullIsNotOverwritten(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	eng, root, ev, bl := syncedFile(t, "c.md", "v1", base)
	seedRemote(t, ev, bl, eng.id, "c.md", "remote v2", base.Add(20*time.Second))

	bl.onDownload = func() {
		writeFile(t, root, "c.md", "edited mid-pull", base.Add(10*time.Second))
		bl.onDownload = nil
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got, _ := readFile(t, root, "c.md"); got != "edited mid-pull" {
		t.Fatalf("c.md = %q, the edit made during the pull was overwritten", got)
	}
	if b, _ := eng.idx.Get("c.md"); b == nil || b.Sha256 != hashHex([]byte("v1")) {
		t.Errorf("base = %+v, want it still at v1: nothing was put in place", b)
	}
	assertNoRetry(t, eng, "c.md")

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got, _ := readFile(t, root, "c.md"); got != "remote v2" {
		t.Errorf("c.md = %q after re-planning, want the newer remote", got)
	}
	if got, _ := readFile(t, root, onlyConflictCopy(t, root, "c.md")); got != "edited mid-pull" {
		t.Errorf("conflict copy = %q, want the mid-pull edit", got)
	}
}

// Scenario: A file is created at the destination while a new remote file downloads.
func TestCreateDuringPullIsNotOverwritten(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	eng := newEngine(t, root, id, ev, bl)
	seedRemote(t, ev, bl, id, "n.md", "theirs", time.Unix(1_700_000_500, 0))

	bl.onDownload = func() {
		writeFile(t, root, "n.md", "mine", time.Unix(1_700_000_000, 0))
		bl.onDownload = nil
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got, _ := readFile(t, root, "n.md"); got != "mine" {
		t.Fatalf("n.md = %q, the file created during the pull was overwritten", got)
	}
	assertNoRetry(t, eng, "n.md")

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	all := append(conflictCopies(t, root, "n.md"), "n.md")
	if got := contents(t, root, all); strings.Join(got, ",") != "mine,theirs" {
		t.Errorf("after re-planning, contents = %v, want both versions kept", got)
	}
}

// Scenario: A file is deleted while a newer remote version downloads.
func TestDeleteDuringPullIsNotRecreated(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	eng, root, ev, bl := syncedFile(t, "d.md", "v1", base)
	seedRemote(t, ev, bl, eng.id, "d.md", "remote v2", base.Add(20*time.Second))

	bl.onDownload = func() {
		os.Remove(filepath.Join(root, "d.md"))
		bl.onDownload = nil
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, ok := readFile(t, root, "d.md"); ok {
		t.Error("d.md was recreated over a deletion made during the pull")
	}
	assertNoRetry(t, eng, "d.md")
}

// Scenario: A file edited after the scan is not trashed by an older remote delete.
func TestEditAfterScanIsNotTrashed(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	eng, root, _, _ := syncedFile(t, "t.md", "v1", base)
	openPass(t, eng)
	writeFile(t, root, "t.md", "edited after scan", base.Add(5*time.Second))

	err := eng.deleteLocal("t.md", observed(&tree.Entry{Path: "t.md", Size: 2, ModTime: base}))
	if err == nil {
		t.Fatal("trashed a file that changed after it was observed")
	}
	if got, _ := readFile(t, root, "t.md"); got != "edited after scan" {
		t.Errorf("t.md = %q, want the edit left in place", got)
	}
}

// Scenario: Repeated conflicts on one path keep every losing version. Every
// round runs in the same second under the same key, which is what two machines
// sharing one owner key look like.
func TestRepeatedConflictsKeepEveryLosingVersion(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	eng, root, ev, bl := syncedFile(t, "r.md", "v0", base)

	var losers []string
	for i := range 3 {
		at := base.Add(time.Duration(100*(i+1)) * time.Second)
		loser := "loser " + string(rune('a'+i))
		losers = append(losers, loser)
		writeFile(t, root, "r.md", loser, at)
		seedRemote(t, ev, bl, eng.id, "r.md", "winner "+string(rune('a'+i)), at.Add(10*time.Second))
		if err := eng.Sync(context.Background()); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	got := contents(t, root, conflictCopies(t, root, "r.md"))
	if strings.Join(got, ",") != strings.Join(losers, ",") {
		t.Errorf("conflict copies hold %v, want every loser %v", got, losers)
	}
}
