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
| `internal/scan` | Walk the sync root → `Entry` map (sha256, mtime); skips `.tendrils-trash`; conflict-copy naming (`ConflictMarker`). |
| `internal/nostrevent` | `Entry` ⇄ Nostr event codec. Parameterized replaceable event, kind `31337`, `d`=path, `x`=sha256, `mtime`/`deleted` tags. **`created_at` = publication time, not mtime** — see "Two clocks" below. Signs/verifies. |
| `internal/config` | State dir (`$TENDRILS_HOME` or OS config dir), local `config.json` (overrides discovery), device key at rest (0600 file — deliberately not a keychain). |
| `internal/blob` | Blossom (BUD-01/02) client: `Upload`/`Download`/`Has` opaque bytes addressed by sha256, with a signed per-request kind-24242 auth event. Verifies content addresses locally on both directions. Pure transport — no knowledge of encryption or plaintext hashes. A 413 is typed as **`ErrTooLarge`** so the engine can tell "retrying cannot fix this" from a transient failure; error bodies are collapsed and truncated, and a proxy's HTML error page is dropped entirely (quoting one per rejected file is what made the daemon log unreadable). |
| `internal/tree` `internal/nostrevent` | `Entry` carries `BlobHash` (sealed-blob Blossom address) alongside `Sha256` (plaintext identity); the event codec round-trips it in a `blob` tag. |
| `internal/engine` | Orchestrates one full `Sync` reconcile pass: scan → read index base → fetch relay truth → `reconcile.Decide` per path → execute (seal+upload+publish / pull+unseal+atomic-write / trash / publish-tombstone). Network deps are `EventStore`/`BlobStore` interfaces (`*blob.Client` satisfies `BlobStore`), so it runs headless. Tested end-to-end with in-memory fakes: publish, pull, two-device convergence, delete propagation to trash, and conflict-copy preservation. One `Sync` is also the bootstrap and the periodic reconcile. **`publishLocal` hashes the bytes it actually read and uploaded**, never the scan's — see "Publish the hash you uploaded" below. Per-path **retry backoff** (`backoff.go`) holds repeatedly-failing paths back instead of retrying them every pass. |
| `internal/relay` | Concrete `engine.EventStore`: publishes/fetches file-entry events over go-nostr websockets to one or more relays. Persistent, lazily-reconnecting connections; publish to all (succeed if any accepts), fetch unions + dedupes by event ID. `Fetch` **paginates** with a NIP-01 `until` cursor (500/page, cursor forced strictly downward so a page of identical timestamps cannot loop) because relays cap one REQ — the reference relay at `relay.towerofsong.ca` is `UseEventstore(db, 400)`. Also `FetchServerList` for Blossom discovery (kind-10063). Tested against a minimal in-process NIP-01 relay (`coder/websocket`) that enforces the same 400 cap, so an unpaginated regression fails the suite. |
| `internal/serverlist` | Blossom server **discovery**: `Entry`-free codec for the BUD-03 kind-10063 "User Server List" (sign/parse a `[]string` of server URLs under the owner's key). `Shareable` strips loopback/unspecified hosts (never advertise `127.0.0.1` to the whole identity); `Merge` unions lists so devices republish instead of clobbering. The daemon publishes the union when it has a server and discovers from it when it doesn't — so a later device enrolls with just `--key`/`--relay`. |
| `internal/gc` | Orphan blob reclamation: folds the relay's current truth into a keep-set, classifies every stored blob (kept / unreferenced / invalid / too-recent / not-ours), and deletes only what it can justify. Dry-run by default. See "Blob GC" below — it is the one operation syncing cannot undo. |
| `internal/buildinfo` | Which build is running. Release builds stamp version/commit/date via `-ldflags -X`; everything else falls back to the toolchain's embedded module and VCS data, so a source build still reports a real commit and a dirty flag. Both binaries take the same stamp. |
| `cmd/tendrils` | **cobra** CLI: `keygen`, `enroll` (`--key`/`--root`, reuses stored key), `status` (pending/conflicts/**stuck** from scan-vs-index), `daemon` (builds the engine from config and runs a periodic reconcile loop; `--interval`, graceful shutdown on Ctrl-C), `gc` (reclaim orphaned blobs; `--apply` to delete), `version`. Root sets `cobra.Command.Version`, so `--version`/`-v` work too. |

Not yet built (next milestones): **relay** discovery (NIP-65/kind-10002 — until then `daemon` still requires `--relay` set at enroll; Blossom-server discovery via kind-10063 **is** built, so `--blossom` is optional on later devices), fsnotify watch (with debounce/settle, to complement the periodic reconcile), Blossom multi-server mirroring (daemon uses the first server — which is also why a file too large for that one server simply cannot sync, however many are configured), and the tray. (Blossom **orphan GC** is now built — see "Blob GC" below.)

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

## Memory is bounded by bytes, not by count

Every byte path in this project is `[]byte` — whole file in memory, no streaming — so peak memory tracks *file size × concurrency*, and neither term is small. The reference store holds 6,630 blobs averaging 32 MB with a 412 MB maximum, on a 1.8 GB Raspberry Pi that also runs the relay and the blob server. On 2026-07-27 a GC sweep drove that host to a load average of 71 and a 178% commit ratio; it swap-thrashed onto its SD card, saturated the SDIO bus, and took the SDIO-attached wifi down with it for half an hour. The host never rebooted and never browned out — from outside it looked exactly like a failing PSU, which is the diagnostic trap worth remembering.

Two ceilings now hold it:

- **`blossomd` streams uploads** (`storeStream`). The buffered version called `io.ReadAll` on the request body; `io.ReadAll` grows by reallocate-and-copy, so at the final growth both buffers are live and the peak is ~2× the blob — ~800 MB for the 412 MB blob, and the observed `MemoryPeak` was 796 MB. The cost of streaming is that the content address is unknown until the last byte, so a duplicate upload can no longer skip the write. That is one wasted temp file against a ceiling that does not move with the largest file anyone syncs.
- **GC counts bytes, not workers** (`gc.Options.MaxInFlightBytes`, default 256 MB). Worker count is a useless memory dial when blob sizes span two orders of magnitude: six slots is 390 MB or 4.8 GB purely by draw. A blob larger than the whole budget is admitted alone rather than refused, because refusing it would deadlock the sweep on exactly the blobs most worth reclaiming.

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
