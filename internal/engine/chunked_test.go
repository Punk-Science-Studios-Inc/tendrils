package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// bigContent builds a deterministic body of n bytes that is not uniform, so a
// reassembly bug that drops, repeats, or reorders a chunk changes the hash.
func bigContent(n int) string {
	var b strings.Builder
	b.Grow(n + 16)
	for i := 0; b.Len() < n; i++ {
		b.WriteString("chunk-payload-line-")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteByte('\n')
	}
	return b.String()[:n]
}

// A file larger than one chunk survives a full publish/pull round trip between
// two devices byte-for-byte, and is carried as several blobs rather than one.
func TestChunkedRoundTrip(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	body := bigContent(5000)

	rootA := t.TempDir()
	writeFile(t, rootA, "music/big.flac", body, time.Unix(1_700_000_300, 0))
	engA := newEngine(t, rootA, id, ev, bl)
	engA.chunkSize = 1000
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("A sync: %v", err)
	}

	entries, _, err := engA.fetchRemote(context.Background())
	if err != nil {
		t.Fatalf("fetch remote: %v", err)
	}
	got := entries["music/big.flac"]
	if got == nil {
		t.Fatal("no published entry for music/big.flac")
	}
	if !got.Chunked() {
		t.Fatalf("entry is not chunked; BlobHash=%q Chunks=%d", got.BlobHash, len(got.Chunks))
	}
	if len(got.Chunks) != 5 {
		t.Errorf("chunk count = %d, want 5", len(got.Chunks))
	}
	if got.BlobHash != "" {
		t.Errorf("chunked entry carries BlobHash %q, want empty", got.BlobHash)
	}
	if got.Size != int64(len(body)) {
		t.Errorf("published size = %d, want %d", got.Size, len(body))
	}

	rootB := t.TempDir()
	engB := newEngine(t, rootB, id, ev, bl)
	engB.chunkSize = 1000
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatalf("B sync: %v", err)
	}
	pulled, ok := readFile(t, rootB, "music/big.flac")
	if !ok {
		t.Fatal("B did not pull music/big.flac")
	}
	if pulled != body {
		t.Errorf("pulled %d bytes, want %d (content differs)", len(pulled), len(body))
	}
}

// A file at or below the chunk threshold keeps the single-blob shape, so
// existing blobs and their addresses are untouched by chunking.
func TestAtThresholdStaysSingleBlob(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	body := bigContent(1000)

	root := t.TempDir()
	writeFile(t, root, "docs/exact.txt", body, time.Unix(1_700_000_400, 0))
	eng := newEngine(t, root, id, ev, bl)
	eng.chunkSize = 1000
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	entries, _, err := eng.fetchRemote(context.Background())
	if err != nil {
		t.Fatalf("fetch remote: %v", err)
	}
	got := entries["docs/exact.txt"]
	if got == nil {
		t.Fatal("no published entry")
	}
	if got.Chunked() {
		t.Errorf("file of exactly chunkSize was chunked into %d parts, want single blob", len(got.Chunks))
	}
	if got.BlobHash == "" {
		t.Error("single-blob entry has no BlobHash")
	}
}

// A chunk list that lost a part must fail the whole-file integrity check rather
// than write a silently truncated file.
func TestChunkedTruncatedListIsRejected(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	rootA := t.TempDir()
	writeFile(t, rootA, "music/big.flac", bigContent(5000), time.Unix(1_700_000_500, 0))
	engA := newEngine(t, rootA, id, ev, bl)
	engA.chunkSize = 1000
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("A sync: %v", err)
	}

	entries, _, err := engA.fetchRemote(context.Background())
	if err != nil {
		t.Fatalf("fetch remote: %v", err)
	}
	remote := entries["music/big.flac"]
	remote.Chunks = remote.Chunks[:len(remote.Chunks)-1]

	rootB := t.TempDir()
	engB := newEngine(t, rootB, id, ev, bl)
	engB.chunkSize = 1000
	err = engB.writeRemote(context.Background(), "music/big.flac", remote, false)
	if err == nil {
		t.Fatal("truncated chunk list was accepted, want integrity failure")
	}
	if !strings.Contains(err.Error(), "does not match expected") {
		t.Errorf("error = %v, want an integrity-check failure", err)
	}
	if _, ok := readFile(t, rootB, "music/big.flac"); ok {
		t.Error("a file was written despite the integrity failure")
	}
}
