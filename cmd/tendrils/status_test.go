package main

import (
	"os"
	"path/filepath"
	"testing"

	"ca.punkscience.tendrils/internal/index"
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
	if err := os.WriteFile(filepath.Join(root, "keep.md"), []byte("note"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Base records keep.md as synced, plus a music file absent from disk, as if
	// this node had synced it and then excluded and deleted it.
	local, err := scan.Tree(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range local {
		if err := store.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Put(&tree.Entry{Path: "music/a.flac", Sha256: "deadbeef", Size: 10}); err != nil {
		t.Fatal(err)
	}

	_, stats, err := computeStatus(store, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Fatalf("without exclude: pending=%d, want 1 (the deleted music path)", stats.Pending)
	}

	_, stats, err = computeStatus(store, root, []string{"music/"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 {
		t.Fatalf("with exclude: pending=%d, want 0", stats.Pending)
	}
}
