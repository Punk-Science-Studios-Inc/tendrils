package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/crypt"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/rootfs"
	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/tree"
)

// fakeEvents is an in-memory relay that mimics the one property the engine
// relies on: it keeps only the newest replaceable event per path (d tag).
//
// It also models the two things a real relay does that the engine has to survive:
// a read that could not be completed (incomplete), and a read that returns
// nothing at all (hidden), which is what a dead connection used to look like.
type fakeEvents struct {
	byPath map[string]*nostr.Event
	// publishes counts every accepted event, so a test can assert that an
	// unchanged tree published *nothing* rather than merely that the set of
	// events ended up the same size.
	publishes int
	// incomplete makes Fetch report its answer as partial, as a relay that timed
	// out or closed the subscription mid-walk now does.
	incomplete bool
	// hidden makes Fetch return no events at all, standing in for a relay whose
	// pages all failed to arrive.
	hidden bool
}

func newFakeEvents() *fakeEvents { return &fakeEvents{byPath: map[string]*nostr.Event{}} }

func (f *fakeEvents) Publish(_ context.Context, evt *nostr.Event) error {
	d := evt.Tags.GetD()
	if prev, ok := f.byPath[d]; !ok || evt.CreatedAt >= prev.CreatedAt {
		f.byPath[d] = evt
	}
	f.publishes++
	return nil
}

func (f *fakeEvents) Fetch(_ context.Context, _ string) ([]*nostr.Event, bool, error) {
	if f.hidden {
		return nil, !f.incomplete, nil
	}
	out := make([]*nostr.Event, 0, len(f.byPath))
	for _, e := range f.byPath {
		out = append(out, e)
	}
	return out, !f.incomplete, nil
}

// fakeBlobs is an in-memory Blossom server addressed by content hash. It counts
// uploads so tests can assert that redundant ones are skipped, not merely that
// the stored set ended up deduplicated by content address.
type fakeBlobs struct {
	data    map[string][]byte
	uploads int
	// onDownload runs inside every Download, standing in for the time a slow
	// transfer gives the owner to touch the destination.
	onDownload func()
	// failDownload, when set, is what every Download returns.
	failDownload error
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{data: map[string][]byte{}} }

func (f *fakeBlobs) Upload(_ context.Context, data []byte) (blob.Descriptor, error) {
	f.uploads++
	sum := hashHex(data)
	cp := append([]byte(nil), data...)
	f.data[sum] = cp
	return blob.Descriptor{SHA256: sum, Size: int64(len(data)), URL: "mem://" + sum}, nil
}

func (f *fakeBlobs) Has(_ context.Context, sha256 string, size int64) (bool, error) {
	b, ok := f.data[sha256]
	return ok && int64(len(b)) == size, nil
}

func (f *fakeBlobs) DownloadSize(_ context.Context, sha256 string, size int64) ([]byte, error) {
	if f.onDownload != nil {
		f.onDownload()
	}
	if f.failDownload != nil {
		return nil, f.failDownload
	}
	b, ok := f.data[sha256]
	if !ok {
		return nil, blob.ErrNotFound
	}
	if size > 0 && int64(len(b)) != size {
		return nil, blob.ErrWrongSize
	}
	return append([]byte(nil), b...), nil
}

func newEngine(t *testing.T, root string, id *keys.Identity, ev EventStore, bl BlobStore) *Engine {
	t.Helper()
	idx, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	rid, err := rootid.Establish(root)
	if err != nil {
		t.Fatalf("establish root: %v", err)
	}
	eng, err := New(root, rid, id, idx, bl, ev, nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

// openPass opens the engine's root as a Sync pass would, for tests that drive a
// single action directly.
func openPass(t *testing.T, e *Engine) {
	t.Helper()
	fsys, err := rootfs.OpenVerified(e.root, e.rootID)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	e.fs = fsys
	t.Cleanup(func() {
		fsys.Close()
		e.fs = nil
	})
}

func writeFile(t *testing.T, root, rel, content string, mtime time.Time) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(abs, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

// seedRemote simulates another device having already sealed, uploaded, and
// published a file, so a single engine can be tested against a populated relay.
func seedRemote(t *testing.T, ev EventStore, bl BlobStore, id *keys.Identity, path, content string, mtime time.Time) {
	t.Helper()
	symKey, err := id.SymmetricKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := crypt.Seal(symKey, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	desc, err := bl.Upload(context.Background(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	entry := &tree.Entry{
		Path:     path,
		Sha256:   hashHex([]byte(content)),
		BlobHash: desc.SHA256,
		Size:     int64(len(content)),
		ModTime:  mtime,
	}
	evt, err := nostrevent.Sign(entry, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Publish(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
}

func mustID(t *testing.T) *keys.Identity {
	t.Helper()
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Sync reports per-file progress: one callback as each planned action begins,
// counting up to a stable total, then a final idle report with no current path.
func TestProgressReporting(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "a.md", "one", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "b.md", "two", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "c.md", "three", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	var seen []Progress
	eng.OnProgress(func(p Progress) { seen = append(seen, p) })

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Three publishes plus the final idle report.
	if len(seen) != 4 {
		t.Fatalf("expected 4 progress reports, got %d: %+v", len(seen), seen)
	}
	for i, p := range seen[:3] {
		if p.Total != 3 {
			t.Errorf("report %d: total=%d want 3", i, p.Total)
		}
		if p.Done != i {
			t.Errorf("report %d: done=%d want %d", i, p.Done, i)
		}
		if p.Path == "" || p.Op != "uploading" {
			t.Errorf("report %d: path=%q op=%q, want a path and \"uploading\"", i, p.Path, p.Op)
		}
	}
	if final := seen[3]; final.Done != 3 || final.Total != 3 || final.Path != "" {
		t.Errorf("final report = %+v, want {Done:3 Total:3 Path:\"\"}", final)
	}
}

// A pass with nothing to do still emits a single idle report, so a UI can clear
// any stale "in progress" line.
func TestProgressNoActions(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng := newEngine(t, t.TempDir(), id, ev, bl)

	var seen []Progress
	eng.OnProgress(func(p Progress) { seen = append(seen, p) })
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(seen) != 1 || seen[0].Total != 0 || seen[0].Path != "" {
		t.Fatalf("expected one idle report, got %+v", seen)
	}
}

// OnStats reports the outstanding-work counts a pass computed for free: two
// local-only files to push, and one conflict copy sitting in the tree (which is
// counted as a conflict, not double-counted as pending).
func TestStatsReporting(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "a.md", "one", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "b.md", "two", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "c"+scan.ConflictMarker+"deadbeef.md", "conflict", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	var stats Stats
	var calls int
	eng.OnStats(func(s Stats) { stats, calls = s, calls+1 })

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if calls != 1 {
		t.Errorf("OnStats called %d times, want 1 per pass", calls)
	}
	if stats.Pending != 2 {
		t.Errorf("pending = %d, want 2 (a.md, b.md)", stats.Pending)
	}
	if stats.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", stats.Conflicts)
	}
	if stats.Deferred != 0 {
		t.Errorf("deferred = %d, want 0 (nothing has failed)", stats.Deferred)
	}
}

// A local-only file is sealed, uploaded, and published on Sync.
func TestPublishLocalFile(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "notes/a.md", "hello", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, _, err := ev.Fetch(context.Background(), id.PublicHex())
	if err != nil || len(got) != 1 {
		t.Fatalf("expected 1 published event, got %d (err %v)", len(got), err)
	}
	entry, err := nostrevent.Parse(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if entry.Path != "notes/a.md" || entry.BlobHash == "" {
		t.Errorf("published entry wrong: %+v", entry)
	}
	if _, ok := bl.data[entry.BlobHash]; !ok {
		t.Errorf("blob %s was not uploaded", entry.BlobHash)
	}
	// The uploaded blob must be ciphertext, never the plaintext.
	if string(bl.data[entry.BlobHash]) == "hello" {
		t.Errorf("blob stored plaintext")
	}
}

// Paths matched by .tendrilsignore are invisible to reconcile: an ignored local
// file is never published, an ignored remote file is never pulled, and an
// existing local copy is left untouched.
func TestSyncSkipsIgnored(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	// A file present only on the relay whose path is ignored must not be pulled.
	seedRemote(t, ev, bl, id, ".obsidian/workspace.json", "remote ui state", time.Unix(1_700_000_050, 0))

	root := t.TempDir()
	writeFile(t, root, ".tendrilsignore", ".obsidian/workspace*.json\n.trash/\n", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "note.md", "hello", time.Unix(1_700_000_000, 0))
	writeFile(t, root, ".trash/old.md", "trashed", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, _, err := ev.Fetch(context.Background(), id.PublicHex())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]*tree.Entry{}
	for _, e := range got {
		ent, err := nostrevent.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		byPath[ent.Path] = ent
	}
	if e, ok := byPath["note.md"]; !ok || e.BlobHash == "" {
		t.Errorf("note.md not published properly: %+v", e)
	}
	if _, ok := byPath[".trash/old.md"]; ok {
		t.Errorf("ignored local file was published")
	}
	if _, ok := readFile(t, root, ".obsidian/workspace.json"); ok {
		t.Errorf("ignored remote file was pulled to disk")
	}
	if got, ok := readFile(t, root, ".trash/old.md"); !ok || got != "trashed" {
		t.Errorf("ignored local file changed: %q present=%v", got, ok)
	}
}

// A file present only on the relay is pulled and written locally.
func TestPullRemoteFile(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	seedRemote(t, ev, bl, id, "docs/b.txt", "remote content", time.Unix(1_700_000_100, 0))

	root := t.TempDir()
	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := readFile(t, root, "docs/b.txt")
	if !ok || got != "remote content" {
		t.Errorf("pulled file = %q (present=%v), want %q", got, ok, "remote content")
	}
}

// Two devices sharing one key and one relay/Blossom converge: what A publishes,
// B receives — the whole product, headless.
func TestTwoDeviceConvergence(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	rootA := t.TempDir()
	writeFile(t, rootA, "shared/note.md", "written on A", time.Unix(1_700_000_200, 0))
	engA := newEngine(t, rootA, id, ev, bl)
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("A sync: %v", err)
	}

	rootB := t.TempDir()
	engB := newEngine(t, rootB, id, ev, bl)
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatalf("B sync: %v", err)
	}

	got, ok := readFile(t, rootB, "shared/note.md")
	if !ok || got != "written on A" {
		t.Errorf("B has %q (present=%v), want %q", got, ok, "written on A")
	}
}

// A file rewritten between the scan that hashed it and the publish that reads it
// must be published under the hash of the bytes actually uploaded, not the scan's
// stale one. Publishing the stale hash produces an event no device can ever
// satisfy: the blob it points at decrypts to content that does not match the hash
// beside it, so every puller rejects it on the integrity check, forever.
func TestPublishUsesHashOfUploadedBytes(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	rootA := t.TempDir()
	writeFile(t, rootA, "track.mp3", "retagged bytes", time.Unix(1_700_000_200, 0))
	engA := newEngine(t, rootA, id, ev, bl)

	// The entry a scan would have produced had it read the file before a tag
	// editor rewrote it: right path and mtime, hash of content that is now gone.
	stale := &tree.Entry{
		Path:    "track.mp3",
		Sha256:  hashHex([]byte("bytes as first scanned")),
		Size:    int64(len("bytes as first scanned")),
		ModTime: time.Unix(1_700_000_200, 0),
	}
	openPass(t, engA)
	if err := engA.publishLocal(context.Background(), stale); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// The published identity describes what was uploaded.
	evt := ev.byPath["track.mp3"]
	if evt == nil {
		t.Fatal("nothing published")
	}
	entry, err := nostrevent.Parse(evt)
	if err != nil {
		t.Fatal(err)
	}
	if want := hashHex([]byte("retagged bytes")); entry.Sha256 != want {
		t.Errorf("published sha256 = %s, want %s (the bytes uploaded)", entry.Sha256, want)
	}

	// Which is what lets another device actually receive it.
	rootB := t.TempDir()
	engB := newEngine(t, rootB, id, ev, bl)
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatalf("B sync: %v", err)
	}
	if got, ok := readFile(t, rootB, "track.mp3"); !ok || got != "retagged bytes" {
		t.Errorf("B has %q (present=%v), want %q", got, ok, "retagged bytes")
	}
}

// Two devices that copy in the same file before either has synced both plan a
// publish, but deterministic sealing gives them the same blob address, so the
// second finds it already present and skips the upload. One blob, one transfer.
func TestSimultaneousPublishUploadsOneBlob(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	mtime := time.Unix(1_700_000_300, 0)

	rootA := t.TempDir()
	writeFile(t, rootA, "notes/same.md", "identical content", mtime)
	rootB := t.TempDir()
	writeFile(t, rootB, "notes/same.md", "identical content", mtime)

	// Neither device has synced, so both see remote==nil and decide to publish.
	engA := newEngine(t, rootA, id, ev, bl)
	engB := newEngine(t, rootB, id, ev, bl)
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("A sync: %v", err)
	}
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatalf("B sync: %v", err)
	}

	if len(bl.data) != 1 {
		t.Errorf("server holds %d blobs, want 1", len(bl.data))
	}
	if bl.uploads != 1 {
		t.Errorf("%d uploads, want 1 (B should have skipped a present blob)", bl.uploads)
	}
}

// A locally deleted file becomes a tombstone on the relay, and a second device
// then moves its copy to the trash — the delete propagates and stays recoverable.
func TestDeletePropagation(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	rootA := t.TempDir()
	writeFile(t, rootA, "gone.md", "temporary", time.Unix(1_700_000_300, 0))
	engA := newEngine(t, rootA, id, ev, bl)
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("A publish: %v", err)
	}

	rootB := t.TempDir()
	engB := newEngine(t, rootB, id, ev, bl)
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatalf("B pull: %v", err)
	}
	if _, ok := readFile(t, rootB, "gone.md"); !ok {
		t.Fatal("precondition: B should have the file before the delete")
	}

	// A deletes the file and syncs → tombstone published.
	if err := os.Remove(filepath.Join(rootA, "gone.md")); err != nil {
		t.Fatal(err)
	}
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("A delete sync: %v", err)
	}

	// B syncs → its copy moves to the trash.
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatalf("B delete sync: %v", err)
	}
	if _, ok := readFile(t, rootB, "gone.md"); ok {
		t.Errorf("B still has the deleted file")
	}
	if _, ok := readFile(t, rootB, filepath.ToSlash(filepath.Join(scan.TrashDir, "gone.md"))); !ok {
		t.Errorf("deleted file was not preserved in the trash")
	}
}

// When the remote wins over a diverged local edit, the local version is kept as
// a conflict copy — a wrong last-writer-wins guess costs a rename, never data.
func TestConflictCopyPreservesLocalEdit(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	root := t.TempDir()
	// Local file diverged from a recorded base and is older than the remote.
	writeFile(t, root, "c.md", "local edit", time.Unix(1_700_000_000, 0))
	eng := newEngine(t, root, id, ev, bl)
	// Record a base that differs from the local content (an earlier synced state).
	if err := eng.idx.Put(&tree.Entry{Path: "c.md", Sha256: hashHex([]byte("v1 base")), ModTime: time.Unix(1_699_000_000, 0)}); err != nil {
		t.Fatal(err)
	}
	// Remote is newer and wins.
	seedRemote(t, ev, bl, id, "c.md", "remote wins", time.Unix(1_700_000_500, 0))

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if got, _ := readFile(t, root, "c.md"); got != "remote wins" {
		t.Errorf("c.md = %q, want %q", got, "remote wins")
	}
	conflict := onlyConflictCopy(t, root, "c.md")
	if got, ok := readFile(t, root, conflict); !ok || got != "local edit" {
		t.Errorf("conflict copy %q = %q (present=%v), want %q", conflict, got, ok, "local edit")
	}
}

// A tree that has already been published and has not changed must publish
// nothing further, however many passes run over it. This is the baseline the
// republish bug violated: on the reference fleet a 5,188-file tree had produced
// 114,747 events, byte-identical metadata republished dozens of times per file.
func TestUnchangedTreePublishesNothingOnLaterPasses(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "a.md", "one", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "notes/b.md", "two", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if ev.publishes != 2 {
		t.Fatalf("first pass published %d events, want 2", ev.publishes)
	}
	for i := 0; i < 3; i++ {
		if err := eng.Sync(context.Background()); err != nil {
			t.Fatalf("sync %d: %v", i+2, err)
		}
	}
	if ev.publishes != 2 {
		t.Errorf("published %d events over four passes, want 2: an unchanged tree must say nothing", ev.publishes)
	}
	if bl.uploads != 2 {
		t.Errorf("uploaded %d blobs, want 2", bl.uploads)
	}
}

// The bug, reduced: a pass whose read of the relay failed sees no entry for any
// path, and "no entry" is what makes the reconciler publish. A read that could
// not be completed must not be allowed to mean that.
//
// On the reference fleet this was one pass republishing 5,048 paths in 33
// minutes, triggered by nothing worse than the relay connection going away for a
// few seconds.
func TestIncompleteRemoteViewDoesNotRepublishTheTree(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	for _, p := range []string{"a.md", "b.md", "notes/c.md"} {
		writeFile(t, root, p, "content of "+p, time.Unix(1_700_000_000, 0))
	}

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	published := ev.publishes

	// The relay is now unreadable: the pages never arrived, so the fetch returns
	// nothing and admits it is not the whole story.
	ev.hidden, ev.incomplete = true, true
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync against an incomplete view: %v", err)
	}
	if ev.publishes != published {
		t.Errorf("published %d more events against an incomplete view, want 0",
			ev.publishes-published)
	}

	// And once the relay answers in full again, the pass is still a no-op —
	// nothing was lost by waiting.
	ev.hidden, ev.incomplete = false, false
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync after recovery: %v", err)
	}
	if ev.publishes != published {
		t.Errorf("published %d more events after recovery, want 0", ev.publishes-published)
	}
}

// The other half of the rule, and the reason it is not simply "never republish":
// a relay that has genuinely lost an event must be told again. A *complete* read
// that comes back without a path is evidence, and the engine acts on it.
func TestCompleteButEmptyRemoteViewRepublishes(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "a.md", "one", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	published := ev.publishes

	// The relay lost it — and says so with a complete, empty answer.
	ev.byPath = map[string]*nostr.Event{}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if ev.publishes != published+1 {
		t.Errorf("published %d events, want %d: a complete view that lacks the path is evidence, and must be acted on",
			ev.publishes, published+1)
	}
}

// An incomplete view must not gag a device that has something new to say. A file
// created or edited since the last sync is published even while the relay is only
// half-readable: that publish carries information the set does not have, and
// withholding it would mean a flaky relay stops a device from syncing at all.
func TestIncompleteRemoteViewStillPublishesNewWork(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "old.md", "already synced", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	published := ev.publishes

	writeFile(t, root, "new.md", "brand new", time.Unix(1_700_000_500, 0))
	writeFile(t, root, "old.md", "edited since", time.Unix(1_700_000_600, 0))
	ev.hidden, ev.incomplete = true, true
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync against an incomplete view: %v", err)
	}
	if ev.publishes != published+2 {
		t.Errorf("published %d events, want %d (the new file and the edited one)",
			ev.publishes-published, 2)
	}
}

// signEntryAt signs an entry with an explicit publication stamp. FoldRemote
// arbitrates on created_at, so a test of it has to place events on that axis
// deliberately — and the stamp must be set before signing or the signature does
// not cover it.
func signEntryAt(t *testing.T, id *keys.Identity, e *tree.Entry, createdAt int64) *nostr.Event {
	t.Helper()
	evt, err := nostrevent.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	evt.CreatedAt = nostr.Timestamp(createdAt)
	if err := evt.Sign(id.SecretHex()); err != nil {
		t.Fatal(err)
	}
	return evt
}

// A relay that hands back superseded versions of a path — which the reference
// relay does for every path whose d tag runs past 100 bytes, because its tag
// index will not index a value that long and replacement therefore never fires —
// must be folded the way the relay would have folded it: latest publish wins.
//
// Folding by mtime instead silently undid restores. Roll a file back to an older
// version, publish it, and the superseded event describing the newer version
// still won the fold, so the restoring device pulled its own file back.
func TestFoldRemoteKeepsTheLatestPublishNotTheNewestMtime(t *testing.T) {
	id := mustID(t)
	newer := &tree.Entry{Path: "song.flac", Sha256: "newversion", BlobHash: "blobnew", Size: 2, ModTime: time.Unix(1_700_000_500, 0)}
	restored := &tree.Entry{Path: "song.flac", Sha256: "oldversion", BlobHash: "blobold", Size: 1, ModTime: time.Unix(1_700_000_000, 0)}

	evts := []*nostr.Event{
		signEntryAt(t, id, newer, 1_800_000_000),    // published first
		signEntryAt(t, id, restored, 1_800_000_100), // then the rollback
	}
	got, skipped := FoldRemote(evts)
	if len(skipped) != 0 {
		t.Fatalf("skipped %d events: %v", len(skipped), skipped)
	}
	if got["song.flac"].Sha256 != "oldversion" {
		t.Errorf("fold chose %q, want the most recent publish %q",
			got["song.flac"].Sha256, "oldversion")
	}
}

// Republished-but-identical events differ only in their blob address (every
// pre-deterministic-sealing pass sealed the same bytes to a different blob). They
// tie on mtime and on content hash, so the old fold fell through to map order and
// named whichever event the fetch happened to return first. Two folds of the same
// events could then name different blobs — and the blob collector folds with this
// same function, so it and the engine could disagree about which blob is live.
func TestFoldRemoteIsOrderIndependentAcrossIdenticalRepublishes(t *testing.T) {
	id := mustID(t)
	mtime := time.Unix(1_700_000_000, 0)
	entry := func(blobHash string) *tree.Entry {
		return &tree.Entry{Path: "cover.jpg", Sha256: "samecontent", BlobHash: blobHash, Size: 3, ModTime: mtime}
	}
	first := signEntryAt(t, id, entry("blob-from-an-old-pass"), 1_800_000_000)
	second := signEntryAt(t, id, entry("blob-from-a-later-pass"), 1_800_000_050)

	for _, order := range [][]*nostr.Event{{first, second}, {second, first}} {
		got, _ := FoldRemote(order)
		if got["cover.jpg"].BlobHash != "blob-from-a-later-pass" {
			t.Errorf("fold chose blob %q, want the latest publish's %q (input order must not matter)",
				got["cover.jpg"].BlobHash, "blob-from-a-later-pass")
		}
	}
}

// A per-device exclude (config.json's "exclude") is invisible to this node the
// same way a shared .tendrilsignore rule is: a remote-only path is not pulled.
// Unlike the shared file, it changes nothing for any other device.
func TestLocalExcludeDoesNotPullRemote(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	seedRemote(t, ev, bl, id, "music/song.flac", "audio", time.Unix(1_700_000_050, 0))
	seedRemote(t, ev, bl, id, "notes/a.md", "note", time.Unix(1_700_000_050, 0))

	root := t.TempDir()
	eng := newEngine(t, root, id, ev, bl)
	eng.SetExclude([]string{"music/"})
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if _, ok := readFile(t, root, "music/song.flac"); ok {
		t.Errorf("excluded remote file was pulled locally")
	}
	if got, ok := readFile(t, root, "notes/a.md"); !ok || got != "note" {
		t.Errorf("non-excluded remote file = %q present=%v, want it pulled", got, ok)
	}
}

// An excluded path that this node had already synced and then deleted must not
// be tombstoned (that would delete it for the rest of the set) and must not be
// pulled back. The local copy is simply gone, which is the point of excluding it.
func TestLocalExcludeDoesNotTombstone(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	root := t.TempDir()
	writeFile(t, root, "music/song.flac", "audio", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "keep.md", "note", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil { // publishes both
		t.Fatalf("first sync: %v", err)
	}

	eng.SetExclude([]string{"music/"})
	if err := os.Remove(filepath.Join(root, "music", "song.flac")); err != nil {
		t.Fatal(err)
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	got, _, err := ev.Fetch(context.Background(), id.PublicHex())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		ent, err := nostrevent.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		if ent.Path == "music/song.flac" && !ent.Live() {
			t.Errorf("excluded path was tombstoned, which would delete it for every device")
		}
	}
	if _, ok := readFile(t, root, "music/song.flac"); ok {
		t.Errorf("excluded path was pulled back after being deleted")
	}
}

// Local patterns are applied after the shared .tendrilsignore, so they can also
// re-include (with '!') a path the shared file hides.
func TestLocalExcludeCanReincludeSharedIgnore(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	root := t.TempDir()
	writeFile(t, root, ".tendrilsignore", "*.secret\n", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "a.secret", "shared-ignored", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "keep.secret", "locally re-included", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	eng.SetExclude([]string{"!keep.secret"})
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, _, err := ev.Fetch(context.Background(), id.PublicHex())
	if err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{}
	for _, e := range got {
		ent, err := nostrevent.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		published[ent.Path] = true
	}
	if published["a.secret"] {
		t.Errorf("shared-ignored file was published")
	}
	if !published["keep.secret"] {
		t.Errorf("locally re-included file was not published")
	}
}

// A missing .tendrilsignore is not an error: local patterns still apply.
func TestIgnoreMatcherWithoutSharedFile(t *testing.T) {
	fsys, err := rootfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	m, err := IgnoreMatcher(fsys, []string{"music/"})
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("IgnoreMatcher returned nil")
	}
	if !m.Match("music/a.flac") {
		t.Errorf("local pattern did not match without a shared ignore file")
	}
	if m.Match("notes/a.md") {
		t.Errorf("unrelated path matched")
	}
}
