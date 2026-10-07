package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/syncpath"
	"ca.punkscience.tendrils/internal/tree"
)

func staging(t *testing.T, root, rel, content string) string {
	t.Helper()
	tmp := filepath.ToSlash(filepath.Join(filepath.Dir(rel), syncpath.TempPrefix+"test"))
	writeFile(t, root, tmp, content, time.Time{})
	return tmp
}

func exists(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// A download cut off before it was journaled is removed; nothing owns it.
func TestOrphanedStagingIsRemoved(t *testing.T) {
	eng, root, _, _ := syncedFile(t, "notes/a.md", "a", t0)
	tmp := staging(t, root, "notes/a.md", "half a download")

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists(root, tmp) {
		t.Error("orphaned staging file survived the pass")
	}
}

// An operation waiting out a backoff keeps its path and its staged bytes.
func TestHeldOperationKeepsItsStagingAndPath(t *testing.T) {
	eng, root, ev, bl := syncedFile(t, "a.md", "v1", t0)
	seedRemote(t, ev, bl, eng.id, "a.md", "v2", t0.Add(time.Minute))
	tmp := staging(t, root, "a.md", "v2")
	info, _ := os.Lstat(filepath.Join(root, filepath.FromSlash(tmp)))
	if err := eng.idx.Begin(index.Op{
		Kind: index.OpPull, Path: "a.md", ExpectSize: 2, ExpectModTime: t0,
		Staged: tmp, StagedSize: info.Size(), StagedModTime: info.ModTime(),
		Final: &tree.Entry{Path: "a.md", Sha256: hashHex([]byte("v2")), Size: 2, ModTime: t0.Add(time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	eng.idx.SetRetry("a.md", index.Retry{Failures: 1, NextAttempt: time.Now().Add(time.Hour)})
	var stats Stats
	eng.OnStats(func(s Stats) { stats = s })

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(root, tmp) {
		t.Error("a journaled staging file was cleaned up")
	}
	if got, _ := readFile(t, root, "a.md"); got != "v1" {
		t.Errorf("a.md = %q; a held path was acted on", got)
	}
	if stats.Recovering != 1 {
		t.Errorf("Recovering = %d, want 1", stats.Recovering)
	}
}

// A staged download that vanished, with a destination that does not hold it,
// is ambiguous: the operation is dropped and nothing on disk is touched.
func TestVanishedStagingIsAbandonedNotGuessed(t *testing.T) {
	eng, root, _, _ := syncedFile(t, "a.md", "v1", t0)
	if err := eng.idx.Begin(index.Op{
		Kind: index.OpPull, Path: "a.md", ExpectSize: 2, ExpectModTime: t0,
		Staged: syncpath.TempPrefix + "gone",
		Final:  &tree.Entry{Path: "a.md", Sha256: hashHex([]byte("v2")), Size: 2, ModTime: t0.Add(time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	openPass(t, eng)
	held, err := eng.recover(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 0 {
		t.Errorf("held = %v", held)
	}
	if got, _ := readFile(t, root, "a.md"); got != "v1" {
		t.Errorf("a.md = %q, want it untouched", got)
	}
	assertBase(t, eng.idx, "a.md", "v1")
	if ops, _ := eng.idx.Journal(); len(ops) != 0 {
		t.Errorf("journal = %v, want the operation dropped", ops)
	}
}

// A record from a newer build is neither run nor discarded.
func TestUninterpretableOperationIsHeld(t *testing.T) {
	eng, root, _, _ := syncedFile(t, "a.md", "v1", t0)
	tmp := staging(t, root, "a.md", "?")
	eng.idx.Begin(index.Op{Kind: "teleport", Path: "a.md", Staged: tmp, Final: &tree.Entry{Path: "a.md"}})

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ops, _ := eng.idx.Journal(); len(ops) != 1 {
		t.Errorf("journal = %v, want the record kept", ops)
	}
	if !exists(root, tmp) {
		t.Error("its staging file was removed")
	}
}

// Recovery against a root that is not the enrolled folder does nothing at all.
func TestRecoveryPausesOnAnUnavailableRoot(t *testing.T) {
	eng, root, _, _ := syncedFile(t, "a.md", "v1", t0)
	eng.idx.Begin(index.Op{Kind: index.OpTrash, Path: "a.md", ExpectSize: 2, ExpectModTime: t0,
		TrashTo: syncpath.TrashDir + "/a.md", Final: &tree.Entry{Path: "a.md", Deleted: true}})
	if err := os.Remove(filepath.Join(root, syncpath.MarkerName)); err != nil {
		t.Fatal(err)
	}
	var stats Stats
	eng.OnStats(func(s Stats) { stats = s })

	assertPaused(t, eng.Sync(context.Background()), &stats, rootid.ErrNoMarker)
	if ops, _ := eng.idx.Journal(); len(ops) != 1 {
		t.Errorf("journal = %v, want the record kept", ops)
	}
	if got, _ := readFile(t, root, "a.md"); got != "v1" {
		t.Errorf("a.md = %q, want it untouched", got)
	}
}
