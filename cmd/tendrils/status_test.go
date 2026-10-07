package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/tree"
)

// The daemonless status path must apply the same ignore rules the engine does.
// Otherwise a path this node has opted out of and deleted reads as a
// forever-pending local deletion — a permanent lie that makes the tree look
// unsynced when it is exactly as the owner asked.
func TestComputeStatusAppliesExclude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TENDRILS_HOME", home)

	store, err := index.Open(filepath.Join(home, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	root := t.TempDir()
	rid, err := rootid.Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.md"), []byte("note"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Base records keep.md as synced, plus a music file absent from disk, as if
	// this node had synced it and then excluded and deleted it.
	scanned, err := scan.Tree(root, scan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range scanned.Entries {
		if err := store.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Put(&tree.Entry{Path: "music/a.flac", Sha256: "deadbeef", Size: 10}); err != nil {
		t.Fatal(err)
	}

	_, stats, err := computeStatus(store, root, rid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Fatalf("without exclude: pending=%d, want 1 (the deleted music path)", stats.Pending)
	}

	_, stats, err = computeStatus(store, root, rid, []string{"music/"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 {
		t.Fatalf("with exclude: pending=%d, want 0", stats.Pending)
	}
}

// Paths the last daemon pass found unrepresentable are reported by the
// daemonless status too, and are not counted as pending.
func TestComputeStatusReportsBlocked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TENDRILS_HOME", home)
	store, err := index.Open(filepath.Join(home, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	rid, err := rootid.Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetBlocked(map[string]string{"CON.txt": "reserved Windows device name", "music/x": "excluded"}); err != nil {
		t.Fatal(err)
	}
	_, stats, err := computeStatus(store, root, rid, []string{"music/"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Blocked != 1 {
		t.Fatalf("blocked = %d, want 1 (excluded paths are not reported)", stats.Blocked)
	}
	if stats.Pending != 0 {
		t.Fatalf("pending = %d, want 0", stats.Pending)
	}
}

// An interrupted operation is reported without a daemon, and printed.
func TestComputeStatusReportsInterruptedOperations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TENDRILS_HOME", home)
	store, err := index.Open(filepath.Join(home, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	rid, err := rootid.Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Begin(index.Op{Kind: index.OpTrash, Path: "a.md", Final: &tree.Entry{Path: "a.md", Deleted: true}}); err != nil {
		t.Fatal(err)
	}
	_, stats, err := computeStatus(store, root, rid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Recovering != 1 {
		t.Fatalf("recovering = %d, want 1", stats.Recovering)
	}
	var out strings.Builder
	printStatus(&out, statusSnapshot{Recovering: stats.Recovering}, false)
	if !strings.Contains(out.String(), "Interrupted:     1") {
		t.Errorf("status output does not report it:\n%s", out.String())
	}
}
