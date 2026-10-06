package gc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/crypt"
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/tree"
)

// fakeStore is an in-memory Blossom server for sweeps. It is mutex-guarded
// because the real thing is an HTTP server and the sweep reads it from several
// goroutines at once; an unsynchronised double here would only hide races.
type fakeStore struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	uploaded  map[string]int64
	deleted   []string
	listErr   error
	deleteErr error
	downloads int
}

func newFakeStore() *fakeStore {
	return &fakeStore{blobs: map[string][]byte{}, uploaded: map[string]int64{}}
}

// put stores data under addr — deliberately allowing addr not to be the real hash,
// so tests can create blobs whose contents do not match their address.
func (f *fakeStore) put(addr string, data []byte, uploaded time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[addr] = data
	f.uploaded[addr] = uploaded.Unix()
}

// count reports how many blobs remain, for assertions after a sweep.
func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.blobs)
}

func (f *fakeStore) List(_ context.Context, _ string) ([]blob.Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]blob.Stored, 0, len(f.blobs))
	for h, b := range f.blobs {
		out = append(out, blob.Stored{SHA256: h, Size: int64(len(b)), Uploaded: f.uploaded[h]})
	}
	return out, nil
}

func (f *fakeStore) Download(_ context.Context, sha256 string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads++
	b, ok := f.blobs[sha256]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (f *fakeStore) Delete(_ context.Context, sha256 string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.blobs, sha256)
	f.deleted = append(f.deleted, sha256)
	return nil
}

func mustKey(t *testing.T) [32]byte {
	t.Helper()
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	k, err := id.SymmetricKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// seal produces a real sealed blob and the address it belongs at.
func seal(t *testing.T, key [32]byte, plaintext string) (string, []byte) {
	t.Helper()
	sealed, err := crypt.Seal(key, []byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	return hashOf(sealed), sealed
}

// hashOf is the real content address. It must be the genuine SHA-256: the sweep
// verifies that a blob's bytes hash to the address it is filed under, so a
// stand-in hash here would make every blob look corrupt.
func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var old = time.Unix(1_600_000_000, 0) // comfortably outside any grace period
var now = time.Unix(1_700_000_000, 0)

// A dry run reports what it would delete and touches nothing. This is the default
// because the operation is irreversible.
func TestDryRunDeletesNothing(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	liveAddr, liveBlob := seal(t, key, "still referenced")
	orphanAddr, orphanBlob := seal(t, key, "an old version")
	st.put(liveAddr, liveBlob, old)
	st.put(orphanAddr, orphanBlob, old)

	live := map[string]struct{}{liveAddr: {}}
	plan, err := Sweep(context.Background(), st, "pub", live, 1, Options{SymKey: key, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Orphans != 1 {
		t.Errorf("orphans = %d, want 1", plan.Orphans)
	}
	if plan.Kept != 1 {
		t.Errorf("kept = %d, want 1", plan.Kept)
	}
	if plan.Deleted != 0 || len(st.deleted) != 0 {
		t.Errorf("a dry run deleted %d blobs; it must delete none", len(st.deleted))
	}
	if _, still := st.blobs[orphanAddr]; !still {
		t.Error("the orphan was removed during a dry run")
	}
}

func TestApplyDeletesOnlyOrphans(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	liveAddr, liveBlob := seal(t, key, "still referenced")
	orphanAddr, orphanBlob := seal(t, key, "an old version")
	st.put(liveAddr, liveBlob, old)
	st.put(orphanAddr, orphanBlob, old)

	live := map[string]struct{}{liveAddr: {}}
	plan, err := Sweep(context.Background(), st, "pub", live, 1, Options{Apply: true, SymKey: key, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", plan.Deleted)
	}
	if _, still := st.blobs[orphanAddr]; still {
		t.Error("the orphan survived an apply run")
	}
	if _, gone := st.blobs[liveAddr]; !gone {
		t.Fatal("a referenced blob was deleted — this is the unrecoverable mistake")
	}
}

// The whole point of the safety gate: if the relay's answer looks truncated, the
// sweep must abort rather than treat unseen-but-live blobs as garbage.
func TestRefusesOnPartialView(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	addr, b := seal(t, key, "content")
	st.put(addr, b, old)

	// The device knows of 100 live paths; the relay only described 5.
	live := map[string]struct{}{}
	for i := 0; i < 5; i++ {
		live[strings.Repeat(string(rune('a'+i)), 64)] = struct{}{}
	}
	_, err := Sweep(context.Background(), st, "pub", live, 100, Options{Apply: true, SymKey: key, Now: now})
	if !errors.Is(err, ErrPartialView) {
		t.Fatalf("err = %v, want ErrPartialView", err)
	}
	if len(st.deleted) != 0 {
		t.Error("blobs were deleted despite an untrusted view")
	}
}

// A relay that answers for nearly everything the device knows about is trusted:
// a device legitimately holding a few unpublished files must not deadlock GC.
func TestAcceptsNearCompleteView(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	live := map[string]struct{}{}
	for i := 0; i < 95; i++ {
		live[strings.Repeat("0", 62)+string("0123456789abcdefghij"[i%20])+string("0123456789abcdefghij"[(i/20)%20])] = struct{}{}
	}
	if _, err := Sweep(context.Background(), st, "pub", live, 100, Options{SymKey: key, Now: now}); err != nil {
		t.Fatalf("95%% coverage should be accepted, got %v", err)
	}
}

// A failed listing aborts. Sweeping a store we could not enumerate would treat
// every live blob as absent.
func TestRefusesWhenListFails(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	st.listErr = errors.New("connection reset")

	if _, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0, Options{Apply: true, SymKey: key, Now: now}); err == nil {
		t.Fatal("expected an error when the store cannot be listed")
	}
	if len(st.deleted) != 0 {
		t.Error("blobs were deleted after a failed listing")
	}
}

// A blob written moments ago has no event describing it yet, and is indistinguishable
// from an orphan. It must be spared.
func TestGracePeriodSparesRecentBlobs(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	addr, b := seal(t, key, "just uploaded")
	st.put(addr, b, now.Add(-time.Minute))

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0,
		Options{Apply: true, SymKey: key, Now: now, Grace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TooRecent != 1 {
		t.Errorf("tooRecent = %d, want 1", plan.TooRecent)
	}
	if plan.Deleted != 0 {
		t.Error("a blob inside the grace period was deleted")
	}
}

// A blob too short to be a sealed blob is deleted even though an event points at
// it. This is the 0-byte case a disk-full server left behind: while it exists the
// server reports the blob present, uploads are skipped, and the file can never be
// repaired.
func TestInvalidBlobDeletedEvenWhenReferenced(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	addr := strings.Repeat("d", 64)
	st.put(addr, []byte{}, old) // zero bytes, at a real address

	live := map[string]struct{}{addr: {}} // and still referenced
	plan, err := Sweep(context.Background(), st, "pub", live, 1, Options{Apply: true, SymKey: key, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Invalid != 1 {
		t.Errorf("invalid = %d, want 1", plan.Invalid)
	}
	if plan.Kept != 0 {
		t.Errorf("kept = %d, want 0 — an invalid blob must not be kept for being referenced", plan.Kept)
	}
	if _, still := st.blobs[addr]; still {
		t.Error("the invalid blob survived")
	}
}

// A blob inside the grace period that is *invalid* is still deleted: it is not a
// half-finished upload we might want, it is provably junk.
func TestInvalidBlobDeletedDespiteGrace(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	addr := strings.Repeat("e", 64)
	st.put(addr, []byte{}, now.Add(-time.Second))

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0,
		Options{Apply: true, SymKey: key, Now: now, Grace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Invalid != 1 || plan.Deleted != 1 {
		t.Errorf("invalid=%d deleted=%d, want 1 and 1", plan.Invalid, plan.Deleted)
	}
}

// The multi-tenant guard. A Blossom server may hold blobs this identity did not
// upload, and blobs carry no owner attribution — so an unreferenced blob that does
// not decrypt under our key is someone else's and must be left alone.
func TestForeignBlobsAreNeverDeleted(t *testing.T) {
	ours := mustKey(t)
	theirs := mustKey(t)

	st := newFakeStore()
	ourOrphan, ourBlob := seal(t, ours, "our old version")
	theirAddr, theirBlob := seal(t, theirs, "someone else's file")
	st.put(ourOrphan, ourBlob, old)
	st.put(theirAddr, theirBlob, old)

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0,
		Options{Apply: true, SymKey: ours, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NotOurs != 1 {
		t.Errorf("notOurs = %d, want 1", plan.NotOurs)
	}
	if plan.Deleted != 1 {
		t.Errorf("deleted = %d, want 1 (only ours)", plan.Deleted)
	}
	if _, gone := st.blobs[theirAddr]; !gone {
		t.Fatal("another tenant's blob was deleted")
	}
	if _, still := st.blobs[ourOrphan]; still {
		t.Error("our own orphan was not reclaimed")
	}
}

// TrustReferences is the escape hatch for a store known to be single-tenant. It
// skips the per-blob download entirely — which is the only reason it exists, since
// proving ownership means reading every candidate.
func TestTrustReferencesSkipsOwnershipReads(t *testing.T) {
	ours := mustKey(t)
	theirs := mustKey(t)
	st := newFakeStore()
	a, ab := seal(t, ours, "ours")
	b, bb := seal(t, theirs, "theirs")
	st.put(a, ab, old)
	st.put(b, bb, old)

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0,
		Options{Apply: true, TrustReferences: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if st.downloads != 0 {
		t.Errorf("downloaded %d blobs; TrustReferences exists to avoid reading them", st.downloads)
	}
	if plan.Deleted != 2 {
		t.Errorf("deleted = %d, want 2 (everything unreferenced)", plan.Deleted)
	}
}

// Without a key and without TrustReferences there is no way to be safe, so the
// sweep refuses to start rather than pick one.
func TestRefusesWithoutKeyOrTrust(t *testing.T) {
	st := newFakeStore()
	if _, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0, Options{Apply: true}); err == nil {
		t.Fatal("expected a refusal with neither a key nor TrustReferences")
	}
	if len(st.deleted) != 0 {
		t.Error("blobs were deleted")
	}
}

// A deletion that fails is counted and reported, not silently dropped — otherwise
// a sweep that reclaimed nothing would look like a success.
func TestDeleteFailuresAreReported(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	addr, b := seal(t, key, "orphan")
	st.put(addr, b, old)
	st.deleteErr = errors.New("permission denied")

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0,
		Options{Apply: true, SymKey: key, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Failed != 1 {
		t.Errorf("failed = %d, want 1", plan.Failed)
	}
	if plan.Deleted != 0 {
		t.Errorf("deleted = %d, want 0", plan.Deleted)
	}
	if len(plan.FirstErrors) == 0 {
		t.Error("the failure cause should be reported")
	}
}

// The keep-set is built from live entries only. A tombstone references nothing, so
// a deleted file's blob becomes collectable — which is the space GC is for.
func TestLiveBlobsExcludesTombstones(t *testing.T) {
	entries := []*tree.Entry{
		{Path: "a", BlobHash: "aaa", Sha256: "x"},
		{Path: "b", BlobHash: "bbb", Deleted: true},
		{Path: "c", BlobHash: ""}, // published before blob addresses existed
		{Path: "d", BlobHash: "ddd", Sha256: "y"},
	}
	live := LiveBlobs(entries)
	if len(live) != 2 {
		t.Fatalf("live = %v, want 2 entries (aaa, ddd)", live)
	}
	if _, ok := live["bbb"]; ok {
		t.Error("a tombstone's blob was kept alive")
	}
	if _, ok := live["aaa"]; !ok {
		t.Error("a live blob is missing from the keep-set")
	}
}

// Totals must add up, so a report can be trusted as a complete accounting of the
// store rather than a selection from it.
func TestPlanAccountsForEveryBlob(t *testing.T) {
	ours := mustKey(t)
	theirs := mustKey(t)
	st := newFakeStore()

	keptAddr, keptBlob := seal(t, ours, "kept")
	orphanAddr, orphanBlob := seal(t, ours, "orphan")
	recentAddr, recentBlob := seal(t, ours, "recent")
	foreignAddr, foreignBlob := seal(t, theirs, "foreign")
	st.put(keptAddr, keptBlob, old)
	st.put(orphanAddr, orphanBlob, old)
	st.put(recentAddr, recentBlob, now.Add(-time.Minute))
	st.put(foreignAddr, foreignBlob, old)
	st.put(strings.Repeat("f", 64), []byte{}, old) // invalid

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{keptAddr: {}}, 1,
		Options{SymKey: ours, Now: now, Grace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TotalBlobs != 5 {
		t.Fatalf("total = %d, want 5", plan.TotalBlobs)
	}
	sum := plan.Kept + plan.Orphans + plan.TooRecent + plan.Invalid + plan.NotOurs
	if sum != plan.TotalBlobs {
		t.Errorf("categories sum to %d but the store holds %d: kept=%d orphans=%d recent=%d invalid=%d notOurs=%d",
			sum, plan.TotalBlobs, plan.Kept, plan.Orphans, plan.TooRecent, plan.Invalid, plan.NotOurs)
	}
}

// The concurrent ownership pass must classify every candidate exactly once. Racy
// accounting here would misreport what was deleted, which is the one thing a
// destructive tool must never do.
func TestConcurrentOwnershipAccountsForEveryBlob(t *testing.T) {
	ours := mustKey(t)
	theirs := mustKey(t)
	st := newFakeStore()

	const nOurs, nTheirs = 60, 40
	for i := 0; i < nOurs; i++ {
		a, b := seal(t, ours, fmt.Sprintf("ours-%d", i))
		st.put(a, b, old)
	}
	for i := 0; i < nTheirs; i++ {
		a, b := seal(t, theirs, fmt.Sprintf("theirs-%d", i))
		st.put(a, b, old)
	}
	total := st.count() // seal collisions in the test hash would show up here

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0,
		Options{Apply: true, SymKey: ours, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TotalBlobs != total {
		t.Errorf("total = %d, want %d", plan.TotalBlobs, total)
	}
	if got := plan.Orphans + plan.NotOurs + plan.Failed; got != total {
		t.Errorf("orphans+notOurs+failed = %d, want %d (a blob was lost or double-counted)", got, total)
	}
	if plan.Deleted != plan.Orphans {
		t.Errorf("deleted = %d but orphans = %d", plan.Deleted, plan.Orphans)
	}
	// Every surviving blob must be one we could not prove was ours.
	st.mu.Lock()
	defer st.mu.Unlock()
	for h, data := range st.blobs {
		if _, err := crypt.Open(ours, data); err == nil {
			t.Errorf("blob %s decrypts under our key but survived the sweep", h[:8])
		}
	}
}

// A blob whose content does not hash to its address is unusable by every tenant,
// not just us: Blossom is content-addressed, so any client that verifies what it
// fetched will reject it, and it can never be re-uploaded while it sits there
// claiming to exist. It must be reclaimed even though it does not decrypt under
// our key — otherwise the truncated blobs a disk-full server leaves behind are
// filed as "someone else's" and kept forever.
func TestCorruptBlobReclaimedEvenThoughNotOurs(t *testing.T) {
	ours := mustKey(t)
	theirs := mustKey(t)
	st := newFakeStore()

	// Truncated: filed at an address its bytes do not hash to.
	corruptAddr := strings.Repeat("7", 64)
	st.put(corruptAddr, []byte("a truncated blob, long enough to look plausible"), old)

	// A genuine blob belonging to another tenant: intact, just not ours.
	foreignAddr, foreignBlob := seal(t, theirs, "someone else's real file")
	st.put(foreignAddr, foreignBlob, old)

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0,
		Options{Apply: true, SymKey: ours, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Invalid != 1 {
		t.Errorf("invalid = %d, want 1 (the corrupt blob)", plan.Invalid)
	}
	if plan.NotOurs != 1 {
		t.Errorf("notOurs = %d, want 1 (the other tenant's intact blob)", plan.NotOurs)
	}
	if _, still := st.blobs[corruptAddr]; still {
		t.Error("the corrupt blob was kept because it did not decrypt under our key")
	}
	if _, gone := st.blobs[foreignAddr]; !gone {
		t.Fatal("another tenant's intact blob was deleted")
	}
}

// A keep-set blob the store does not hold is reported, not acted on. It means an
// upload never completed, so some file cannot be pulled by a device that lacks
// it — worth surfacing loudly, since nothing else in the system notices.
func TestMissingKeepSetBlobsAreReported(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	presentAddr, presentBlob := seal(t, key, "uploaded fine")
	st.put(presentAddr, presentBlob, old)

	absentAddr := strings.Repeat("9", 64) // referenced, never uploaded
	live := map[string]struct{}{presentAddr: {}, absentAddr: {}}

	plan, err := Sweep(context.Background(), st, "pub", live, 2, Options{Apply: true, SymKey: key, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Missing != 1 {
		t.Errorf("missing = %d, want 1", plan.Missing)
	}
	if len(plan.MissingBlobs) != 1 || plan.MissingBlobs[0] != absentAddr {
		t.Errorf("missing blobs = %v, want [%s]", plan.MissingBlobs, absentAddr)
	}
	if plan.Kept != 1 {
		t.Errorf("kept = %d, want 1", plan.Kept)
	}
	if plan.Deleted != 0 {
		t.Errorf("deleted = %d, want 0 — a missing blob is not something to act on", plan.Deleted)
	}
}

// Missing is measured against the relay's current truth, not the wider keep-set.
// The keep-set also unions in the calling device's index base, which legitimately
// names superseded versions this sweep is meant to reclaim — counting those as
// missing reported healthy, correct reclamation as data loss. On a real store
// that inflated 10 genuine holes into 144.
func TestMissingIgnoresSupersededBaseEntries(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	currentAddr, currentBlob := seal(t, key, "the current version")
	st.put(currentAddr, currentBlob, old)

	supersededAddr := strings.Repeat("3", 64) // an old version, already reclaimed
	genuinelyGone := strings.Repeat("4", 64)  // current, but never uploaded

	// The keep-set is the union: what the relay says, plus this device's base.
	live := map[string]struct{}{currentAddr: {}, supersededAddr: {}, genuinelyGone: {}}
	// The relay's own answer names only the current version and the lost one.
	required := map[string]struct{}{currentAddr: {}, genuinelyGone: {}}

	plan, err := Sweep(context.Background(), st, "pub", live, 2,
		Options{SymKey: key, Now: now, Required: required})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Missing != 1 {
		t.Errorf("missing = %d, want 1 (only the blob the relay still needs)", plan.Missing)
	}
	if len(plan.MissingBlobs) != 1 || plan.MissingBlobs[0] != genuinelyGone {
		t.Errorf("missing = %v, want [%s]", plan.MissingBlobs, genuinelyGone)
	}
}

// Without Required, Missing falls back to the keep-set, so existing callers keep
// their previous meaning.
func TestMissingFallsBackToKeepSet(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()
	absent := strings.Repeat("5", 64)

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{absent: {}}, 1,
		Options{SymKey: key, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Missing != 1 {
		t.Errorf("missing = %d, want 1", plan.Missing)
	}
}

// The byte budget is what stops a sweep from being the process that pushes a
// small host into swap. Counting workers cannot do it: blob sizes on a real store
// span two orders of magnitude, so six slots is 390 MB or 4.8 GB depending purely
// on which blobs are drawn together.
func TestByteBudgetHoldsCeiling(t *testing.T) {
	const limit, each = 100, 30
	b := newByteBudget(limit)

	var mu sync.Mutex
	var peak int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !b.acquire(each) {
				return
			}
			b.mu.Lock()
			cur := b.inUse
			b.mu.Unlock()
			mu.Lock()
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			b.release(each)
		}()
	}
	wg.Wait()

	if peak > limit {
		t.Errorf("peak in flight %d bytes, limit %d", peak, limit)
	}
	if b.inUse != 0 {
		t.Errorf("budget leaked %d bytes", b.inUse)
	}
}

// A blob bigger than the entire budget must still be swept, alone. Refusing it
// would deadlock the sweep on exactly the blobs most worth reclaiming.
func TestByteBudgetAdmitsOversizedBlobAlone(t *testing.T) {
	b := newByteBudget(100)
	if !b.acquire(500) {
		t.Fatal("an over-budget blob must still be admitted")
	}

	joined := make(chan bool, 1)
	go func() { joined <- b.acquire(10) }()
	select {
	case <-joined:
		t.Fatal("a second check ran alongside an over-budget blob")
	case <-time.After(50 * time.Millisecond):
	}

	b.release(500)
	select {
	case ok := <-joined:
		if !ok {
			t.Error("acquire should succeed once the budget frees")
		}
	case <-time.After(2 * time.Second):
		t.Error("acquire never woke after release")
	}
}

// A cancelled sweep must not leave workers parked on a budget nothing will
// release.
func TestByteBudgetCloseUnblocksWaiters(t *testing.T) {
	b := newByteBudget(100)
	if !b.acquire(100) {
		t.Fatal("first acquire should fit")
	}

	waited := make(chan bool, 1)
	go func() { waited <- b.acquire(50) }()
	time.Sleep(20 * time.Millisecond) // let it park on the condition

	b.close()
	select {
	case ok := <-waited:
		if ok {
			t.Error("acquire should report failure once the budget is closed")
		}
	case <-time.After(2 * time.Second):
		t.Error("close did not wake the waiter")
	}
}

// trackingStore records the peak concurrent blob bytes a sweep has outstanding,
// so the ceiling can be asserted rather than assumed. It under-counts slightly —
// the budget is held across Download *and* crypt.Open, while this releases when
// Download returns — so peak <= ceiling here is a necessary condition, not the
// full picture.
type trackingStore struct {
	*fakeStore
	mu       sync.Mutex
	inFlight int64
	peak     int64
}

func (s *trackingStore) Download(ctx context.Context, addr string) ([]byte, error) {
	data, err := s.fakeStore.Download(ctx, addr)
	if err != nil {
		return nil, err
	}
	cost := int64(len(data)) * inspectOverhead

	s.mu.Lock()
	s.inFlight += cost
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	s.mu.Unlock()

	time.Sleep(2 * time.Millisecond) // hold long enough that overlap is observable

	s.mu.Lock()
	s.inFlight -= cost
	s.mu.Unlock()
	return data, nil
}

func TestSweepRespectsMemoryCeiling(t *testing.T) {
	ours := mustKey(t)
	st := &trackingStore{fakeStore: newFakeStore()}

	const nBlobs = 24
	body := strings.Repeat("x", 4096)
	var sealedLen int64
	for i := 0; i < nBlobs; i++ {
		a, b := seal(t, ours, fmt.Sprintf("%s-%d", body, i))
		st.put(a, b, old)
		sealedLen = int64(len(b))
	}
	total := st.count()

	// Room for about two checks at a time, against the six default workers.
	ceiling := sealedLen * inspectOverhead * 2
	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0,
		Options{Apply: true, SymKey: ours, Now: now, MaxInFlightBytes: ceiling})
	if err != nil {
		t.Fatal(err)
	}

	if st.peak > ceiling {
		t.Errorf("peak in flight %d bytes exceeds the %d ceiling", st.peak, ceiling)
	}
	// Bounding memory must not cost correctness: every blob is still swept.
	if plan.Orphans != total {
		t.Errorf("orphans = %d, want %d", plan.Orphans, total)
	}
	if plan.Deleted != total {
		t.Errorf("deleted = %d, want %d", plan.Deleted, total)
	}
}

// A host that cannot hold the biggest blob cannot prove ownership of it, and
// must not try: ownership proof keeps the sealed blob and its plaintext in
// memory at once, ~2x the blob. Over the cap, the blob is spared and reported,
// and every smaller orphan is still swept — so a small host reclaims everything
// it can instead of dying on the one blob it cannot.
func TestBlobsTooLargeToInspectAreSpared(t *testing.T) {
	key := mustKey(t)
	st := newFakeStore()

	bigAddr, big := seal(t, key, strings.Repeat("x", 4096))
	st.put(bigAddr, big, old)
	smallAddr, small := seal(t, key, "tiny")
	st.put(smallAddr, small, old)

	plan, err := Sweep(context.Background(), st, "pub", map[string]struct{}{}, 0,
		Options{Apply: true, SymKey: key, Now: now, MaxInspectBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TooLarge != 1 || plan.TooLargeBytes != int64(len(big)) {
		t.Errorf("tooLarge = %d (%d bytes), want 1 (%d)", plan.TooLarge, plan.TooLargeBytes, len(big))
	}
	if plan.Deleted != 1 || plan.Orphans != 1 {
		t.Errorf("deleted = %d, orphans = %d, want 1 each (the small blob)", plan.Deleted, plan.Orphans)
	}
	if _, still := st.blobs[bigAddr]; !still {
		t.Error("the oversized blob was deleted despite the cap")
	}
	if _, still := st.blobs[smallAddr]; still {
		t.Error("the small orphan survived")
	}
}
