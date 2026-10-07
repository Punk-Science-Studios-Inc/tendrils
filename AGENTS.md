# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Tendrils is a folder sync utility for Windows, Linux, and Android that uses [Nostr](https://github.com/nostr-protocol/nostr) relays as the transport to synchronize files between devices.

Module: `ca.punkscience.tendrils` (Go 1.26.3). UI: [Fyne](https://fyne.io) v2 — one Go UI codebase targets all three platforms.

## Current state

The tested pure core and a runnable CLI exist; the sync engine and network I/O are the next milestone. Everything is **pure Go, no CGO** (bbolt for the index, not SQLite) so Windows/Linux builds stay simple — there is no C compiler on the dev box. Fyne is **not** a dependency and, per the concept brief, the v1 UI is a minimal standalone tray, not the full toolkit; the headless engine is proven first.

Built and tested (`go test ./...` green):

| Package | Responsibility |
|---|---|
| `internal/tree` | `Entry` — the one shared type (path, sha256, size, mtime, deleted). Dependency-free leaf. |
| `internal/keys` | Parse nsec/hex, derive the AES-256 blob key via HKDF-SHA256 (domain-separated, no salt so it is reproducible from the key alone). |
| `internal/crypt` | Blob encryption at rest: AES-256-GCM, `nonce‖ciphertext`. The nonce is **deterministic** — SIV-style, `HMAC-SHA256(key, domain‖plaintext)[:12]` — so the same file under the same key always seals to the same bytes and therefore the same Blossom address. |
| `internal/reconcile` | The pure conflict decision: LWW-by-mtime, delete-is-absolute, re-creation-honoured, conflict copies. Delete-is-absolute now includes **tombstone re-assertion**: a base tombstone facing a live remote entry no newer than it republishes the tombstone rather than pulling the file back, so a concurrent edit that displaced the delete on the relay cannot resurrect it. One test per Gherkin scenario. **The correctness-critical heart.** |
| `internal/index` | bbolt store of the last-synced `Entry` per path (the reconcile "base") + last-reconcile time. Retains tombstones. Also holds per-path **`Retry`** state (failure count, next-attempt time, cause, permanent flag) in its own bucket — created on open, so an index from an older build upgrades in place. |
| `internal/scan` | Walk the sync root → `Entry` map (sha256, mtime) **plus `Coverage`**: unreadable folders, unreadable files, unsupported entries (symlinks, junctions) and subordinate mounts become `Gap`s instead of failing the scan. Ignored files are never opened; a directory is pruned only when no later `!` rule could re-include anything beneath it (`ignore.PruneDir`). Skips Tendrils bookkeeping (`Reserved`: trash, temp files, root marker); conflict-copy naming (`ConflictMarker`). |
| `internal/rootid` | Sync-root identity: a `.tendrils-root` marker (random ID) plus the filesystem ID (Linux `f_fsid`, Windows volume serial), recorded in `config.json` at enroll/adopt. `Verify` is run before every pass and every destructive action. See "An absent root is not an empty tree" below. |
| `internal/syncpath` | Pure wire-path validator. **Invalid** (absolute/drive paths, traversal, empty segments, NUL, bad UTF-8, bookkeeping names) vs **blocked** on this platform (Windows reserved names, `:` streams, forbidden characters, trailing dot/space, 8.3 aliases, components > 255). `Collisions` finds case-only clashes of whole paths or parent dirs. Owns the bookkeeping names. See "Every path goes through the root handle" below. |
| `internal/rootfs` | The only way sync code touches the tree: an `os.Root` opened per pass. Validates every path, refuses non-directory parents (symlinks, junctions), re-verifies root identity and that the path still leads to the held directory before every mutation, and on a case-insensitive root refuses a write whose components exist in another case. Atomic `Create`/`Commit`/`Abort`, `WriteFile`, `Trash`. |
| `internal/ignore` | gitignore-style matcher (`.tendrilsignore` subset: `#`, `!`, trailing `/`, anchored, `* ? **`). Used for both the **shared** synced ignore file and each node's **local** `exclude` patterns — see "Per-node exclusions" below. |
| `internal/nostrevent` | `Entry` ⇄ Nostr event codec. Parameterized replaceable event, kind `31337`, `d`=path, `x`=sha256, `mtime`/`deleted` tags. **`created_at` = publication time, not mtime** — see "Two clocks" below. Signs/verifies. |
| `internal/config` | State dir (`$TENDRILS_HOME` or OS config dir), local `config.json` (overrides discovery, plus the per-node **`exclude`** patterns), device key at rest (0600 file — deliberately not a keychain). |
| `internal/blob` | Blossom (BUD-01/02) client: `Upload`/`Download`/`Has` opaque bytes addressed by sha256, with a signed per-request kind-24242 auth event. Verifies content addresses locally on both directions. Pure transport — no knowledge of encryption or plaintext hashes. A 413 is typed as **`ErrTooLarge`** so the engine can tell "retrying cannot fix this" from a transient failure; error bodies are collapsed and truncated, and a proxy's HTML error page is dropped entirely (quoting one per rejected file is what made the daemon log unreadable). |
| `internal/tree` `internal/nostrevent` | `Entry` carries `BlobHash` (sealed-blob Blossom address) alongside `Sha256` (plaintext identity); the event codec round-trips it in a `blob` tag. |
| `internal/engine` | Orchestrates one full `Sync` reconcile pass: scan → read index base → fetch relay truth → `reconcile.Decide` per path → execute (seal+upload+publish / pull+unseal+atomic-write / trash / publish-tombstone). Network deps are `EventStore`/`BlobStore` interfaces (`*blob.Client` satisfies `BlobStore`), so it runs headless. Tested end-to-end with in-memory fakes: publish, pull, two-device convergence, delete propagation to trash, and conflict-copy preservation. One `Sync` is also the bootstrap and the periodic reconcile. **`publishLocal` hashes the bytes it actually read and uploaded**, never the scan's — see "Publish the hash you uploaded" below. Per-path **retry backoff** (`backoff.go`) holds repeatedly-failing paths back instead of retrying them every pass. A pass acts on the relay's *absence* of a path only when the fetch reported itself complete (`withheldByPartialView`), and `FoldRemote` collapses retained replaceable events by **`created_at`**, the relay's own rule — see "A read that failed is not an empty set" below. |
| `internal/relay` | Concrete `engine.EventStore`: publishes/fetches file-entry events over go-nostr websockets to one or more relays. Persistent, lazily-reconnecting connections; publish to all (succeed if any accepts), fetch unions + dedupes by event ID. `Fetch` **paginates** with a NIP-01 `until` cursor (500/page) because relays cap one REQ — the reference relay at `relay.towerofsong.ca` is `UseEventstore(db, 400)`. Every page is **proven complete by EOSE**, never by go-nostr's `QuerySync`, which reports a timeout, a `CLOSED` and a dead socket alike as an empty, error-free result; a page that cannot be read is retried and then fails the fetch. `Fetch` returns a **completeness flag** alongside the events — see "A read that failed is not an empty set" below. Also `FetchServerList` for Blossom discovery (kind-10063), under the same rule. Tested against a minimal in-process NIP-01 relay (`coder/websocket`) that enforces the same 400 cap and can go silent, close a subscription, or hang up mid-walk on demand. |
| `internal/serverlist` | Blossom server **discovery**: `Entry`-free codec for the BUD-03 kind-10063 "User Server List" (sign/parse a `[]string` of server URLs under the owner's key). `Shareable` strips loopback/unspecified hosts (never advertise `127.0.0.1` to the whole identity); `Merge` unions lists so devices republish instead of clobbering. The daemon publishes the union when it has a server and discovers from it when it doesn't — so a later device enrolls with just `--key`/`--relay`. |
| `internal/gc` | Orphan blob reclamation: folds the relay's current truth into a keep-set, classifies every stored blob (kept / unreferenced / invalid / too-recent / not-ours), and deletes only what it can justify. Dry-run by default. See "Blob GC" below — it is the one operation syncing cannot undo. |
| `internal/buildinfo` | Which build is running. Release builds stamp version/commit/date via `-ldflags -X`; everything else falls back to the toolchain's embedded module and VCS data, so a source build still reports a real commit and a dirty flag. Both binaries take the same stamp. |
| `internal/selfupdate` | Finding, verifying and installing a newer release. Queries the public GitHub releases API (endpoint is a field, so tests drive it from an `httptest.Server`), compares semver, streams the archive to a staging dir **inside the destination**, verifies it against the release's `checksums.txt`, **execs the new binary and checks the version it reports**, then renames it into place. Caches the check in `update.json` (24 h; failures back off 15 m → 6 h). Also `Boundary`, the coordinated-upgrade gate fed by the release's `release.json`. No token, no credentials, no cgo. |
| `cmd/tendrils` | **cobra** CLI: `keygen`, `enroll` (`--key`/`--root`/`--exclude`, reuses stored key), `status` (pending/conflicts/**stuck**/**unavailable**/**paused** from scan-vs-index), `adopt` (verify and record the root's identity; required once for older enrollments), `exclude` (`list`/`add`/`remove` this device's local opt-outs), `daemon` (builds the engine from config and runs a periodic reconcile loop; `--interval`, graceful shutdown on Ctrl-C), `gc` (reclaim orphaned blobs; `--apply` to delete), `upgrade` (`--check`/`--version`/`--force`/`--yes`), `version`. Root sets `cobra.Command.Version`, so `--version`/`-v` work too, and its `PersistentPostRun` prints the cached update notice and schedules the next background check. |

Not yet built (next milestones): **relay** discovery (NIP-65/kind-10002 — until then `daemon` still requires `--relay` set at enroll; Blossom-server discovery via kind-10063 **is** built, so `--blossom` is optional on later devices), fsnotify watch (with debounce/settle, to complement the periodic reconcile), Blossom multi-server mirroring (daemon uses the first server — which is also why a file too large for that one server simply cannot sync, however many are configured), and the tray. (Blossom **orphan GC** is now built — see "Blob GC" below.)

## Per-node exclusions (selective sync)

There are two ignore sources, and the difference between them is the whole point:

- **`.tendrilsignore`** at the sync root is itself synced. One edit applies to
  every device — the rules are shared.
- **`"exclude"`** in `config.json` is per-node and never published. It lets one
  device opt out of a subtree while every other device keeps syncing it — e.g. a
  Raspberry Pi that cannot hold a 188 GB music folder still syncs the rest of the
  vault, and the music is unaffected everywhere else.

`tendrils exclude add music/` / `list` / `remove` edits the local list (or set it
at enrollment with `enroll --exclude`, or edit `config.json` directly). The engine
compiles the shared file's lines and then the node's patterns into one matcher
(`engine.IgnoreMatcher`), so the node's rules are applied last and win — they can
hide more, or re-include a shared-ignored path with `!`. A path either matcher
ignores is invisible to `reconcile`: **never pulled, published, trashed or
tombstoned**, and any local copy is left frozen in place.

That last property is both the feature and the trap. Excluding a path does
**not** delete an already-synced local copy. To reclaim the space you exclude,
then delete the local folder — that deletion is invisible too, so it neither
tombstones the path for the rest of the set nor comes back. The index base still
records those paths, which is harmless: GC unions the base into its keep-set, and
the relay still references the blobs from the devices that kept syncing them.

`status` applies the same matcher in its daemonless path, so an excluded-then-
deleted path does not read as forever-pending. A change to `exclude` needs a
daemon restart (patterns are captured at startup, though the matcher is compiled
per pass); a change to `.tendrilsignore` does not.

## Publish the hash you uploaded
`publishLocal` reads the file, seals it, uploads it, and publishes an event describing it. The `Sha256` in that event must be the hash of **the bytes it just read** — never `local.Sha256` from the scan earlier in the same pass.

A file rewritten in between (a tag editor, a copy still running) otherwise gets published as "these bytes, under the old hash": the blob is fine, the hash beside it is not, and the two do not agree. Nothing can consume that event. Every puller downloads the blob, decrypts it successfully, fails `hashHex(plaintext) != remote.Sha256`, and rejects it — forever, since the reconciler keeps choosing to pull. This shipped as a real bug (the comment said "re-hash from the bytes we just read"; the code did not) and poisoned one path in the author's tree for ten hours until the publishing device came back and republished.

`ModTime` deliberately stays the scan's, older than the file's if the file did change. That mismatch is what makes the next scan re-hash the path and publish a corrected entry, so the window is self-healing. Taking a fresh mtime here instead would match the file, and the drift would never be noticed again.

## Presence must mean correctness

Three layers assume that a blob existing at an address means the bytes at that address are the bytes the address names. That assumption was violated, and it produced the worst failure mode the project has had — silent, permanent, and self-reporting as success.

`blossomd` stored uploads with `os.WriteFile`, which opens `O_CREATE|O_TRUNC` and *then* writes. On `ENOSPC` the create had already succeeded, so a **zero-byte file was left at the blob's real content address**. From there:

1. `HEAD` on that address returned 200.
2. `engine.uploadIfAbsent` asked `Has()`, was told yes, and skipped the upload.
3. It published an event pointing at an empty blob — internally consistent, permanently unsatisfiable.
4. Every pulling device failed `blob: integrity check failed`, and no device ever re-uploaded, because the server kept saying it had the blob.

One transient disk-full error became an unrecoverable file, and the pass that did it logged success. On the author's store this had been happening for a week: **11,434 zero-byte blobs, 14 live files (1.2 GB) pointing at one.**

Three fixes, and all three matter:

- **`storeStream`** copies the upload to a temp file in the same directory, fsyncs, and renames. Presence now means the bytes are down. On any failure nothing is left behind to lie about. It also hashes as it copies, so the blob is never held in memory — see "Memory is bounded by bytes, not by count" below.
- **`Has(sha, size)`** takes the expected size and reports absent on a mismatch. Deliberately conservative: a missing `Content-Length` also counts as absent. A needless re-upload costs bandwidth; a wrongly-skipped one abandons the file.
- **blossomd reports a 0-byte blob as 404** unless the address really is the empty hash, so pre-existing corruption stops lying immediately rather than waiting for a sweep.

The general rule: **never let "it exists" stand in for "it is correct" when the consequence of being wrong is silent and permanent.**

## An absent root is not an empty tree

`internal/rootid`, `scan.Coverage`, `tendrils adopt`. This is the local twin of
the next section: a scan that could not see something must not report it as
gone.

An unmounted drive leaves its mountpoint behind as an empty directory. Scanned,
it is indistinguishable from the owner deleting every file, and `reconcile`
would tombstone the whole tree on every device. Three rules prevent that:

- **The root must prove it is the root.** Enrollment writes `.tendrils-root` (a
  random ID) and records it plus the filesystem ID in `config.json`; neither is
  synced, and `scan.Reserved` keeps the marker out of every plan. A pass whose
  root is missing, unmarked, marked by another enrollment or on another
  filesystem returns `*engine.Paused` and does nothing. Each delete, tombstone
  and overwrite re-verifies first, so a drive pulled mid-pass stops the pass.
- **Only observed absence is deletion.** Scan failures below the root become
  `Gap`s. A path inside one is skipped entirely: not deleted, not published, not
  written over. Such paths are counted in `Stats.Unavailable`, and so are gaps
  with nothing known beneath them. They are never hidden. A filesystem mounted
  inside the root is **not synced**: it is recorded in the index's `mounts`
  bucket and stays a gap even after it is unmounted. That covers files an older
  build synced from it. `adopt` forgets those boundaries.
- **Adoption is explicit.** An enrollment with no recorded identity (any older
  build) makes `daemon` refuse to start. `enroll` will not mark an unmarked
  folder on a device that already has history. Both point to `tendrils adopt`,
  which refuses a folder holding none of the last-synced files unless
  `--force`. Silently adopting would adopt an empty mountpoint just as readily.

FAT, exFAT and XFS derive `f_fsid` from the device number. A removable drive
enumerated in a different order can therefore read as "another filesystem" and
pause until it is adopted again. That is a false pause, never a false delete.
Real-mount tests live behind `-tags mountintegration` (see
`internal/engine/mount_linux_test.go` for the `unshare` harness, and the CI job).

## Every path goes through the root handle

`internal/syncpath`, `internal/rootfs`. A wire path is a name another device
signed, so it is input, not a filename. Before this, `filepath.Join(root, path)`
would follow `../outside/x` out of the folder, and a symlink or junction swapped
in above a target would carry a write wherever it pointed.

- **One boundary.** Engine, scan, status, `adopt` and `repair` all read and
  write through `rootfs`. Nothing in sync code joins a root path by hand.
- **Invalid is not blocked.** An invalid name is never acted on anywhere. A
  blocked one is valid elsewhere: this device leaves it alone, the relay keeps
  it, other devices keep syncing it. Both count in `Stats.Blocked`, are recorded
  in the index's `blocked` bucket (so daemonless `status` sees them), and are
  **never tombstoned** for being absent here. Names are never rewritten or
  renamed automatically.
- **Case.** The root is probed (the marker in another case) rather than assumed
  from the OS: Linux mounts exFAT/VFAT case-insensitively. On such a root, names
  that would be live after the pass and fold together — as whole paths or as
  parent dirs — are all blocked. A tombstoned spelling does not count, so a
  case-only rename applies; removals run before writes in a pass for the same
  reason. Folding is `strings.ToLower`, not NTFS's upcase table.
- **Not covered.** `os.Root` follows a symlink that stays inside the root; the
  parent check catches the ordinary case but not a race inside the root (it can
  never escape). Unicode normalization (NFC vs NFD) is not folded. exFAT on
  Linux also rejects Windows' forbidden characters; such a write fails and backs
  off rather than being blocked up front.

## A read that failed is not an empty set

The relay is asked one question per pass — "what has this key published?" — and
the answer decides whether the engine has anything to say. `reconcile` publishes
a file precisely when the folded remote set has no entry for it, so **a truncated
answer that passes itself off as complete makes a device republish its entire
tree**.

That is exactly what happened. `go-nostr`'s `QuerySync` returns
`(whatever arrived, nil)` in three different failures: its own undocumented
7-second deadline, a relay-sent `CLOSED` (auth-required, rate-limited), and the
websocket dying underneath it. `fetchAll` read a zero-event page as "walked past
the oldest event" and returned success. One dropped connection therefore looked
identical to a key that had never published anything, and the next pass re-sealed,
re-uploaded and re-announced every file in the tree.

Measured on the reference fleet before the fix: **114,747 events for a 5,188-path
tree**, and a single pass on 2026-08-23 that republished **5,048 paths in 33
minutes** — 3,973 of those paths' surviving events all sit inside that one burst.
It is self-amplifying: more events means more pages per fetch, more pages means
more chances to hit a page that never completes.

Three rules now hold it:

- **A page is EOSE or it is not an answer.** `queryPageOnce` subscribes and
  collects until end-of-stored-events. A `CLOSED`, a lost connection, a deadline
  or the events channel closing early is an error however many events had already
  arrived. Pages are retried (a fetch is hundreds of pages; one hiccup must not
  stall a big tree) and then the fetch fails loudly.
- **`Fetch` reports completeness, and callers must handle it.** Its signature is
  `([]*nostr.Event, bool, error)` so nobody can quietly ignore the difference
  between "no such path" and "no answer". `gc` refuses to sweep against an
  incomplete view; `repair` says so and carries on (it only ever uploads).
- **The engine acts on absence only when absence is a fact.** Under an incomplete
  view, a path whose index base already records a successful publish of exactly
  this content is left alone — the relay's silence about it is not evidence. A new
  or locally-changed file is still published, because that publish carries
  information the set does not have. Nothing is abandoned: the path is judged
  afresh the first pass that reads the relay in full.

Two smaller holes in the same seam, closed with it:

- **The cursor still has to be forced past a dense second.** `until` is inclusive,
  so a page whose events all share one `created_at` cannot advance it; forcing it
  down guarantees termination but steps over anything at that second beyond the
  relay's per-REQ cap (the reference relay has three such seconds, left by publish
  storms). That walk now reports itself incomplete instead of pretending the
  skipped second was empty — but only once the relay has *proven* it truncates a
  page, since the last page of every ordinary walk also fails to advance. Claiming
  incompleteness on every fetch would be just as bad in the other direction: an
  engine that never trusts a complete view can never repair a relay that really did
  lose an event.
- **A publish that never landed also reported success.** go-nostr's `Publish`
  returns `nil` when the connection dies before the relay's `OK`, so the index
  would record a file as published that the relay never received. `relay.Publish`
  now checks whether the connection survived the call.

Note what an incomplete view can and cannot do. Every destructive action —
`OpWriteRemote`, `OpDeleteLocal` — requires a remote entry the fetch actually
saw, so a partial view can never trash a file or overwrite one. The only thing it
could ever cause was a storm of redundant publishes, and the only thing it can
cause now is a deferred one.

The same rule applies to the kind-10063 Blossom server list: a failed read used
to look like an empty list, and the daemon publishes the *union* of the discovered
list and its own servers — so one bad read would have dropped every server this
device did not happen to have configured. A failed discovery now leaves the
published list alone.

## The relay only indexes 100 bytes of a `d` tag

The amplifier behind that event count, and a genuine limit of the transport
rather than a bug in the loop above.

NIP-01 puts no ceiling on a tag value; real relays do. The reference relay
(khatru over fiatjaf's eventstore) **silently declines to index a tag value longer
than 100 bytes**. Confirmed directly: a `#d` filter for a 103-byte path returns
`0` results while the relay is holding 253 events with exactly that `d` tag.
Since replacing a parameterized-replaceable event means looking up the previous
one by `(kind, pubkey, d)`, replacement never fires for those paths and **every
publish is kept forever**.

The split in the field data is total and has no exceptions: every one of the
4,085 paths at or under 100 bytes has exactly one event on the relay; all 1,103
paths at 101 bytes or more have many, up to 306. A deep music tree hits this
constantly.

Two consequences the client has to live with:

- **`engine.FoldRemote` folds by `created_at`, not by the `mtime` tag.** For a
  long path the client is doing the arbitration the relay could not, and the
  relay's rule is NIP-01's: greatest `created_at` per `(pubkey, kind, d)`, ties to
  the lexically smallest id. Folding by mtime got two things wrong — restoring an
  older version of a file was silently undone (the superseded event describing the
  newer version still won), and republished-but-identical events, which tie on
  both mtime and content hash, resolved to whichever event the fetch happened to
  return first. That last one is the dangerous half: the blob collector folds with
  this same function, and two folds of one event set in different orders could
  name different blob addresses.
- **The daemon warns when it publishes a path it knows the relay cannot replace**
  (`nostrevent.PathTooLongForRelay`), only on passes that actually publish one.

**Fixing it at the source is a coordinated upgrade and is deliberately not done
here.** It means putting something bounded in `d` — `sha256(path)` — and carrying
the real path in its own tag. An old build reads `d` as the path, so it would
write files named after hashes into the tree, and the two builds would see
disjoint namespaces and each republish everything. If it is ever done: ship a
release that *reads* both forms first, wait for the fleet, and only then switch
what it writes. The ~110k superseded events already on the relay cannot be cleaned
up with NIP-09 either — a deletion request is matched by the same broken index —
so they need dropping relay-side.

## Memory is bounded by bytes, not by count

Every byte path in this project is `[]byte` — whole file in memory, no streaming — so peak memory tracks *file size × concurrency*, and neither term is small. The reference store holds 6,630 blobs averaging 32 MB with a 412 MB maximum, on a 1.8 GB Raspberry Pi that also runs the relay and the blob server. On 2026-07-27 a GC sweep drove that host to a load average of 71 and a 178% commit ratio; it swap-thrashed onto its SD card, saturated the SDIO bus, and took the SDIO-attached wifi down with it for half an hour. The host never rebooted and never browned out — from outside it looked exactly like a failing PSU, which is the diagnostic trap worth remembering.

Two ceilings now hold it:

- **`blossomd` streams uploads** (`storeStream`). The buffered version called `io.ReadAll` on the request body; `io.ReadAll` grows by reallocate-and-copy, so at the final growth both buffers are live and the peak is ~2× the blob — ~800 MB for the 412 MB blob, and the observed `MemoryPeak` was 796 MB. The cost of streaming is that the content address is unknown until the last byte, so a duplicate upload can no longer skip the write. That is one wasted temp file against a ceiling that does not move with the largest file anyone syncs.
- **GC counts bytes, not workers** (`gc.Options.MaxInFlightBytes`, default 256 MB). Worker count is a useless memory dial when blob sizes span two orders of magnitude: six slots is 390 MB or 4.8 GB purely by draw. A blob larger than the whole budget is admitted alone rather than refused, because refusing it would deadlock the sweep on exactly the blobs most worth reclaiming. **`gc.Options.MaxInspectBytes` (CLI `--max-inspect-mb`)** is the complementary cap: proving ownership holds the sealed blob *and* its decrypted plaintext at once (~2× the blob), so on a small host the single largest blob is a guaranteed OOM. Over the cap, a candidate is counted in `Plan.TooLarge` and left in place — the sweep reclaims everything it safely can rather than dying on the one blob it cannot. On the 2 GB Pi with a 984 MiB orphan, that is the difference between a sweep and an OOM-kill.

Neither change touches the wire format, the event codec, or blob addressing, so unlike the `created_at` fix this needs no coordinated upgrade — deploy the server where the server runs.

Still `[]byte` and still unbounded by file size: `engine.publishLocal` (`os.ReadFile` then `crypt.Seal`, ~2× the file, but sequential so it is bounded by the largest single file) and `gc.inspect` (`crypt.Open` allocates the plaintext rather than decrypting in place). Streaming `Seal` is not available without changing the sealing format: `crypt.deriveNonce` HMACs the entire plaintext to derive the deterministic nonce, so no ciphertext byte can be emitted until the whole file has been read. Chunked sealing with per-chunk nonces would fix that and the oversized-file 413 problem together, at the price of a format migration.

## Blob GC: refuse rather than guess

`internal/gc` plus `tendrils gc`. Nothing ever reclaimed a blob, so every edit's previous version accumulated — amplified enormously by the random-nonce and `created_at` bugs, both since fixed. Result: **926 GB of blobs for a 181 GB tree**, a full disk, and all syncing stopped.

Deleting a blob is the only operation here that syncing again cannot undo, so the sweep is built to abort rather than approximate:

- **The keep-set is folded, and folded by the engine's rule.** Relays retain superseded replaceable events, so a raw fetch returns *several versions per path* — 11,269 blob addresses across 4,805 files here. `engine.FoldRemote` is exported precisely so GC and the engine cannot disagree about which event wins; if they diverged, GC would either spare all garbage forever or delete a blob a device is about to pull.
- **A partial view never deletes.** A failed relay fetch aborts, and so does a keep-set covering less than `minRelayCoverage` of the paths the device already knows about.
- **A grace period is mandatory.** A blob uploaded seconds ago has no event yet and is indistinguishable from an orphan.
- **Other tenants are untouchable.** Blossom stores carry *no owner attribution*, and a server may be shared (this one allows a second key, used for other projects). So by default a candidate is deleted only once proven ours by decrypting under our key — an AES-GCM tag no other key can forge. That means reading every candidate, which is slow; `--trust-references` skips it and is only correct for a store known to hold one identity's blobs.

Invalid blobs are the one exception to the keep-set: a blob too short to be a sealed blob (< 28 bytes = 12-byte nonce + 16-byte tag) is deleted **even when referenced**, because leaving it is what stops the referencing file from ever being repaired.

`--apply` is required to delete; the default reports only.

## Retry backoff: never every pass, never never again

`internal/engine/backoff.go` plus the index's retry bucket. A pass acts on every path the reconciler says needs work, which is correct right up until an action fails for a reason the next pass will hit identically — then the path is retried every interval forever, burning a planned slot, two log lines, and (for a transient failure) a real upload that gets cut off. Fifteen thousand identical rejections in 48 hours is what prompted this.

- **Transient** (timeouts, 5xx, resets, disk-full): exponential from 1 min, doubling, capped at 1 h.
- **Permanent** (`blob.ErrTooLarge` only): a flat 24 h.

Two properties are load-bearing:

1. **Nothing is ever abandoned.** "Permanent" means *repeating cannot fix it*, not *give up* — what makes a 413 permanent is the server's body limit, and that can change. The longest wait is finite so a raised limit is picked up on its own, with no state for the owner to clear by hand.
2. **Deferred work still counts as pending.** `Stats.Deferred` is a subset of `Stats.Pending`, surfaced by `status` as "Stuck". A tree with an unsyncable file in it is not synced, and backoff must not be able to make it look that way.

Classification is deliberately narrow. Misjudging a timeout as permanent would stall a healthy file for a day over a blip, so only an explicit size rejection qualifies.

## Two clocks: `created_at` vs `mtime`

Every file event carries two timestamps and they answer different questions. Conflating them was the source of several convergence bugs, so keep them apart.

- **`created_at` = when the event was published.** It decides which *publish* survives, because a relay keeps exactly one replaceable event per `(pubkey, kind, d)` and picks the greatest `created_at`. Setting it to the file mtime (as builds before 2026-07-23 did) meant an event describing an old file was silently discarded by the relay as stale — so re-creating a deleted file was unpublishable (a tombstone is stamped `now`, a restored file carries its original older mtime), restoring any older version was a no-op, and the losing device never found out and republished forever.
- **`mtime` tag = when the file last changed.** It decides which *version of the file* wins, in `reconcile`. Nothing else may read `created_at` for arbitration.

`nostrevent.Parse` has always preferred the `mtime` tag, so events from older builds decode identically — only relay tie-breaking changed.

**This is a coordinated upgrade.** A device still on an old build publishes `created_at = mtime`, which now loses to any event a new device published at `now`, so its publishes are silently dropped by the relay. Upgrade every device together; reads are unaffected in both directions.

**Addressing model (resolved):** a Blossom blob is addressed by the SHA-256 of its *stored* (encrypted) bytes, while `tree.Entry.Sha256` is the *plaintext* hash used for file identity, dedup, and the reconciler's same-content check. Sealing is deterministic (keyed synthetic nonce), so the same file under the same key seals to the same bytes and the same content address on every device: two devices that copy in the same file converge on **one** blob instead of uploading a duplicate each, and `engine.uploadIfAbsent` skips the transfer outright when `blob.Has` says the server already holds it. The publishing device still records the sealed address in `tree.Entry.BlobHash` and the event's `blob` tag — the address is derived, not assumed. A puller fetches by `BlobHash` (verified by `blob.Download`), unseals, and verifies the plaintext against `Sha256` — two independent integrity checks. The reconciler compares only `Sha256`, so content that matches is "converged" regardless of how it was sealed, which is what keeps blobs written by the old random-nonce build from causing a re-download.

The tradeoff accepted here is the standard deterministic-encryption one: an observer with access to the Blossom server can tell that two blobs hold identical plaintext. The nonce is keyed, so that equality never spans identities. `crypt.Open` reads the nonce from the blob, so blobs sealed by the earlier random-nonce build still decrypt — no migration, but they also do not dedup against newly sealed copies; clearing those orphans is a separate GC job (not built).

Dependencies: `github.com/nbd-wtf/go-nostr`, `go.etcd.io/bbolt`, `github.com/spf13/cobra`.

## Versioning and releases

`tendrils version`, `tendrils --version`, `blossomd --version`. Every binary can
say what it is, and the daemon prints it in its startup banner.

That is not politeness. The `created_at` change (see "Two clocks" above) is
invisible to the device that loses: its publishes are dropped by the relay and it
is never told. The only way to diagnose a fleet that has drifted is to ask each
device what it runs, so asking has to be possible without a debugger.

- **Version resolution** lives in `internal/buildinfo`. A release build wins,
  stamped by `-ldflags -X ca.punkscience.tendrils/internal/buildinfo.{version,commit,date}`.
  Failing that, the module version the toolchain embeds is used — but *only* if it
  names a real tag. A synthesized pseudo-version (`0.0.0-20260728020249-a81f7ab00a90`)
  is rejected, because reporting one as a release makes an unreleased build look
  like a shipped one. What is left is `dev`, plus the VCS commit and dirty flag
  the toolchain embeds automatically.
- **Both binaries take the same stamp.** `tendrils` and `blossomd` from one
  release always agree about the version, so "what is this host running" has one
  answer, not two.
- **Cutting a release** is `git tag -a vX.Y.Z && git push origin vX.Y.Z`.
  `.goreleaser.yaml` builds linux/darwin/windows × amd64/arm64, archives both
  binaries with the docs, writes `checksums.txt`, and publishes the GitHub
  Release. `-rc`/`-beta` tags publish as prereleases automatically.
- **CI** (`.github/workflows/ci.yml`) runs build, vet, `gofmt -l`, `go mod tidy`
  cleanliness and `go test` on **Linux and Windows** — Windows is in the
  matrix because this project has already shipped two Windows-only defects that
  were invisible on Linux. It also runs a GoReleaser snapshot build and asserts
  the version was stamped, so a broken release config fails on a pull request
  rather than after a tag has been pushed and can no longer be moved. `-race` is
  a **separate Linux job**, not part of the matrix: it requires cgo, and the rest
  of the pipeline is kept runnable with no C compiler anywhere — the same
  constraint that put bbolt in the index instead of SQLite. That job skips
  `internal/relay`: go-nostr's write-pump assigns `r.Connection = nil` unlocked
  while `Relay.close()` reads the same field under a mutex, so `Close()` trips the
  detector by construction. The bug is upstream and no usage pattern here avoids
  it; re-check the exclusion whenever go-nostr is upgraded.
- **The installers download releases**, verify them against `checksums.txt`, and
  only build from source when no release matches the platform. `checksums.txt` is
  therefore part of the published contract — renaming it breaks every installer
  in the field.

## Self-update: the stale device has to be the one that speaks

`internal/selfupdate`, `cmd/tendrils/upgrade.go`, `cmd/tendrils/update.go`,
`docs/UPGRADING.md`.

Versioning above says the only way to diagnose a drifted fleet is to ask each
device what it runs. Asking is a manual per-device chore across Linux, Windows
and a Pi, and the device that most needs asking is the one whose publishes are
being silently dropped. So the device asks on its own behalf and says the answer
out loud.

- **A check never delays a command, and never fails one.** It runs as a detached
  child (`tendrils update-check`, hidden) that writes `update.json`; the *next*
  invocation reads the cache. A goroutine cannot do this job — `tendrils status`
  exits in three milliseconds, long before any HTTP round trip.
- **The lease is written before the child is launched.** A child that dies
  without reporting back (killed with its shell, out of disk) must not leave a
  state where every subsequent command launches another one. Claim, then spawn.
- **Never re-execute a test binary.** Under `go test`, `os.Executable()` is the
  compiled suite; launching it with an unrecognised argument runs every test
  again, and each of those would launch another. `reExecutable()` refuses a
  `.test` binary and anything under the temp dir, and there is a test asserting
  it. This is a fork bomb, not a nuisance.
- **Three states, not two.** "A newer release exists", "there is nothing newer",
  and **"nothing has ever been published"** are distinct, and `ErrNoReleases`
  keeps the third from collapsing into either. It is the state the repository is
  in right now, so it is the first path that runs in the field: reported as "up
  to date" it would be a lie that hides the whole feature, and reported as an
  error it would make a fresh install look broken. A 404 from the listing
  endpoint (repo private or gone) is the same *state* with a different message,
  because "make the repo public" and "cut a release" are different fixes.
  `/releases/latest` alone is not enough to tell them apart — it also 404s when
  every release so far is a prerelease — so a 404 there falls through to the
  listing rather than being believed.
- **Presence must not stand in for correctness, again.** The archive is verified
  against `checksums.txt` (a *missing* entry is a refusal, not a warning), and
  then the staged binary is **executed and asked its version before any rename**.
  A file of the right name at the right path proves nothing: a truncated unpack,
  an archive for another architecture, and a proxy's HTML error page all produce
  one. Verification of every binary happens before the first one moves, so a host
  is never left with a new `tendrils` and an old `blossomd`.
- **Atomic replace, and a rollback.** Staging dir inside the destination (so the
  move is a same-filesystem rename), fsync, rename — the discipline `storeStream`
  established. On Windows the running image cannot be overwritten but can be
  renamed, so it goes to `<path>.old` and is swept up by a later run;
  `AsidePolicy` makes that path selectable so POSIX tests exercise it and the
  Windows CI leg exercises it for real, against a genuinely running process. If
  the move fails after the aside, the aside is undone before the error returns.
- **`blossomd` only where it already is**, and **before** `tendrils`.
  `docs/UPGRADING.md` upgrades the Blossom host first; an updater that quietly
  inverted that on a host running both would be doing the one thing the guide
  tells owners not to do. If blossomd fails, tendrils is untouched — both stale
  is consistent, mixed is not.
- **The daemon reports, never upgrades.** Banner line, daily re-check, a log
  line. Swapping a binary under a live daemon needs a restart to take effect,
  restart is service-manager-specific, and a sync daemon that restarts itself
  mid-pass is a new failure mode for no gain. `upgrade` prints the platform's
  restart command and stops.
- **Opt-out is checked before anything is scheduled.**
  `TENDRILS_NO_UPDATE_CHECK=1` or `"update_check": false`: no cache read, no
  spawn, no packet. An unreadable `config.json` disables the check — the question
  is whether this device may talk to the network on its own behalf, and with the
  owner's stated preference unreadable the honest answer is no.
- **The notice goes to stderr**, so `tendrils status | grep` sees exactly the
  status. It is suppressed for `version` (its output gets pasted into bug
  reports) and for `daemon` (which prints its own).

### The coordinated-upgrade gate

Every release publishes a **`release.json`** asset — `wire_format` plus
`min_compatible` — alongside `checksums.txt`. `internal/buildinfo.WireFormat` is
what this build speaks, and a test asserts the checked-in `release.json` agrees
with it, so a release that changes the wire format cannot ship without the
marker that makes `upgrade` stop and ask.

`upgrade` refuses to cross a declared boundary without an interactive
confirmation or `--force`. **`--yes` is deliberately not enough**: it means "do
not ask me the routine questions", and moving one device across a wire-format
boundary is not routine — it manufactures exactly the split-brain the boundary
warns about, and the losing side cannot tell. A release that declares nothing is
not a boundary (nothing before the marker existed changed the format); a marker
that exists but cannot be parsed **is** one, because the single file whose job is
to say when a change is dangerous is not a file to guess about.

**Bump `WireFormat` and `release.json` together** whenever old and new devices
would disagree invisibly: the event codec, the sealing format, or an arbitration
rule. Do not bump it for something old devices merely lack — a device that cannot
do something new still interoperates, and a device that quietly loses does not.

## Commands

```bash
go build ./...        # build all packages
go test ./...         # run all tests
go test ./path/to/pkg -run TestName   # run a single test
go vet ./...          # static checks
gofmt -l -w .         # format (gofmt is the project standard)
```

### Packaging with Fyne

Fyne uses **CGO**, so plain `GOOS=... go build` cross-compilation does not work. Use the `fyne` CLI for native builds and `fyne-cross` (Docker-based) for reproducible cross-platform builds.

```bash
go install fyne.io/tools/cmd/fyne@latest        # the fyne packaging CLI

fyne package -os windows                         # .exe (run on/for Windows)
fyne package -os linux                           # Linux tarball
fyne package -os android -appID ca.punkscience.tendrils   # .apk; needs Android SDK + NDK

go run .                                          # fast local dev iteration on desktop
```

- Desktop builds need a C compiler on PATH (MinGW-w64 on Windows, gcc/clang on Linux).
- Android builds need the Android SDK and NDK; set `ANDROID_HOME` / `ANDROID_NDK_HOME`.
- `-appID` must be the reverse-domain ID `ca.punkscience.tendrils` and stay stable across releases (changing it makes Android treat it as a different app).

## Architecture notes to preserve as the code grows

These are the load-bearing decisions implied by the project's premise. Document the concrete design here once it exists.

- **Nostr is the sync transport, not a file store.** Relays broadcast small events; large file contents need a separate plan (chunking, external blob storage, or NIP-based file transfer). Keep the event-publishing layer separate from the file-content transfer layer so either can change independently.
- **Three platforms, one core, one UI.** Fyne gives a shared UI layer across all three, but OS-specific concerns still differ — isolate filesystem watching, path conventions, background execution, and the Android app lifecycle behind interfaces so the sync engine stays portable. Android is the constraint that shapes the boundary: it has no persistent background daemon (use a foreground service / WorkManager equivalent), scoped/sandboxed storage, and doze-mode network limits. Keep the sync engine independent of Fyne so it can run headless and be tested without a UI.
- **Conflict resolution and identity.** File sync across devices needs a merge/conflict strategy and a device-identity/keypair model (Nostr keys are a natural fit). Decide these early — they are hard to retrofit.
