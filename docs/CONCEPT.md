# Tendrils — Concept Brief

_Confirmed output of the brainstorm phase. This is the agreed concept and MVP
line; the executable specification lives in `features/*.feature`._

## Problem

Sam keeps an Obsidian vault in pCloud so it syncs across his devices. When an
agent edits a note through the pCloud network drive, the sync process silently
clobbers the agent's writes and the work is lost. He wants to edit files
**locally** on every machine and have his own tool deliberately propagate
changes — a sync he can see, understand, control, and repair.

## Personas

- **The Owner-Operator (primary).** Technical, runs AI agents against a personal
  Obsidian vault, works across Windows and Linux (Android later), hates
  per-machine passwords, and values owning and understanding the tool over
  convenience.

## One-sentence pitch

Tendrils keeps your personal folders identical across all your devices by
editing locally and syncing deliberately over Nostr + Blossom — one key enrolls
a device, and nothing is ever silently overwritten.

## Why build it instead of adopting Syncthing

The honest driver is **trust through ownership**: when sync breaks, Sam wants to
open the hood, understand exactly what it did, and fix it because it is his.
Three concrete differentiators reinforce that:

- **One-key enrollment.** A Nostr keypair is the login — paste the key and the
  device is in. No per-device pairing dance.
- **Relay-mediated, not direct.** Devices push to a relay / Blossom server and
  pull later; they never need to be online at the same time.
- **Legible by construction.** A headless engine plus a CLI and logs is the most
  understandable interface possible — which is the whole point.

"Simple enough to fully understand and repair" is treated as a design
constraint, not a nicety.

## Architecture spine (borrowed, not invented)

- **File bytes → Blossom**, encrypted, addressed by their sha256 hash.
- **"What the tree looks like now" → small Nostr events**, one per changed file
  (`path`, `blob-sha256`, `mtime`, `deleted?`), à la NIP-94. Latest event per
  path is the current truth. **No monolithic manifest** — that scaling wall is
  what abandoned `blossom-drive`.
- **Reconciliation → Negentropy / NIP-77** to find the diff between a device and
  the relay efficiently.
- **One key does identity, upload authorization, and encryption** (the symmetric
  key is derived from the Nostr key).

The sync loop in one sentence: **watch → settle → encrypt → upload blob →
publish tiny event; and independently, periodically reconcile against the relay
and pull what's missing.** Bootstrap is just reconcile-from-empty.

## Confirmed design decisions

| # | Decision |
|---|----------|
| Conflict model | **Last-writer-wins by plain file mtime.** The loser is always preserved as a conflict copy — a wrong guess costs a rename, never data. |
| Deletes | **Absolute — a delete always wins**, even over a newer concurrent edit. Deleted files move to `.tendrils-trash/`, pruned after **7 days**. A deliberately-later re-creation of a deleted filename is honoured. |
| Sync trigger | **Both** a live `fsnotify` watch (with debounce/settle) **and** a periodic reconcile (on a timer, on startup, on network-regain). |
| Tree representation | **One small Nostr event per changed file.** No giant manifest. |
| New-device bootstrap | **Reconcile-from-empty** — the same code path as a device that was offline for a week. |
| Non-empty folder on enroll | **Union-merge** — local files are published, remote files are pulled, same-path differences resolve by newest + conflict copy. This is the day-one migration path. |
| Config discovery | **Published on Nostr under the owner's key** (relay list NIP-65, Blossom server list kind 10063), with a **local config-file override**. |
| Retry on outage | **Every minute for 10 minutes, then hourly; after 12 hours report critical and stop.** Pending changes stay queued locally throughout. |
| Encryption | **Encrypt every blob at rest**, always. Symmetric key **derived from the Nostr key**. |
| Identity | A **Nostr keypair** is the login; pasting the key enrolls a device. No passwords, no accounts. |

## MVP line

**In v1:**

- One synced folder tree, local-first, auto-propagating on change.
- Last-writer-wins by mtime + recoverable conflict copies and trash.
- One-key device enrollment with native folder selection and union-merge.
- Headless Go **sync daemon + CLI**, plus a **minimal tray status indicator**.
- Runs on **Windows and Linux** desktops.

**Deferred (explicitly out of v1):**

- **File sharing** (single-file download links via Blossom) — a natural v2 add.
- **Android app** — where one-key enrollment shines but the hard problems live
  (no background daemon, scoped storage, doze). Prove the engine headless first.
- **Fyne / rich GUI** — no UI-toolkit commitment in v1; the tray uses a
  standalone systray library, not the full framework.
- Multiple / independent synced folders.
- Line-level merge (mtime last-writer-wins is enough).
- Mirror / multi-server redundancy.
- Selective / partial sync **is built** (`tendrils exclude` — per-node
  opt-outs; see README and AGENTS.md). Still deferred: ignore rules beyond the
  `.tendrilsignore` subset.

## What's ready to build

The three v1 capabilities are specified as declarative Gherkin:

- `features/local-first-sync.feature`
- `features/device-enrollment.feature`
- `features/status-observability.feature`

Open decisions that were deliberately deferred (not silently assumed) are in
`docs/OPEN-QUESTIONS.md`.
