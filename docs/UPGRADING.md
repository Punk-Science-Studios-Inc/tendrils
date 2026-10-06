# Upgrading a Tendrils fleet

How to roll this release out across every device and the Blossom host, and what
to run once afterwards to repair the damage the fixed bugs left behind.

Read the ordering section before starting. Nothing here is destructive except
`tendrils gc --apply`, which is called out separately.

---

## The short version

On a device that already has Tendrils installed:

```sh
tendrils upgrade                 # verify and replace both binaries
systemctl --user restart tendrils-daemon.service
```

`tendrils upgrade` downloads the release archive for this platform, verifies it
against the release's `checksums.txt`, **runs the new binary and asks it its
version**, and only then moves it into place. A failed upgrade leaves the
working binary exactly where it was. `blossomd` is replaced too, but only if it
already sits beside `tendrils`.

The rest of this document is the manual route (still the way to bootstrap a new
machine or recover a broken one), the ordering rules for a whole fleet, and the
one-time repair steps for damage earlier defects left behind. **`tendrils
upgrade` does not run any of those repair steps.**

> Nothing has been published yet. Until the first release exists,
> `tendrils upgrade` reports *"No releases have been published … yet"* and exits
> zero — it is a state, not a failure — and installing means the `install.sh`
> route below or a build from source.

---

## Update checks

A background check asks GitHub at most once a day whether a newer release
exists, caches the answer in `update.json` in the state directory, and prints a
short notice on **stderr** the next time you run a command:

```
$ tendrils status
Identity:  npub1…
Pending changes: 0
Conflicts:       0

A new version is available: 0.4.0 (you have 0.3.1)
Run 'tendrils upgrade' to install it.
```

The properties that matter:

- **It never delays a command.** The check runs in a detached child process and
  writes a cache file; the *next* invocation reads it. No command ever waits on
  the network to print its output.
- **Offline is silent.** A failed check is cached as a failure and retried on a
  growing delay (15 minutes doubling to a 6-hour ceiling). A laptop offline for
  a week neither hammers the API nor prints a network error from a command that
  has nothing to do with the network. The wait is bounded, so a device that
  comes back after a month notices the release that day.
- **It is off in one step.** `TENDRILS_NO_UPDATE_CHECK=1` in the environment, or
  `"update_check": false` in `config.json`. Both are consulted before anything
  is scheduled: no cache read, no process spawned, no packet.
- **The daemon reports; it never upgrades.** It prints the notice in its startup
  banner beside the version, re-checks daily while running, and logs what it
  finds. Replacing the binary under a live daemon takes effect on the next
  start, restart is service-manager-specific, and a sync daemon that restarts
  itself mid-pass is a new failure mode for no benefit.

Why a folder-sync tool ships an update checker at all: **a device on a stale
build cannot always tell that it is broken.** The `created_at` change of
2026-07-23 is the worked example — a pre-change device had every event it
published silently dropped by the relay, and nothing on that device could say
so. Auditing a fleet by hand across Linux, Windows and a Pi is how that gets
found today; a notice turns it into the stale device speaking up.

### Flags

| Flag | Does |
|---|---|
| `--check` | Report only. Exits **1** when an upgrade is available, 0 when current, 0 when nothing is published. For scripts and monitoring. |
| `--version vX.Y.Z` | Install that exact release, including an older one. A downgrade is confirmed first — it is a rollback, and it should be deliberate. |
| `--yes` | Assume yes for the routine confirmations. **Not** enough for a wire-format boundary. |
| `--force` | Cross a declared wire-format boundary, or reinstall the version already running (to repair a damaged binary). |

### What it verifies, and what it refuses

An unverified binary is worse than a stale one, so each of these is a refusal
rather than a warning:

- the release publishes no archive for this OS/architecture;
- the release publishes no `checksums.txt`;
- `checksums.txt` does not list this archive (**a missing entry is a refusal** —
  it is the shape a substitution takes);
- the downloaded bytes do not match the listed hash;
- the transfer ended short of the size the release declared;
- the downloaded binary does not run, or does not report the version it was
  published as.

Nothing is moved until every binary in the upgrade has passed. A host left with
a new `tendrils` and an old `blossomd` is worse than one left alone.

### How the replacement happens

Download into a staging directory **inside the destination**, fsync, then
rename — the same discipline as `blossomd`'s `storeStream` and the engine's
atomic writes. A rename either happens or it does not.

- **Linux / macOS** — `rename(2)` over a running binary is fine; the running
  process keeps its inode and is undisturbed.
- **Windows** — a running image cannot be overwritten or deleted, but it *can*
  be renamed. The installed binary is renamed to `tendrils.exe.old`, the new one
  is put in its place, and the `.old` file is swept up on a later run — the
  first one where the old process is no longer holding it. Seeing a `.old` file
  next to your binary after an upgrade is expected; it disappears on its own.

If the move fails after the aside, the aside is undone: the previous binary is
back before the error is printed.

### Restarting

`upgrade` prints the right line for the platform and stops there:

```sh
systemctl --user restart tendrils-daemon.service     # Linux (systemd user unit)
```
```powershell
schtasks /End /TN "Tendrils Daemon"                  # Windows scheduled task
schtasks /Run /TN "Tendrils Daemon"
```

If `blossomd` was replaced, restart the blob server too — and per the ordering
rules below, on a host running both, restart the blob server first.

### Coordinated upgrades

Some releases require **every device to move together**; upgrading one device
across that boundary manufactures exactly the split-brain the boundary warns
about, and the losing side is silent about it.

Each release publishes a `release.json` asset carrying a `wire_format` integer
and a `min_compatible` version. When a release declares a wire format this build
does not speak — or declares a `min_compatible` newer than this build —
`upgrade` says so and refuses to continue without an explicit confirmation, or
`--force` in a non-interactive run. `--yes` deliberately does not satisfy it:
`--yes` means "do not ask me the routine questions", and this is not routine.

A release that declares no wire format is not a boundary (nothing before the
marker existed changed it). A release whose marker exists but cannot be parsed
**is** treated as a boundary — the one file whose job is to say when a change is
dangerous is not a file to guess about.

---

## What changed, and why it matters operationally

Four defects in earlier builds could corrupt or strand data. All are fixed, but
**fixing the code does not undo what it already did** — that is what the one-time
steps at the end are for.

| Defect | Effect on a running fleet |
|---|---|
| `blossomd` wrote uploads non-atomically (`os.WriteFile` creates, *then* writes) | A failed write — a full disk is the easy way — left a **0-byte file at the blob's real address**. The server then reported that blob as present forever. |
| The engine trusted bare existence from `Has()` | Seeing the 0-byte blob "present", it skipped the upload and published an event pointing at nothing. Every puller failed; no device ever re-uploaded; the pass logged success. |
| `publishLocal` published the *scan's* hash, not the hash of the bytes it uploaded | A file rewritten between scan and upload was published under a hash that did not match its own blob — an event no device could ever satisfy. |
| Nothing ever reclaimed a blob | Every edit orphaned its predecessor. On the reference deployment this reached **926 GB of blobs for a 181 GB tree**, filling the disk and stopping all syncing. |

New commands: **`tendrils gc`** (reclaim orphans), **`tendrils repair`** (restore
missing blobs), **`tendrils retry`** (clear backoff after fixing a cause).

New behaviour: the daemon now uses **every** configured Blossom server in
preference order instead of only the first.

---

## Compatibility

**There is no protocol change.** Event format, kinds, and tags are identical, so
old and new devices interoperate and you can upgrade at your own pace.

Two caveats:

- **A device left on an old build keeps its bugs.** It can still publish a
  mismatched hash and still skip uploads on the strength of a corrupt blob. Every
  device it syncs with inherits the resulting holes. Upgrade them all.
- **`gc` and `repair` need the new `blossomd`**, because they enumerate the store
  through the new `GET /list/<pubkey>` endpoint. Upgrade the Blossom host first.

This is *not* like the `created_at` change of 2026-07-23, which genuinely had to
be coordinated. If you are also coming from a build older than that, read the
"Two clocks" section of `AGENTS.md` first — that one *does* require every device
to move together.

---

## Order of operations

1. **The Blossom host**, before anything else. It stops new 0-byte blobs being
   created and provides the listing endpoint the repair tools need.
2. **Every device**, in any order.
3. **The one-time repair and reclaim steps**, from a single device.

---

## 1. Upgrade the Blossom host

```sh
cd /path/to/tendrils && git pull
go build -o blossomd ./cmd/blossomd

# Keep a rollback.
cp ~/tendrils-blossom/blossomd ~/tendrils-blossom/blossomd.bak.$(date +%Y%m%d_%H%M%S)
install -m755 blossomd ~/tendrils-blossom/blossomd
systemctl --user restart tendrils-blossom.service   # or however you run it
journalctl --user -u tendrils-blossom.service -n 3 --no-pager
```

Cross-compiling for a Pi or other ARM64 host — pure Go, no CGO, so this works
from any machine:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o blossomd-arm64 ./cmd/blossomd
scp blossomd-arm64 user@host:/tmp/
ssh user@host 'install -m755 /tmp/blossomd-arm64 ~/tendrils-blossom/blossomd && systemctl --user restart tendrils-blossom.service'
```

Verify it came up against the **right directory**:

```sh
journalctl --user -u tendrils-blossom.service -n 2 --no-pager
# blossomd listening on 0.0.0.0:8091, dir=/media/external/blossom/blobs (auth on, 2 allowed key(s))
```

> **Check the `dir=` path is really your blob store.** `blossomd` creates its
> directory if absent, so if the store lives on a separate disk that failed to
> mount, it will happily start on an **empty directory on the root filesystem**
> and serve a store with nothing in it. Every device then treats its blobs as
> missing and re-uploads. Confirm the mount before restarting:
>
> ```sh
> findmnt /media/external && ls -1 /media/external/blossom/blobs | wc -l
> ```
>
> If that disk is in `/etc/fstab`, make sure the entry matches the disk that is
> actually attached (`lsblk -o NAME,LABEL,UUID,FSTYPE`) and carries `nofail`. A
> stale `UUID=` line silently leaves the store unmounted after every reboot.

---

## 2. Upgrade each device

**Linux / macOS**

```sh
tendrils upgrade                                     # on a device that already has it
systemctl --user restart tendrils-daemon.service
```

or, to bootstrap a machine that does not:

```sh
curl -fsSL https://raw.githubusercontent.com/Punk-Science-Studios-Inc/tendrils/main/install.sh | sh
systemctl --user restart tendrils-daemon.service
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/Punk-Science-Studios-Inc/tendrils/main/install.ps1 | iex
```

Then restart the daemon however it is registered — see the `install-windows`
skill in this repo if it runs as a scheduled task or startup launcher.

**From a working copy**

```sh
git pull && go build -o ~/.local/bin/tendrils ./cmd/tendrils
systemctl --user restart tendrils-daemon.service
```

Confirm the new build is live:

```sh
tendrils gc --help >/dev/null && echo "new build (gc present)"
tendrils status
```

---

## 3. Optional: put a LAN Blossom server first

The daemon now tries every configured server in order and falls through when one
is unreachable, so listing a local server ahead of a public one is safe even on a
laptop that leaves the network.

Worth doing when your public endpoint sits behind a proxy that caps request
bodies — Cloudflare's free tier refuses uploads over **100 MB**, which makes
large files permanently unsyncable through it. A direct LAN address has no such
cap.

`~/.config/tendrils/config.json`:

```json
{
  "blossom_servers": [
    "http://192.168.1.40:8091",
    "https://blossom.example.ca"
  ]
}
```

Use an **IP address, not an `.local` mDNS name** — mDNS resolution adds ~50 ms to
every connection that has to be re-established.

This is failover, not mirroring: an upload lands on the first server that accepts
it, so it buys reachability rather than redundancy. If your servers are
independent stores rather than two routes to the same one, be aware a blob may
exist on only one of them.

Restart the daemon after editing.

---

## 4. One-time repair, from a single device

Run these from **one** device that holds a complete copy of the tree. Order
matters.

### 4a. Clear stale backoff

Repeated failures are held back on a growing schedule — up to a day for failures
judged permanent. After an upgrade that fixes the cause, that wait is stale.

```sh
systemctl --user stop tendrils-daemon.service
tendrils retry --list      # inspect what is held back and why
tendrils retry             # clear it
systemctl --user start tendrils-daemon.service
```

### 4b. Restore missing blobs

Finds files whose published event points at a blob the store no longer has, and
rebuilds that blob from the local copy. Sealing is deterministic, so the exact
bytes the event names are reconstructed and uploaded to precisely that address —
no new event, no mtime change, no conflict risk.

```sh
tendrils repair                     # report only
tendrils repair --apply             # restore
```

Sync **cannot** detect this class of problem by itself: it compares file
contents, not blob availability, so an affected file looks perfectly converged on
every device while being unpullable by any device that does not already have it.
`pending` will read zero. Run `repair` explicitly after upgrading.

If some entries report "not on this device" or "local copy is a different
version", run `repair` on a device holding that version.

### 4c. Reclaim orphaned blobs

**This deletes data.** It reports only unless you pass `--apply`.

```sh
systemctl --user stop tendrils-daemon.service
tendrils gc                         # dry run — read this before applying
tendrils gc --apply
systemctl --user start tendrils-daemon.service
```

`gc` needs the relay (the keep-set comes from it) and the daemon stopped (it
holds the index lock). It releases the lock immediately after reading the base,
so the daemon can be restarted while a long sweep continues.

If the relay cannot return a complete history because more than one query's
limit of events share a timestamp, export the relay's LMDB store on its host and
use the read-only snapshot mode. The exporter holds one LMDB read transaction,
walks every raw event, and retains the current event for each path using the
same `created_at` and event-ID tie break as the sync engine:

```sh
python3 tools/relay_snapshot.py --db ~/.nostr-relay/db \
  --pubkey YOUR_PUBLIC_KEY --output /tmp/tendrils-relay-snapshot.json
# Copy the JSON to an enrolled device with enough RAM to inspect blobs.
tendrils gc --snapshot /tmp/tendrils-relay-snapshot.json --snapshot-check
tendrils gc --snapshot /tmp/tendrils-relay-snapshot.json --server http://YOUR_SERVER:8091
```

The snapshot must be less than 30 minutes old when the command starts. The
command checks its identity, counts, event IDs and signatures. Snapshot mode
is **report-only** and can run while the daemon holds the index. It never
accepts `--apply`; deletions require a complete live relay read. Keep the
default 48-hour grace so uploads made after the snapshot are spared. Leave
ownership checks enabled when the Blossom store serves more than one identity.

For a large store on a slow link, run the byte scan on the Blossom host instead
of pulling every candidate over the network. First validate the snapshot with
`--snapshot-check`, copy it to the server, and confirm the two `sha256sum`
values match. Then run the local streaming report with the keep-set count printed
by `--snapshot-check`:

```sh
python3 tools/blossom_local_report.py \
  --snapshot /tmp/tendrils-relay-snapshot.json \
  --expected-keep-count 14431 \
  --blob-dir /path/from/BLOSSOM_DIR \
  --key-file ~/.config/tendrils/key
```

This needs Python's `cryptography` package on the Blossom host. It verifies each
candidate's content address and AES-GCM tag in 1 MiB blocks, and reports any
read failure as an incomplete result. It never writes to the blob store. The
script trusts the snapshot only after the Go `--snapshot-check` and checksum
comparison; do not edit the snapshot between those steps.

Read the dry run before applying. Sanity check: **`referenced` bytes should be
close to the size of your synced tree.** If it is wildly smaller, stop — the
keep-set is wrong and applying would delete live data.

Useful flags:

| Flag | When |
|---|---|
| `--workers N` | Lower on a small host. Each concurrent check holds a whole blob *and* its plaintext in memory; 3 is comfortable on a 2 GB Pi. |
| `--server URL` | Sweep a specific server. Run it against a **local** address where possible — a sweep reads every candidate blob, and doing that over a slow link is the difference between hours and days. |
| `--grace 48h` | How recent a blob must be to be spared. Do not shorten it below your slowest device's sync interval. |
| `--trust-references` | **Rarely correct.** Skips per-blob ownership proof and deletes anything unreferenced. Only safe if the store holds no other identity's blobs. See below. |

> **`--trust-references` will destroy another application's data if your Blossom
> server is shared.** By default `gc` proves each candidate is yours by decrypting
> it under your key — an AES-GCM tag no other key can forge. On the reference
> deployment, 16% of sampled candidates belonged to a different application
> sharing the same server. Blobs carry no owner attribution, so nothing else would
> have caught it. Leave ownership proof on unless you are certain.

### 4d. Verify

```sh
tendrils repair          # expect: "All N published files have their blob."
tendrils status          # expect: pending 0, conflicts 0, no "Stuck" line
tendrils gc              # expect: unreferenced count near zero
```

---

## Where to run what

| | Blossom host | Every device | One device |
|---|:--:|:--:|:--:|
| Upgrade `blossomd` | ● | | |
| Upgrade `tendrils` | | ● | |
| `tendrils retry` | | | ● |
| `tendrils repair` | | | ● |
| `tendrils gc --apply` | | | ● |

---

## Troubleshooting

**"the daemon is running and holds the index"** — `gc`, `repair --apply`, and
`retry` read the index; stop the daemon first.

**`gc` aborts with "incomplete view of live blobs"** — the relay returned
materially fewer live paths than the device knows about. This is the guard doing
its job; deleting on a truncated keep-set would remove live blobs. Check the
relay is reachable and fully caught up, then retry.

**Uploads fail with 413** — a proxy is capping request bodies (100 MB on
Cloudflare's free tier). Add a direct server ahead of the proxied one (§3), or
route uploads past the proxy.

**Uploads fail with 524** — the proxy's origin timeout, typically a large file
over a slow link. Transient; backoff retries. If it is chronic, check whether the
Blossom host is on Wi-Fi — a 72 Mbit link yields roughly 5 MB/s and dominates
every other cost in the system.

**`status` shows "Stuck: N"** — N paths are waiting out a retry backoff. They are
still counted as pending. `tendrils retry --list` shows why.

**A file syncs everywhere but a new device cannot pull it** — its blob is
missing. Run `tendrils repair`.
