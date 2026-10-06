// Package engine is the orchestrator that drives Tendrils' sync loop. It owns no
// new rules — the conflict decision lives in internal/reconcile — it only wires
// the pure core to the outside world: scan the disk, ask the relay what it
// knows, feed both plus the index base to the reconciler, and execute the single
// action it returns (upload+publish, pull+write, publish a tombstone, or trash a
// file).
//
// One Sync pass is the whole product: it is the periodic reconcile, the
// on-startup catch-up, and the new-device bootstrap (reconcile-from-empty is
// just a Sync with an empty index) all at once.
//
// The network-facing dependencies are interfaces (EventStore, BlobStore) so the
// engine runs headless and is tested end-to-end with in-memory fakes and a temp
// directory — no relay, no Blossom server, no Fyne.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/crypt"
	"ca.punkscience.tendrils/internal/ignore"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/reconcile"
	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/tree"
)

// EventStore is the relay side: publish one file-entry event, and fetch every
// file-entry event the owner's key has published. The concrete implementation is
// a go-nostr relay client; tests use an in-memory fake.
type EventStore interface {
	Publish(ctx context.Context, evt *nostr.Event) error
	// Fetch returns every file-entry event the owner's key has published, and
	// whether that answer is the complete set. The second return value is not
	// advisory: absence of a path from an *incomplete* set is not evidence that
	// the path is unpublished, and treating it as such is what republishes a whole
	// tree. See Sync's withheldByPartialView.
	Fetch(ctx context.Context, pubkey string) (evts []*nostr.Event, complete bool, err error)
}

// BlobStore is the file-content side. *blob.Client satisfies it directly.
type BlobStore interface {
	Upload(ctx context.Context, data []byte) (blob.Descriptor, error)
	Download(ctx context.Context, sha256 string) ([]byte, error)
	// Has reports whether the store already holds this blob at this exact size.
	// The size is what makes a skipped upload safe — see blob.Client.Has.
	Has(ctx context.Context, sha256 string, size int64) (bool, error)
}

// Progress is the engine's position within a Sync pass, emitted to the callback
// registered with OnProgress so a UI or the status endpoint can show live work.
// It is file-granular: which action, out of how many, on which path.
type Progress struct {
	Done  int    // actions completed so far this pass
	Total int    // actions planned for this pass
	Path  string // the path being acted on now; empty when the pass is finished
	Op    string // human verb for the current action: "uploading", "downloading", ...
}

// Engine holds the wiring for one synced folder on one device.
type Engine struct {
	root        string
	id          *keys.Identity
	symKey      [32]byte
	idx         *index.Store
	blobs       BlobStore
	events      EventStore
	log         *slog.Logger
	report      func(Progress)
	statsReport func(Stats)
	chunkSize   int64
	// exclude is this node's own ignore patterns, from config.json. They are
	// applied after the synced .tendrilsignore, so a device can hide a subtree
	// from itself without changing what any other device syncs.
	exclude []string
}

// OnProgress registers a callback invoked as each planned action begins and once
// more when the pass finishes (Path empty, Done==Total). Optional; nil disables
// reporting. It is called synchronously from Sync's goroutine — keep it cheap and
// non-blocking.
func (e *Engine) OnProgress(fn func(Progress)) { e.report = fn }

func (e *Engine) reportProgress(p Progress) {
	if e.report != nil {
		e.report(p)
	}
}

// Stats is a pass's outstanding-work summary.
type Stats struct {
	Pending   int // local changes still to push (uploads and tombstones)
	Conflicts int // conflict copies sitting in the tree, awaiting the owner
	Deferred  int // paths held back by retry backoff after repeated failures
}

// OnStats registers a callback given, once per pass, that pass's outstanding
// work. The engine has all of it in hand from the scan and plan, so a status
// query can report it without an expensive rescan of its own. Optional; nil
// disables reporting.
func (e *Engine) OnStats(fn func(Stats)) { e.statsReport = fn }

func (e *Engine) reportStats(s Stats) {
	if e.statsReport != nil {
		e.statsReport(s)
	}
}

// SetExclude supplies this node's per-device ignore patterns (config.json's
// "exclude"). They are compiled alongside the synced .tendrilsignore on every
// pass, so a change takes effect without rebuilding the engine. A path either
// set ignores is invisible to reconcile — never pulled, published, trashed or
// tombstoned — which is what lets a node drop a subtree locally while every
// other node keeps syncing it. Call before Sync; not safe to call concurrently.
func (e *Engine) SetExclude(patterns []string) { e.exclude = patterns }

// plannedAction is one path the reconciler decided needs work, captured up front
// so the pass knows its total before executing (that count is what "3 of 12" needs).
type plannedAction struct {
	path     string
	decision reconcile.Decision
	local    *tree.Entry
	remote   *tree.Entry
	// deferred marks a path still inside its retry backoff: planned, counted as
	// outstanding work, but not acted on this pass.
	deferred bool
}

// New builds an Engine. It derives the blob-encryption key from id up front so
// every seal/open in a Sync reuses it.
func New(root string, id *keys.Identity, idx *index.Store, blobs BlobStore, events EventStore, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	symKey, err := id.SymmetricKey()
	if err != nil {
		return nil, fmt.Errorf("engine: derive symmetric key: %w", err)
	}
	return &Engine{
		root:   root,
		id:     id,
		symKey: symKey,
		idx:    idx,
		blobs:  blobs,
		events: events,
		log:    log,

		chunkSize: ChunkSize,
	}, nil
}

// Sync runs one full reconcile pass over every path known locally, in the index,
// or on the relay. Per-path failures are collected and joined, not fatal: one
// unreadable file or unreachable blob must not stall the rest of the tree.
func (e *Engine) Sync(ctx context.Context) error {
	base, err := e.idx.All()
	if err != nil {
		return fmt.Errorf("engine: read index: %w", err)
	}
	// The base doubles as the scan's mtime+size cache: unchanged files are not
	// re-hashed, so a pass over a large tree costs a stat per file, not a full read.
	local, err := scan.Tree(e.root, base)
	if err != nil {
		return fmt.Errorf("engine: scan: %w", err)
	}
	remote, remoteComplete, err := e.fetchRemote(ctx)
	if err != nil {
		return fmt.Errorf("engine: fetch remote: %w", err)
	}
	retries, err := e.idx.Retries()
	if err != nil {
		return fmt.Errorf("engine: read retry state: %w", err)
	}
	// The ignore file (.tendrilsignore at the root) is itself a synced file, read
	// fresh each pass so edits take effect without a restart.
	ign := e.loadIgnore()

	// Plan first, so the total is known before any action runs — that count is
	// what turns per-file progress into "3 of 12".
	now := time.Now()
	var plan []plannedAction
	var withheld int
	for _, path := range unionPaths(local, base, remote) {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Ignored paths are invisible to reconcile: never published, pulled, or
		// deleted. Any already-synced copy is left frozen in place, not removed.
		if ign.Match(path) {
			continue
		}
		d := reconcile.Decide(base[path], local[path], remote[path])
		if d.Op == reconcile.OpNone {
			continue
		}
		// An incomplete read of the relay cannot be read as "the set has never
		// heard of this path". Acting on that mistake is how a device republishes
		// a tree it has already published, so hold those paths back entirely.
		if !remoteComplete && withheldByPartialView(d, base[path], local[path], remote[path]) {
			withheld++
			continue
		}
		plan = append(plan, plannedAction{
			path:     path,
			decision: d,
			local:    local[path],
			remote:   remote[path],
			deferred: retries[path].NextAttempt.After(now),
		})
	}

	if withheld > 0 {
		e.log.Warn("relay view incomplete: leaving already-published paths alone this pass",
			"paths", withheld, "read", len(remote))
	}
	// A path too long for the relay to index is a path the relay cannot replace,
	// so every publish of it is kept forever rather than superseding the last.
	// Only worth saying on a pass that is actually about to add one — on a settled
	// tree it says nothing.
	if n := overlongPublishes(plan); n > 0 {
		e.log.Warn("publishing paths a relay cannot replace: their old events will accumulate on the relay",
			"paths", n, "limit_bytes", nostrevent.MaxIndexedTagValue)
	}

	// Publish the pass's outstanding-work counts for the status endpoint, so a
	// query reports these from the pass the daemon already ran rather than paying
	// for its own full-tree rescan. Deferred paths are counted as pending too:
	// work held back by backoff is still outstanding, and hiding it would report
	// a tree as synced when it is not.
	e.reportStats(passStats(local, plan))

	// Acting only on what is not in backoff keeps a permanently-failing file from
	// consuming a slot and two log lines every pass, and keeps the progress total
	// honest about what this pass will actually attempt.
	todo := make([]plannedAction, 0, len(plan))
	for _, a := range plan {
		if !a.deferred {
			todo = append(todo, a)
		}
	}
	if n := len(plan) - len(todo); n > 0 {
		e.log.Info("paths held back by retry backoff", "count", n)
	}

	total := len(todo)
	var errs []error
	for i, a := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.reportProgress(Progress{Done: i, Total: total, Path: a.path, Op: verb(a.decision.Op)})
		e.log.Info("reconcile", "path", a.path, "op", a.decision.Op.String(), "reason", a.decision.Reason)
		err := e.execute(ctx, a.path, a.decision, a.local, a.remote)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.path, err))
			e.log.Error("action failed", "path", a.path, "op", a.decision.Op.String(), "err", err)
		}
		e.recordAttempt(a.path, retries[a.path], err, time.Now())
	}
	e.reportProgress(Progress{Done: total, Total: total}) // pass finished, idle

	if err := e.idx.SetLastReconcile(time.Now()); err != nil {
		errs = append(errs, fmt.Errorf("record last-reconcile: %w", err))
	}
	return errors.Join(errs...)
}

// withheldByPartialView reports whether a planned action rests entirely on the
// *absence* of a remote entry that this pass could not prove absent.
//
// Only one decision does: reconcile's "local-only file, publish to set", which
// fires when the folded remote set has no entry for the path. When the read of
// the relay was complete that is a fact and republishing is right — a relay that
// genuinely lost or expired an event must be told again. When the read was
// incomplete it is not a fact, it is a missing answer, and the honest response to
// a missing answer is to do nothing and ask again next pass.
//
// The line is drawn at the index base. A path the index says we already published
// at exactly this content has nothing new to contribute, so silence about it
// costs nothing to ignore. A path with no base — a genuinely new or locally
// changed file — is published anyway: a device on a flaky relay must still be
// able to get its work out, and that publish carries information the set does not
// already have.
//
// Nothing is abandoned by this. A withheld path is re-examined every pass and
// acted on the first time the relay answers in full.
func withheldByPartialView(d reconcile.Decision, base, local, remote *tree.Entry) bool {
	if remote != nil {
		return false // the view did say something about this path
	}
	if d.Op != reconcile.OpPublishLocal {
		return false
	}
	if !base.Live() {
		return false // never synced here: publishing it is new information
	}
	return tree.SameContent(base, local)
}

// overlongPublishes counts planned publishes whose path is longer than a relay
// will index as a d tag — see nostrevent.MaxIndexedTagValue.
func overlongPublishes(plan []plannedAction) int {
	n := 0
	for _, a := range plan {
		switch a.decision.Op {
		case reconcile.OpPublishLocal, reconcile.OpPublishDelete:
			if nostrevent.PathTooLongForRelay(a.path) {
				n++
			}
		}
	}
	return n
}

// passStats derives the status counts from a pass's scan and plan: pending is the
// number of local changes to push (uploads and tombstones, conflict copies
// aside), conflicts the number of conflict copies present in the tree, deferred
// the number of paths backoff held back this pass.
func passStats(local map[string]*tree.Entry, plan []plannedAction) Stats {
	var s Stats
	for path := range local {
		if scan.IsConflictCopy(path) {
			s.Conflicts++
		}
	}
	for _, a := range plan {
		if a.deferred {
			s.Deferred++
		}
		if scan.IsConflictCopy(a.path) {
			continue
		}
		if a.decision.Op == reconcile.OpPublishLocal || a.decision.Op == reconcile.OpPublishDelete {
			s.Pending++
		}
	}
	return s
}

// recordAttempt updates a path's backoff state from the outcome of its action:
// cleared on success, advanced to the next interval on failure. Both are
// best-effort — a lost write only means the next pass judges the path afresh,
// which is the behaviour that existed before backoff did.
func (e *Engine) recordAttempt(path string, prev index.Retry, cause error, now time.Time) {
	if cause == nil {
		// Failures is >=1 in any stored record, so this also asks "was there one?"
		// and spares the common success path a write.
		if prev.Failures > 0 {
			if err := e.idx.ClearRetry(path); err != nil {
				e.log.Warn("could not clear retry state", "path", path, "err", err)
			}
		}
		return
	}
	r := nextRetry(prev, cause, now)
	if err := e.idx.SetRetry(path, r); err != nil {
		e.log.Warn("could not record retry state", "path", path, "err", err)
		return
	}
	e.log.Info("path held back after failure",
		"path", path, "failures", r.Failures, "permanent", r.Permanent, "next_attempt", r.NextAttempt)
}

// verb is the human-facing present participle for an action, shown in progress.
func verb(op reconcile.Op) string {
	switch op {
	case reconcile.OpPublishLocal:
		return "uploading"
	case reconcile.OpWriteRemote:
		return "downloading"
	case reconcile.OpDeleteLocal:
		return "trashing"
	case reconcile.OpPublishDelete:
		return "publishing deletion"
	default:
		return op.String()
	}
}

// execute performs the reconciler's chosen action for one path.
func (e *Engine) execute(ctx context.Context, path string, d reconcile.Decision, local, remote *tree.Entry) error {
	switch d.Op {
	case reconcile.OpPublishLocal:
		return e.publishLocal(ctx, local)
	case reconcile.OpWriteRemote:
		return e.writeRemote(ctx, path, remote, d.ConflictCopy)
	case reconcile.OpDeleteLocal:
		return e.deleteLocal(path)
	case reconcile.OpPublishDelete:
		return e.publishDelete(ctx, path)
	default:
		return nil
	}
}

// publishLocal seals the local file, uploads the ciphertext, and publishes an
// event carrying the plaintext identity plus the sealed-blob address.
func (e *Engine) publishLocal(ctx context.Context, local *tree.Entry) error {
	abs := e.abs(local.Path)
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if info.Size() > e.chunkSize {
		return e.publishLocalChunked(ctx, local, abs)
	}
	plaintext, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	sealed, err := crypt.Seal(e.symKey, plaintext)
	if err != nil {
		return fmt.Errorf("seal: %w", err)
	}
	blobHash, err := e.uploadIfAbsent(ctx, sealed)
	if err != nil {
		return err
	}

	// Re-hash from the bytes we just read so the published identity matches what
	// we uploaded, not a possibly-stale scan result. The scan hashed the file
	// earlier in the pass; anything that rewrote it in between (a tag editor, a
	// still-running copy) would otherwise be published as "these bytes, under the
	// old hash" — an event no device can ever satisfy, because the blob it points
	// at decrypts to content that does not match the hash beside it.
	//
	// ModTime deliberately stays the scan's, older than the file's if it did
	// change: that mismatch is what makes the next scan re-hash the path and
	// publish a corrected entry. Taking a fresh mtime here would instead match the
	// file, and the drift would never be noticed again.
	entry := &tree.Entry{
		Path:     local.Path,
		Sha256:   hashHex(plaintext),
		BlobHash: blobHash,
		Size:     int64(len(plaintext)),
		ModTime:  local.ModTime,
	}
	if err := e.publish(ctx, entry); err != nil {
		return err
	}
	return e.idx.Put(entry)
}

// ChunkSize is the plaintext span sealed into one blob. A file larger than this
// is published as an ordered list of chunks instead of a single blob, which
// bounds memory at roughly twice this figure however large the file is, and
// keeps every request body well under the 100 MB limit a free Cloudflare zone
// imposes on the edge in front of the store.
const ChunkSize = 16 << 20

// publishLocalChunked seals and uploads the file one ChunkSize span at a time,
// publishing the ordered chunk addresses rather than a single blob address.
func (e *Engine) publishLocalChunked(ctx context.Context, local *tree.Entry, abs string) error {
	f, err := os.Open(abs)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	defer f.Close()

	sum := sha256.New()
	buf := make([]byte, e.chunkSize)
	var chunks []tree.Chunk
	var total int64

	for {
		n, readErr := io.ReadFull(f, buf)
		if n > 0 {
			sum.Write(buf[:n])
			total += int64(n)
			sealed, err := crypt.Seal(e.symKey, buf[:n])
			if err != nil {
				return fmt.Errorf("seal chunk %d: %w", len(chunks)+1, err)
			}
			hash, err := e.uploadIfAbsent(ctx, sealed)
			if err != nil {
				return fmt.Errorf("chunk %d: %w", len(chunks)+1, err)
			}
			chunks = append(chunks, tree.Chunk{BlobHash: hash, Size: int64(len(sealed))})
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read: %w", readErr)
		}
	}

	entry := &tree.Entry{
		Path:    local.Path,
		Sha256:  hex.EncodeToString(sum.Sum(nil)),
		Chunks:  chunks,
		Size:    total,
		ModTime: local.ModTime,
	}
	if err := e.publish(ctx, entry); err != nil {
		return err
	}
	return e.idx.Put(entry)
}

// writeRemoteChunked reassembles a chunked file by pulling one chunk at a time
// into a temp file beside the destination, so peak memory stays near one chunk
// rather than the whole file. The plaintext hash is accumulated across chunks
// and checked before anything is put in place: a truncated or reordered chunk
// list fails the check and the temp file is discarded.
func (e *Engine) writeRemoteChunked(ctx context.Context, path string, remote *tree.Entry, conflictCopy bool) error {
	abs := e.abs(path)
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	tmp, err := os.CreateTemp(dir, scan.TempPrefix+"*")
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename below has succeeded
	}()

	sum := sha256.New()
	for i, c := range remote.Chunks {
		sealed, err := e.blobs.Download(ctx, c.BlobHash)
		if err != nil {
			if errors.Is(err, blob.ErrNotFound) {
				return fmt.Errorf("chunk %d of %d (%s…) for %q is not on the configured Blossom server: this device is likely pointed at a different or empty server than the one holding your files — check --blossom / the blossom_servers in your config", i+1, len(remote.Chunks), short(c.BlobHash), path)
			}
			return fmt.Errorf("download chunk %d of %d: %w", i+1, len(remote.Chunks), err)
		}
		plaintext, err := crypt.Open(e.symKey, sealed)
		if err != nil {
			return fmt.Errorf("open chunk %d of %d: %w", i+1, len(remote.Chunks), err)
		}
		if _, err := tmp.Write(plaintext); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		sum.Write(plaintext)
	}

	if got := hex.EncodeToString(sum.Sum(nil)); got != remote.Sha256 {
		return fmt.Errorf("decrypted content %s does not match expected %s", got, remote.Sha256)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if !remote.ModTime.IsZero() {
		if err := os.Chtimes(tmpName, time.Now(), remote.ModTime); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	if conflictCopy {
		if err := e.preserveConflictCopy(path); err != nil {
			return fmt.Errorf("preserve conflict copy: %w", err)
		}
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return e.idx.Put(remote)
}

// uploadIfAbsent stores sealed and returns its content address, skipping the
// transfer when the server already holds it. Because sealing is deterministic,
// two devices that copy in the same file derive the same address, so the second
// one gets to skip a redundant upload rather than push a duplicate of bytes the
// server already has — the win is bandwidth; the address would coincide anyway.
//
// A failed existence check is not fatal: it only costs us the optimisation, and
// re-uploading identical bytes to the same address is harmless.
//
// The size is passed so the check cannot be satisfied by a blob that is present
// but wrong. Skipping an upload is only safe if the server holds *these* bytes;
// trusting bare existence let a truncated blob permanently strand a file while
// every pass reported success.
func (e *Engine) uploadIfAbsent(ctx context.Context, sealed []byte) (string, error) {
	want := hashHex(sealed)
	switch present, err := e.blobs.Has(ctx, want, int64(len(sealed))); {
	case err != nil:
		e.log.Debug("blob presence check failed, uploading anyway", "blob", short(want), "err", err)
	case present:
		return want, nil
	}
	desc, err := e.blobs.Upload(ctx, sealed)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	return desc.SHA256, nil
}

// writeRemote pulls the remote blob, unseals it, and writes it to disk. If the
// local file carried an unpublished change, it is preserved as a conflict copy
// first — a wrong last-writer-wins guess then costs a rename, never data.
func (e *Engine) writeRemote(ctx context.Context, path string, remote *tree.Entry, conflictCopy bool) error {
	if remote.Chunked() {
		return e.writeRemoteChunked(ctx, path, remote, conflictCopy)
	}
	if remote.BlobHash == "" {
		return fmt.Errorf("remote entry has no blob address")
	}
	sealed, err := e.blobs.Download(ctx, remote.BlobHash)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("blob %s… for %q is not on the configured Blossom server: this device is likely pointed at a different or empty server than the one holding your files — check --blossom / the blossom_servers in your config", short(remote.BlobHash), path)
		}
		return fmt.Errorf("download: %w", err)
	}
	plaintext, err := crypt.Open(e.symKey, sealed)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	if got := hashHex(plaintext); got != remote.Sha256 {
		return fmt.Errorf("decrypted content %s does not match expected %s", got, remote.Sha256)
	}

	abs := e.abs(path)
	if conflictCopy {
		if err := e.preserveConflictCopy(path); err != nil {
			return fmt.Errorf("preserve conflict copy: %w", err)
		}
	}
	if err := atomicWrite(abs, plaintext, remote.ModTime); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return e.idx.Put(remote)
}

// deleteLocal moves the local file to the trash (recoverable for the retention
// window) and records the remote tombstone as the new base.
func (e *Engine) deleteLocal(path string) error {
	if err := e.moveToTrash(path); err != nil {
		return fmt.Errorf("trash: %w", err)
	}
	return e.idx.Put(&tree.Entry{Path: path, Deleted: true, ModTime: time.Now()})
}

// publishDelete announces a local deletion as a tombstone and records it.
func (e *Engine) publishDelete(ctx context.Context, path string) error {
	entry := &tree.Entry{Path: path, Deleted: true, ModTime: time.Now()}
	if err := e.publish(ctx, entry); err != nil {
		return err
	}
	return e.idx.Put(entry)
}

// publish signs entry with the device key and hands it to the relay.
func (e *Engine) publish(ctx context.Context, entry *tree.Entry) error {
	evt, err := nostrevent.Sign(entry, e.id.SecretHex())
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	if err := e.events.Publish(ctx, evt); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// fetchRemote asks the relay for the owner's file-entry events and folds them
// into the current per-path truth, keeping the latest publish per path (see
// FoldRemote). A single unparseable event (bad signature, wrong kind) is skipped,
// not fatal. The bool is the fetch's own report of whether that set is complete;
// Sync must have it to tell "no such path" from "no answer".
func (e *Engine) fetchRemote(ctx context.Context) (map[string]*tree.Entry, bool, error) {
	evts, complete, err := e.events.Fetch(ctx, e.id.PublicHex())
	if err != nil {
		return nil, false, err
	}
	out, skipped := FoldRemote(evts)
	for _, err := range skipped {
		e.log.Warn("skipping unparseable event", "err", err)
	}
	return out, complete, nil
}

// FoldRemote folds raw file-entry events into the current per-path truth, and
// returns the parse errors it skipped over.
//
// Exported because anything that needs to know "what does the relay currently say
// is true?" must fold by exactly this rule. Blob collection is the case that
// forced it: a sweeper deciding which blobs are live has to agree with the engine
// about which event wins per path, or it will either spare garbage forever or
// delete a blob a device is about to pull.
//
// The rule is NIP-01's own: for a parameterized replaceable event the current one
// is the greatest created_at for (pubkey, kind, d), ties broken by the lexically
// smallest id. Folding is not optional, because a relay may hand back superseded
// versions — this project's reference relay keeps every version of any path whose
// d tag exceeds 100 bytes, its tag index silently declining to index a value that
// long, so replacement never fires for those paths.
//
// It deliberately does *not* fold by the mtime tag. mtime says which version of
// the file is newer, which is reconcile's question; created_at says which publish
// is current, which is the relay's. Folding by mtime got both wrong: restoring an
// older version of a file was silently undone (the superseded event describing
// the newer version still won the fold), and versions that tied on mtime and hash
// — every republish of an unchanged file, of which the reference relay holds
// tens of thousands — resolved to whichever event the fetch happened to return
// first. That last one is the dangerous half: two folds of the same events in a
// different order could name different blob addresses, so the collector and the
// engine could disagree about which blob is live.
func FoldRemote(evts []*nostr.Event) (map[string]*tree.Entry, []error) {
	out := make(map[string]*tree.Entry, len(evts))
	kept := make(map[string]*nostr.Event, len(evts))
	var skipped []error
	for _, evt := range evts {
		entry, err := nostrevent.Parse(evt)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}
		if prev, ok := kept[entry.Path]; ok && !supersedes(evt, prev) {
			continue
		}
		kept[entry.Path] = evt
		out[entry.Path] = entry
	}
	return out, skipped
}

// supersedes reports whether event a replaces event b as the relay's current
// version of a path: greater created_at wins, and an exact tie goes to the
// lexically smaller id, which is what NIP-01 tells relays to retain. Deciding it
// the same way the relay would is the point — every device then agrees, and a
// relay that does replace properly and one that does not produce the same answer.
func supersedes(a, b *nostr.Event) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID < b.ID
}

// preserveConflictCopy copies the current local file to a conflict-marked
// sibling that will itself sync on the next pass, so the owner sees the losing
// version on every device and resolves it with a rename.
func (e *Engine) preserveConflictCopy(path string) error {
	src := e.abs(path)
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing local to preserve
		}
		return err
	}
	dst := e.abs(conflictCopyPath(path, e.id.PublicHex()))
	return atomicWrite(dst, data, time.Now())
}

// moveToTrash relocates a file under the sync root's trash directory, preserving
// its relative structure; a name clash is disambiguated with a timestamp.
func (e *Engine) moveToTrash(path string) error {
	src := e.abs(path)
	dst := filepath.Join(e.root, scan.TrashDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		dst = fmt.Sprintf("%s.%d", dst, time.Now().UnixNano())
	}
	return os.Rename(src, dst)
}

func (e *Engine) abs(relSlash string) string {
	return filepath.Join(e.root, filepath.FromSlash(relSlash))
}

// loadIgnore reads the sync root's .tendrilsignore into a matcher. A missing or
// unreadable file yields an empty matcher (nothing ignored).
func (e *Engine) loadIgnore() *ignore.Matcher {
	return IgnoreMatcher(e.root, e.exclude)
}

// IgnoreMatcher compiles the effective ignore rules for a node: the synced
// .tendrilsignore at root, followed by this node's local patterns. Local rules
// are appended last, so they win — they can hide more, or re-include a shared
// ignored path with '!'. A missing ignore file contributes no rules. Both the
// engine and the daemonless status command call this, so the two cannot
// disagree about which paths this node is responsible for.
func IgnoreMatcher(root string, local []string) *ignore.Matcher {
	var lines []string
	if data, err := os.ReadFile(filepath.Join(root, ignore.FileName)); err == nil {
		lines = strings.Split(string(data), "\n")
	}
	return ignore.Compile(append(lines, local...))
}

// short trims a hex hash to a readable prefix for error messages.
func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// hashHex is the lowercase-hex SHA-256 of data — the plaintext content identity.
func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// unionPaths returns the sorted set of paths appearing in any of the three maps,
// so a Sync pass visits every path exactly once in a stable order.
func unionPaths(maps ...map[string]*tree.Entry) []string {
	set := make(map[string]struct{})
	for _, m := range maps {
		for p := range m {
			set[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// conflictCopyPath inserts the conflict marker (plus a short device tag, so
// copies from two devices do not collide) before a path's extension.
func conflictCopyPath(path, pubkey string) string {
	tag := pubkey
	if len(tag) > 8 {
		tag = tag[:8]
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	return stem + scan.ConflictMarker + tag + ext
}

// atomicWrite writes data to abs via a temp file in the same directory followed
// by a rename, so a reader never sees a half-written file, and stamps the file's
// mtime to match the source of truth.
func atomicWrite(abs string, data []byte, mtime time.Time) error {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, scan.TempPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(tmpName, time.Now(), mtime); err != nil {
			return err
		}
	}
	return os.Rename(tmpName, abs)
}
