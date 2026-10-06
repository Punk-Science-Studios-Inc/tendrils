// Package gc reclaims Blossom blobs no live file event references any more.
//
// Nothing in Tendrils has ever deleted a blob. Every edit of a file publishes a
// new blob and abandons the old one, and two now-fixed bugs multiplied that
// enormously: sealing used a random nonce (so the same content re-uploaded as a
// fresh blob every time) and a `created_at` mistake made losing devices republish
// forever. The result on the author's own store was 926 GB of blobs for a 181 GB
// tree — 80% of it garbage, and a full disk that stopped all syncing.
//
// Deleting data is the one operation here that cannot be undone by syncing again,
// so this package is built to refuse rather than guess. Four rules:
//
//   - **The keep-set comes from the relay, not from one device.** A blob is live
//     if any current file event references it. A single device's index only knows
//     what that device synced.
//   - **A partial view never deletes.** If the relay fetch fails, or returns
//     implausibly less than the device already knows about, the sweep aborts. A
//     truncated keep-set would classify live blobs as orphans — precisely the
//     failure that makes GC dangerous.
//   - **Recent blobs are untouchable.** An upload whose describing event has not
//     propagated yet looks exactly like an orphan. A grace period covers that gap.
//   - **Other tenants are untouchable.** A Blossom server may hold blobs this
//     identity did not upload, and stored blobs carry no owner attribution. So
//     unless the operator declares the store single-tenant, a candidate is deleted
//     only once it is *proven* ours by decrypting under our key — an AES-GCM tag
//     no other key can forge.
//
// Invalid blobs are the one exception to the keep-set: a blob too short to be a
// sealed blob at all cannot be the bytes its address claims, so it is deleted
// even when referenced. Keeping it would be worse than deleting it — while it
// sits there the server reports the blob as present, every upload is skipped, and
// the file it belongs to can never be repaired.
package gc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/crypt"
	"ca.punkscience.tendrils/internal/tree"
)

// minSealedSize is the smallest a sealed blob can be: a 12-byte nonce plus a
// 16-byte GCM tag, wrapping zero bytes of plaintext. Anything shorter is not a
// sealed blob, whatever its filename claims.
const minSealedSize = 12 + 16

// DefaultGrace is how recently a blob may have been written and still be spared.
// Generous on purpose: the cost of waiting a day to reclaim a blob is nothing,
// and the cost of deleting one mid-publish is a file that cannot be pulled.
const DefaultGrace = 48 * time.Hour

// minRelayCoverage is the fraction of the device's own known live paths that the
// relay fetch must account for before the sweep will trust it. Set below 1 so a
// device with genuinely unpublished local files does not deadlock the sweep, but
// high enough that a truncated fetch is caught.
const minRelayCoverage = 0.9

// ErrPartialView means the relay's answer could not be trusted as complete, so
// no deletion was attempted.
var ErrPartialView = errors.New("gc: incomplete view of live blobs; refusing to delete")

// Store is the blob-server side of a sweep.
type Store interface {
	List(ctx context.Context, pubkey string) ([]blob.Stored, error)
	Download(ctx context.Context, sha256 string) ([]byte, error)
	Delete(ctx context.Context, sha256 string) error
}

// Options configures one sweep.
type Options struct {
	// Apply actually deletes. False (the default) plans and reports only.
	Apply bool
	// Grace spares blobs written more recently than this. Zero uses DefaultGrace.
	Grace time.Duration
	// TrustReferences skips per-blob ownership proof, deleting anything the
	// keep-set does not cover. Only correct when the store holds this identity's
	// blobs and nothing else — it will destroy another tenant's data otherwise.
	TrustReferences bool
	// SymKey proves ownership: a candidate is ours only if it decrypts under it.
	// Required unless TrustReferences is set.
	SymKey [32]byte
	// Now is the clock, injectable for tests. Zero means time.Now().
	Now time.Time
	// Required is the set of blobs that must exist for the tree to be pullable —
	// the relay's current truth alone. Missing is reported against this, never
	// against the full keep-set: the keep-set also unions in the calling device's
	// index base, which legitimately references superseded versions that this
	// sweep is meant to reclaim. Conflating the two reports normal, correct
	// reclamation as data loss. Nil falls back to the keep-set.
	Required map[string]struct{}
	// Workers is how many ownership checks run at once. Zero uses ownerWorkers.
	Workers int
	// MaxInFlightBytes caps the total memory the ownership checks may hold at
	// once. Zero uses defaultMaxInFlightBytes.
	//
	// Workers alone is a bad memory dial because it counts blobs, and blobs vary
	// by two orders of magnitude. On the author's store the mean is 32 MB and the
	// largest is 412 MB, so six workers is either ~390 MB or ~4.8 GB depending
	// entirely on which blobs happen to be drawn together — and the 4.8 GB case on
	// a 1.8 GB host is what took the machine down. Counting bytes makes the ceiling
	// hold whatever the draw.
	MaxInFlightBytes int64
	// MaxInspectBytes refuses to inspect (and therefore delete) any blob larger
	// than this. Proving ownership means holding the whole sealed blob and, for a
	// successful open, its plaintext too — roughly twice the blob's size at peak.
	// On a host with little RAM the largest blobs in a store are exactly the ones
	// a sweep cannot safely touch; without this cap the sweep allocates ~2× the
	// biggest blob and is OOM-killed, taking the host's other services with it.
	// Such blobs are counted in Plan.TooLarge and left in place. Zero means no cap.
	MaxInspectBytes int64
}

// Plan is what a sweep found, and what it did if Apply was set.
type Plan struct {
	// Blobs the store holds.
	TotalBlobs int
	TotalBytes int64
	// Referenced by a live event (the keep-set).
	Kept      int
	KeptBytes int64
	// Unreferenced and eligible: deleted when Apply is set.
	Orphans     int
	OrphanBytes int64
	// Unreferenced but spared by the grace period.
	TooRecent      int
	TooRecentBytes int64
	// Too short to be a sealed blob. Deleted regardless of references.
	Invalid      int
	InvalidBytes int64
	// Unreferenced but not provably ours, so left alone.
	NotOurs      int
	NotOursBytes int64
	// Unreferenced, but larger than the host is willing to inspect, so left alone.
	// Nothing is wrong with them; the sweep simply cannot prove ownership without
	// more memory than it has. See Options.MaxInspectBytes.
	TooLarge      int
	TooLargeBytes int64
	// Missing counts keep-set blobs the store does not hold. A sweep never causes
	// this — it only deletes what the keep-set excludes — but the sweep is the one
	// moment the two sets are compared, so it is the natural place to notice.
	// Non-zero means some file cannot be pulled by a device that does not already
	// have it, so it is worth surfacing rather than silently passing over.
	Missing int
	// MissingBlobs samples the addresses, for diagnosis.
	MissingBlobs []string
	// Deleted and Failed are populated when Apply is set.
	Deleted      int
	DeletedBytes int64
	Failed       int
	// FirstErrors samples deletion failures, so a run that mostly worked still
	// reports why the rest did not.
	FirstErrors []string
}

// Sweep reclaims orphaned blobs. live is the set of blob addresses that current
// file events reference; knownLive is how many live paths the calling device
// believes exist, used only to sanity-check that live is not a truncated view.
func Sweep(ctx context.Context, store Store, pubkey string, live map[string]struct{}, knownLive int, opts Options) (Plan, error) {
	if !opts.TrustReferences && opts.SymKey == ([32]byte{}) {
		return Plan{}, errors.New("gc: ownership proof requires a key; pass TrustReferences only for a single-tenant store")
	}
	// A keep-set materially smaller than what this device already knows about means
	// we are not looking at the whole picture. Deleting on that basis would remove
	// blobs that are live but unseen.
	if knownLive > 0 && float64(len(live)) < float64(knownLive)*minRelayCoverage {
		return Plan{}, fmt.Errorf("%w: relay described %d live blobs but this device knows of %d paths",
			ErrPartialView, len(live), knownLive)
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	grace := opts.Grace
	if grace == 0 {
		grace = DefaultGrace
	}
	cutoff := now.Add(-grace)

	stored, err := store.List(ctx, pubkey)
	if err != nil {
		return Plan{}, fmt.Errorf("gc: list blobs: %w", err)
	}

	var plan Plan

	// Classification that needs no I/O happens first and in listing order. The
	// order matters more than it looks: blob stores live on spinning disks, and
	// walking them in content-address order is by construction a random seek per
	// blob. Sorting by hash here (for tidy reporting) dropped a real sweep to
	// 5 MB/s. The server lists in directory order, which is far closer to physical
	// order, so we keep it. Totals do not depend on order anyway.
	present := make(map[string]struct{}, len(stored))
	var candidates []blob.Stored
	for _, b := range stored {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		present[b.SHA256] = struct{}{}
		plan.TotalBlobs++
		plan.TotalBytes += b.Size

		// Invalid first: a blob that cannot hold what its address claims is junk
		// even when an event points at it, and leaving it there is what stops the
		// referencing file from ever being repaired.
		if b.Size < minSealedSize {
			plan.Invalid++
			plan.InvalidBytes += b.Size
			plan.delete(ctx, store, b, opts.Apply)
			continue
		}
		if _, referenced := live[b.SHA256]; referenced {
			plan.Kept++
			plan.KeptBytes += b.Size
			continue
		}
		if time.Unix(b.Uploaded, 0).After(cutoff) {
			plan.TooRecent++
			plan.TooRecentBytes += b.Size
			continue
		}
		if opts.MaxInspectBytes > 0 && b.Size > opts.MaxInspectBytes {
			// Ownership proof needs the sealed blob and its plaintext in memory at
			// once. Refusing here is the difference between leaving a big orphan
			// behind and taking the host down proving it is one.
			plan.TooLarge++
			plan.TooLargeBytes += b.Size
			continue
		}
		candidates = append(candidates, b)
	}

	// Anything the relay's current truth names but the store does not hold.
	// Reported, never acted on: there is nothing to delete, and the fix is a
	// device re-uploading.
	required := opts.Required
	if required == nil {
		required = live
	}
	for h := range required {
		if _, ok := present[h]; !ok {
			plan.Missing++
			if len(plan.MissingBlobs) < 20 {
				plan.MissingBlobs = append(plan.MissingBlobs, h)
			}
		}
	}

	if opts.TrustReferences {
		for _, b := range candidates {
			if err := ctx.Err(); err != nil {
				return plan, err
			}
			plan.Orphans++
			plan.OrphanBytes += b.Size
			plan.delete(ctx, store, b, opts.Apply)
		}
		return plan, nil
	}
	return sweepOwned(ctx, store, candidates, opts, plan)
}

// ownerWorkers is the default number of concurrent ownership checks. Proving
// ownership means reading the blob, so a handful of readers lets the disk
// scheduler coalesce and reorder seeks.
const ownerWorkers = 6

// defaultMaxInFlightBytes is the default ceiling on blob bytes held at once.
// Sized for the smallest host this is expected to sweep — a 1.8 GB Raspberry Pi
// also running a relay and a blob server — rather than for throughput. A bigger
// machine can raise it; the point of the default is that the sweep cannot be the
// thing that pushes a small one into swap.
const defaultMaxInFlightBytes = 256 << 20

// inspectOverhead is what one ownership check costs in memory per byte of blob:
// the sealed bytes arrive whole from the store, and crypt.Open allocates the
// plaintext separately rather than decrypting in place.
const inspectOverhead = 2

// byteBudget is a semaphore counting bytes rather than slots.
//
// A blob larger than the whole budget is admitted anyway, but only when nothing
// else is in flight. Refusing it outright would deadlock the sweep on exactly the
// blobs that most need reclaiming, and serialising it is the honest reading of a
// budget it cannot fit inside.
type byteBudget struct {
	mu     sync.Mutex
	cond   *sync.Cond
	limit  int64
	inUse  int64
	closed bool
}

func newByteBudget(limit int64) *byteBudget {
	b := &byteBudget{limit: limit}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire blocks until n bytes fit, reporting false if the budget was closed
// while waiting.
func (b *byteBudget) acquire(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for !b.closed && b.inUse > 0 && b.inUse+n > b.limit {
		b.cond.Wait()
	}
	if b.closed {
		return false
	}
	b.inUse += n
	return true
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	b.inUse -= n
	b.mu.Unlock()
	b.cond.Broadcast()
}

// close wakes every waiter so a cancelled sweep does not leave workers parked on
// a budget nothing will release.
func (b *byteBudget) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cond.Broadcast()
}

// sweepOwned proves ownership of each candidate and deletes the ones that are
// ours. Reads run concurrently, and the plan is updated from a single goroutine
// so the accounting needs no locking.
func sweepOwned(ctx context.Context, store Store, candidates []blob.Stored, opts Options, plan Plan) (Plan, error) {
	type verdict struct {
		b       blob.Stored
		ours    bool
		corrupt bool
		err     error
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := opts.Workers
	if workers < 1 {
		workers = ownerWorkers
	}
	limit := opts.MaxInFlightBytes
	if limit < 1 {
		limit = defaultMaxInFlightBytes
	}
	// Workers bound how many reads the disk sees at once; the budget bounds how
	// much memory they may hold between them. Both are needed: the budget alone
	// would let a thousand tiny blobs run concurrently, and the worker count alone
	// cannot see that six large ones do not fit.
	budget := newByteBudget(limit)
	defer context.AfterFunc(ctx, budget.close)()

	jobs := make(chan blob.Stored)
	results := make(chan verdict)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				cost := b.Size * inspectOverhead
				if !budget.acquire(cost) {
					return // sweep cancelled
				}
				ours, corrupt, err := inspect(ctx, store, b, opts.SymKey)
				budget.release(cost)
				select {
				case results <- verdict{b: b, ours: ours, corrupt: corrupt, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, b := range candidates {
			select {
			case jobs <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	for v := range results {
		switch {
		case v.err != nil:
			plan.Failed++
			plan.note(fmt.Sprintf("%s: ownership check failed: %v", short(v.b.SHA256), v.err))
		case v.corrupt:
			// Unusable by anyone, so ownership does not enter into it.
			plan.Invalid++
			plan.InvalidBytes += v.b.Size
			plan.delete(ctx, store, v.b, opts.Apply)
		case !v.ours:
			plan.NotOurs++
			plan.NotOursBytes += v.b.Size
		default:
			plan.Orphans++
			plan.OrphanBytes += v.b.Size
			// Deleting from this loop keeps it serial, which is fine: an unlink is
			// cheap next to the read that proved the blob was ours.
			plan.delete(ctx, store, v.b, opts.Apply)
		}
	}
	return plan, ctx.Err()
}

// inspect reads a candidate and reports what it is: ours, someone else's, or
// corrupt.
//
// Ownership is proved cryptographically. Forging an AES-GCM tag without the key
// is infeasible, so a successful open is proof it is ours, and a failure means we
// have no right to delete it.
//
// Corruption is checked first, and is the more useful verdict. A blob whose
// content does not hash to the address it is filed under is worthless to *every*
// tenant, not just us: Blossom is content-addressed, so any client that verifies
// what it fetched — ours does — will reject it. Nobody can ever use it, and it
// cannot be re-uploaded while it sits there claiming to exist. Without this check
// such a blob would be filed as "not ours" and kept forever, which is exactly the
// wrong answer for the truncated blobs a disk-full server leaves behind.
func inspect(ctx context.Context, store Store, b blob.Stored, key [32]byte) (ours, corrupt bool, err error) {
	sealed, err := store.Download(ctx, b.SHA256)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return false, false, nil // vanished under us; nothing to delete
		}
		// A client that verifies content addresses reports a mismatch as an error
		// rather than handing the bytes back. That is corruption, not a transport
		// failure, and it is safe to reclaim.
		if strings.Contains(err.Error(), "integrity check failed") {
			return false, true, nil
		}
		return false, false, err
	}
	if hashHex(sealed) != b.SHA256 {
		return false, true, nil
	}
	if _, err := crypt.Open(key, sealed); err != nil {
		return false, false, nil
	}
	return true, false, nil
}

// hashHex is the content address of data.
func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (p *Plan) delete(ctx context.Context, store Store, b blob.Stored, apply bool) {
	if !apply {
		return
	}
	if err := store.Delete(ctx, b.SHA256); err != nil {
		p.Failed++
		p.note(fmt.Sprintf("%s: %v", short(b.SHA256), err))
		return
	}
	p.Deleted++
	p.DeletedBytes += b.Size
}

func (p *Plan) note(msg string) {
	if len(p.FirstErrors) < 10 {
		p.FirstErrors = append(p.FirstErrors, msg)
	}
}

// LiveBlobs is the keep-set: every blob address a live (non-tombstone) entry
// references. Tombstones reference nothing, so a deleted file's blob is
// collectable — which is the point.
func LiveBlobs(entries []*tree.Entry) map[string]struct{} {
	live := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if !e.Live() {
			continue
		}
		if e.BlobHash != "" {
			live[e.BlobHash] = struct{}{}
		}
		// A chunked entry's own address is empty; its parts are what the store
		// holds, and every one of them has to survive the sweep.
		for _, c := range e.Chunks {
			if c.BlobHash != "" {
				live[c.BlobHash] = struct{}{}
			}
		}
	}
	return live
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
