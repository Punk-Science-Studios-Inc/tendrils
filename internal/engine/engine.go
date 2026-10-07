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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
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
	"ca.punkscience.tendrils/internal/rootfs"
	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/syncpath"
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
	rootID      rootid.Identity
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
	// fs is the root opened for the current pass; nil between passes.
	fs *rootfs.FS
	// platform, when set, replaces the root's probed naming rules. Tests use it to
	// exercise Windows rules on Linux.
	platform *syncpath.Platform
	// faultHook is called at each persisted stage of a mutation, for crash tests.
	faultHook func(stage, path string)
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
	// Unavailable counts paths, and unreadable or unsupported places, the scan
	// could not observe. Nothing under them is synced or deleted until it can be.
	Unavailable int
	// Blocked counts paths this device cannot sync: invalid names, names this
	// platform cannot represent, and names that collide by case. They are left
	// untouched here and keep syncing everywhere else.
	Blocked int
	// Recovering counts interrupted operations that could not be finished yet.
	// Their paths are left alone until they are.
	Recovering int
	// Paused says why no work was done at all — the root is missing, is not the
	// enrolled folder, or the ignore rules cannot be read. Empty when syncing.
	Paused string
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
// every seal/open in a Sync reuses it. rootID is the identity recorded when root
// was enrolled; a pass does nothing unless root still carries it.
func New(root string, rootID rootid.Identity, id *keys.Identity, idx *index.Store, blobs BlobStore, events EventStore, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	symKey, err := id.SymmetricKey()
	if err != nil {
		return nil, fmt.Errorf("engine: derive symmetric key: %w", err)
	}
	return &Engine{
		root:   root,
		rootID: rootID,
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
//
// A root that is missing or is not the enrolled folder pauses the pass before
// anything is read: an unmounted drive's empty mountpoint scans exactly like a
// tree whose every file was deleted.
func (e *Engine) Sync(ctx context.Context) error {
	fsys, err := rootfs.OpenVerified(e.root, e.rootID)
	if err != nil {
		return e.pause(err)
	}
	if e.platform != nil {
		fsys.SetPlatform(*e.platform)
	}
	e.fs = fsys
	defer func() {
		fsys.Close()
		e.fs = nil
	}()
	retries, err := e.idx.Retries()
	if err != nil {
		return fmt.Errorf("engine: read retry state: %w", err)
	}
	recovering, err := e.recover(retries, time.Now())
	if err != nil {
		var gone *rootid.Unavailable
		if errors.As(err, &gone) {
			return e.pause(err)
		}
		return fmt.Errorf("engine: recover: %w", err)
	}
	if retries, err = e.idx.Retries(); err != nil {
		return fmt.Errorf("engine: read retry state: %w", err)
	}
	// The ignore file (.tendrilsignore at the root) is itself a synced file, read
	// fresh each pass so edits take effect without a restart. Unreadable rules
	// pause the pass: guessing them empty would publish what they hide.
	ign, err := e.loadIgnore()
	if err != nil {
		return e.pause(err)
	}
	base, err := e.idx.All()
	if err != nil {
		return fmt.Errorf("engine: read index: %w", err)
	}
	mounts, err := e.idx.Mounts()
	if err != nil {
		return fmt.Errorf("engine: read mounts: %w", err)
	}
	// The base doubles as the scan's mtime+size cache: unchanged files are not
	// re-hashed, so a pass over a large tree costs a stat per file, not a full read.
	scanned, err := scan.Walk(e.fs, scan.Options{Base: base, Ignore: ign, Mounts: mounts})
	if err != nil {
		return fmt.Errorf("engine: scan: %w", err)
	}
	local, cov := scanned.Entries, scanned.Coverage
	if n := len(cov.Mounts) - len(mounts); n > 0 {
		e.log.Warn("subordinate mount points are not synced; their contents are held back", "mounts", cov.Mounts[len(mounts):])
		if err := e.idx.AddMounts(cov.Mounts[len(mounts):]); err != nil {
			return fmt.Errorf("engine: record mounts: %w", err)
		}
	}
	e.discardOrphans(scanned.Staged)
	remote, remoteComplete, err := e.fetchRemote(ctx)
	if err != nil {
		return fmt.Errorf("engine: fetch remote: %w", err)
	}

	// Plan first, so the total is known before any action runs — that count is
	// what turns per-file progress into "3 of 12".
	now := time.Now()
	var plan []plannedAction
	var withheld int
	unseen := newUnseen(cov, ign)
	paths := unionPaths(local, base, remote)
	blocked := blockedPaths(e.fs, ign, paths, scanned.Blocked, cov, base, local, remote)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Ignored paths are invisible to reconcile: never published, pulled, or
		// deleted. Any already-synced copy is left frozen in place, not removed.
		if ign.Match(path) {
			continue
		}
		// A path this device cannot represent is left exactly as it is, here and
		// on the relay. It is never tombstoned for being absent.
		if blocked.holds(path) {
			continue
		}
		// An unfinished operation owns its path until it is finished.
		if recovering[path] {
			continue
		}
		// A path the scan could not see is unknown, not deleted, and nothing may
		// be written over what could not be read.
		if unseen.hold(path) {
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
	stats := passStats(local, plan)
	stats.Unavailable = unseen.count()
	if stats.Unavailable > 0 {
		e.log.Warn("parts of the tree could not be observed; nothing under them is synced or deleted",
			"unavailable", stats.Unavailable, "examples", unseen.examples(3))
	}
	stats.Recovering = len(recovering)
	if stats.Recovering > 0 {
		e.log.Warn("interrupted operations are still unfinished; their paths are held", "count", stats.Recovering)
	}
	stats.Blocked = len(blocked.reasons)
	if stats.Blocked > 0 {
		e.log.Warn("paths this device cannot sync are left untouched here",
			"blocked", stats.Blocked, "examples", blocked.examples(3))
	}
	if err := e.idx.SetBlocked(blocked.reasons); err != nil {
		e.log.Warn("could not record blocked paths", "err", err)
	}
	e.reportStats(stats)

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
	// Removals go first. On a case-insensitive root a rename that differs only in
	// case arrives as a delete of one spelling and a write of the other, and the
	// write must not land on the file the delete is about to trash.
	sort.SliceStable(todo, func(i, j int) bool {
		return todo[i].decision.Op == reconcile.OpDeleteLocal && todo[j].decision.Op != reconcile.OpDeleteLocal
	})

	total := len(todo)
	var errs []error
	for i, a := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		if destructive(a.decision.Op) {
			if err := e.verifyRoot(); err != nil {
				return errors.Join(append(errs, err)...)
			}
		}
		e.reportProgress(Progress{Done: i, Total: total, Path: a.path, Op: verb(a.decision.Op)})
		e.log.Info("reconcile", "path", a.path, "op", a.decision.Op.String(), "reason", a.decision.Reason)
		err := e.execute(ctx, a.path, a.decision, a.local, a.remote)
		var gone *rootid.Unavailable
		if errors.As(err, &gone) {
			return errors.Join(append(errs, e.pause(err))...)
		}
		if errors.Is(err, rootfs.ErrChanged) {
			e.log.Info("changed during the pass; deciding again next pass", "path", a.path, "op", a.decision.Op.String(), "err", err)
			continue
		}
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

// Paused is returned by a pass that did no work because the root, or the rules
// for reading it, could not be trusted. It is a state to wait out, not a failure
// of any one path.
type Paused struct{ Cause error }

func (p *Paused) Error() string { return "sync paused: " + p.Cause.Error() }
func (p *Paused) Unwrap() error { return p.Cause }

func (e *Engine) pause(cause error) error {
	p := &Paused{Cause: cause}
	e.reportStats(Stats{Paused: cause.Error()})
	return p
}

// verifyRoot pauses unless the root is still the enrolled folder and the pass's
// handle still holds it.
func (e *Engine) verifyRoot() error {
	if err := e.fs.Verify(); err != nil {
		return e.pause(err)
	}
	return nil
}

// blocked is the set of paths this pass will not act on because this device
// cannot represent them. A blocked directory holds everything beneath it.
type blocked struct {
	reasons map[string]string
}

// blockedPaths finds every known path that is invalid, unrepresentable here, or
// — on a case-insensitive root — would share a file or directory with a
// differently-cased name. Only names that would be live after this pass take
// part in a collision: a spelling this pass removes or tombstones is about to
// go, and counting it would block every case-only rename, from either side.
func blockedPaths(fsys *rootfs.FS, ign *ignore.Matcher, paths []string, scanned map[string]string, cov scan.Coverage, base, local, remote map[string]*tree.Entry) blocked {
	b := blocked{reasons: make(map[string]string)}
	for p, reason := range scanned {
		if !ign.Match(p) && !ign.PruneDir(p) {
			b.reasons[p] = reason
		}
	}
	var live []string
	for _, p := range paths {
		if ign.Match(p) {
			continue
		}
		if err := fsys.Check(p); err != nil {
			b.reasons[p] = err.Error()
			continue
		}
		if liveAfterPass(cov.Observed(p), base[p], local[p], remote[p]) {
			live = append(live, p)
		}
	}
	if fsys.Platform().CaseInsensitive {
		for p, reason := range syncpath.Collisions(live) {
			b.reasons[p] = reason
		}
	}
	return b
}

// liveAfterPass reports whether a name will exist after this pass if it is not
// blocked. An unobserved path is held as it is, so it counts as live if either
// side has it.
func liveAfterPass(observed bool, base, local, remote *tree.Entry) bool {
	if !observed {
		return local.Live() || remote.Live()
	}
	switch reconcile.Decide(base, local, remote).Op {
	case reconcile.OpWriteRemote, reconcile.OpPublishLocal:
		return true
	case reconcile.OpDeleteLocal, reconcile.OpPublishDelete:
		return false
	default:
		return local.Live()
	}
}

func (b blocked) holds(p string) bool {
	if len(b.reasons) == 0 {
		return false
	}
	if _, ok := b.reasons[p]; ok {
		return true
	}
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' {
			if _, ok := b.reasons[p[:i]]; ok {
				return true
			}
		}
	}
	return false
}

func (b blocked) examples(n int) []string {
	keys := make([]string, 0, len(b.reasons))
	for p := range b.reasons {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	if len(keys) > n {
		keys = keys[:n]
	}
	for i, p := range keys {
		keys[i] = b.reasons[p]
	}
	return keys
}

// destructive reports whether an action removes or replaces local content or
// tells other devices to. Each is preceded by a fresh root check, so a drive
// unmounted mid-pass stops the pass instead of finishing it on an empty folder.
func destructive(op reconcile.Op) bool {
	switch op {
	case reconcile.OpDeleteLocal, reconcile.OpPublishDelete, reconcile.OpWriteRemote:
		return true
	}
	return false
}

// unseen tracks which scan gaps held paths back this pass, so status can report
// both the known paths it could not judge and the unreadable places holding
// nothing known — an unsupported file the owner has just created, say.
type unseen struct {
	cov  scan.Coverage
	ign  *ignore.Matcher
	hits map[string]int
	held int
}

func newUnseen(cov scan.Coverage, ign *ignore.Matcher) *unseen {
	return &unseen{cov: cov, ign: ign, hits: make(map[string]int)}
}

// CountUnavailable counts, over a set of known paths, the ones a scan could not
// observe plus the unobserved places holding none of them — the figure a pass
// reports as Stats.Unavailable. Ignored and reserved paths are not counted.
func CountUnavailable(cov scan.Coverage, ign *ignore.Matcher, paths []string) int {
	u := newUnseen(cov, ign)
	for _, p := range paths {
		if !scan.Reserved(p) && !ign.Match(p) {
			u.hold(p)
		}
	}
	return u.count()
}

func (u *unseen) hold(path string) bool {
	gap, ok := u.cov.GapFor(path)
	if !ok {
		return false
	}
	u.hits[gap]++
	u.held++
	return true
}

func (u *unseen) count() int {
	n := u.held
	for _, g := range u.cov.Gaps {
		if u.hits[g.Path] == 0 && !u.ignored(g.Path) {
			n++
		}
	}
	return n
}

func (u *unseen) ignored(path string) bool {
	return u.ign.Match(path) || u.ign.PruneDir(path)
}

func (u *unseen) examples(n int) []string {
	var out []string
	for _, g := range u.cov.Gaps {
		if len(out) == n {
			break
		}
		if !u.ignored(g.Path) {
			out = append(out, g.Path+": "+g.Reason)
		}
	}
	return out
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
		return e.writeRemote(ctx, path, remote, observed(local), d.ConflictCopy)
	case reconcile.OpDeleteLocal:
		return e.deleteLocal(path, observed(local))
	case reconcile.OpPublishDelete:
		return e.publishDelete(ctx, path)
	default:
		return nil
	}
}

// publishLocal seals the local file, uploads the ciphertext, and publishes an
// event carrying the plaintext identity plus the sealed-blob address.
func (e *Engine) publishLocal(ctx context.Context, local *tree.Entry) error {
	info, err := e.fs.Lstat(local.Path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if info.Size() > e.chunkSize {
		return e.publishLocalChunked(ctx, local)
	}
	plaintext, err := e.fs.ReadFile(local.Path)
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
func (e *Engine) publishLocalChunked(ctx context.Context, local *tree.Entry) error {
	f, err := e.fs.Open(local.Path)
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
func (e *Engine) writeRemoteChunked(ctx context.Context, path string, remote *tree.Entry, want rootfs.Expect, conflictCopy bool) error {
	tmp, err := e.fs.Create(path)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	defer tmp.Abort()

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
	return e.land(tmp, path, remote, want, conflictCopy)
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
func (e *Engine) writeRemote(ctx context.Context, path string, remote *tree.Entry, want rootfs.Expect, conflictCopy bool) error {
	if remote.Chunked() {
		return e.writeRemoteChunked(ctx, path, remote, want, conflictCopy)
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

	tmp, err := e.fs.Create(path)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	defer tmp.Abort()
	if _, err := tmp.Write(plaintext); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return e.land(tmp, path, remote, want, conflictCopy)
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

// observed is the destination state a plan was made against.
func observed(local *tree.Entry) rootfs.Expect {
	if local == nil || local.Deleted {
		return rootfs.ExpectAbsent()
	}
	return rootfs.Expect{Size: local.Size, ModTime: local.ModTime}
}

// loadIgnore reads the sync root's .tendrilsignore into a matcher.
func (e *Engine) loadIgnore() (*ignore.Matcher, error) {
	return IgnoreMatcher(e.fs, e.exclude)
}

// IgnoreMatcher compiles the effective ignore rules for a node: the synced
// .tendrilsignore at root, followed by this node's local patterns. Local rules
// are appended last, so they win — they can hide more, or re-include a shared
// ignored path with '!'. A missing ignore file contributes no rules; one that
// exists but cannot be read is an error, never an empty rule set. Both the
// engine and the daemonless status command call this, so the two cannot
// disagree about which paths this node is responsible for.
func IgnoreMatcher(fsys *rootfs.FS, local []string) (*ignore.Matcher, error) {
	var lines []string
	data, err := fsys.ReadFile(ignore.FileName)
	switch {
	case err == nil:
		lines = strings.Split(string(data), "\n")
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("read ignore rules: %w", err)
	}
	return ignore.Compile(append(lines, local...)), nil
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

// conflictCopyPath inserts the conflict marker, a short key tag, a timestamp and
// a random suffix before a path's extension. Devices sharing one key share the
// tag, so the time and randomness are what keep their copies apart.
func conflictCopyPath(p, pubkey string, now time.Time) string {
	tag := pubkey
	if len(tag) > 8 {
		tag = tag[:8]
	}
	var r [4]byte
	rand.Read(r[:])
	ext := path.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	return stem + scan.ConflictMarker + tag + "-" + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(r[:]) + ext
}
