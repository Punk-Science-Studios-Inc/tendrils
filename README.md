# Tendrils

**Sync a folder across your devices over [Nostr](https://github.com/nostr-protocol/nostr) — end-to-end encrypted, no server account, no cloud vendor.**

Tendrils keeps a folder identical across your machines. It uses a Nostr relay to broadcast tiny "what changed" events and a [Blossom](https://github.com/hzrd149/blossom) server to move the file bytes — which are **encrypted on your device before they ever leave it**. One Nostr key is your whole identity: it enrolls every device and derives the key that encrypts your files. There is no account to create and no third party that can read your data.

- **End-to-end encrypted** — AES-256-GCM per blob; the relay and Blossom server only ever see ciphertext.
- **Byte-exact** — like Syncthing, not like git: it never rewrites your files (no line-ending translation).
- **Decentralized transport** — any Nostr relay + any Blossom server. Self-host both, or use public ones.
- **Cross-platform, pure Go, no CGO** — Windows, Linux, macOS. (An Android/Fyne UI is planned.)
- **Conflict-safe** — last-writer-wins by mtime, deletes go to a recoverable trash, conflicting edits are kept as copies.

> Status: pre-release. The sync engine and CLI work and are tested end-to-end across real machines. APIs and on-disk formats may still change.

## Install

Prebuilt binaries for Linux, macOS and Windows (amd64 and arm64). The installer
downloads the latest release, verifies it against the release's `checksums.txt`,
and installs `tendrils` plus the optional `blossomd` blob server. No toolchain
required — it falls back to building from source (Go 1.26+ and git) only when
there is no release for your platform.

**Linux / macOS**
```sh
curl -fsSL https://raw.githubusercontent.com/Punk-Science-Studios-Inc/tendrils/main/install.sh | sh
```

**Windows (PowerShell)**
```powershell
irm https://raw.githubusercontent.com/Punk-Science-Studios-Inc/tendrils/main/install.ps1 | iex
```

Set `TENDRILS_VERSION=v0.1.0` to pin a release, `TENDRILS_BIN_DIR` to change
where it lands. Check what you have with:

```sh
tendrils version     # full build identity — version, commit, build date, Go version
tendrils --version   # one line
```

Upgrading an existing fleet — including the one-time repair steps after this
release — is documented in **[docs/UPGRADING.md](docs/UPGRADING.md)**. Report the
output of `tendrils version` from each device when a fleet disagrees: a wire
format change has already once been silent on the losing device.

Or build it yourself:
```sh
git clone https://github.com/Punk-Science-Studios-Inc/tendrils && cd tendrils
go build -o tendrils ./cmd/tendrils
```

## Quickstart

You need two pieces of transport: a **Nostr relay** and a **Blossom server** (see [Infrastructure](#infrastructure)). Then:

```sh
# 1. Create your master key. BACK UP THE nsec — it is your whole identity.
tendrils keygen

# 2. Enroll this device: point it at a folder and your transport.
tendrils enroll --key nsec1... --root ~/Notes \
  --relay wss://your.relay --blossom http://your-blossom:8091

# 3. Start syncing (periodic reconcile; Ctrl-C to stop).
tendrils daemon --interval 1m
```

Repeat steps 2–3 on **every device using the same `nsec`**. Each device may put its folder wherever it likes — files sync by relative path, not absolute location. First sync on a fresh device is a clean pull; to make a device "catch up" authoritatively, enroll it against an **empty** folder and let it pull.

Other commands: `tendrils status` (pending changes / conflicts). Run `tendrils <cmd> --help` for flags.

## Infrastructure

Tendrils is transport-agnostic. You bring a relay and a Blossom server.

> **The one rule that makes multi-device sync work: every device must reach the *same* Blossom server.** The relay only carries the tiny "what changed" events (paths and hashes); the file *bytes* live on Blossom. If each device points at its own local Blossom, the metadata will sync and it will *look* like it's working, but a second device can never fetch the bytes — it's asking an empty server for hashes that live somewhere else. Pick one Blossom server all your devices can reach (a box on your LAN, a small VPS, a tunnelled home server, or a public one) and point every device at it. After the first device is running, later devices can **discover** that server automatically (see below) and be enrolled with just `--key` and `--relay`.

- **Nostr relay** — any relay your key can publish to. Public relays (e.g. `wss://nos.lol`) work, but note that event *paths* (not contents) are visible to the relay operator, so a private/self-hosted relay is recommended for privacy. Many relays enforce a write allowlist — add your key's pubkey there.
- **Blossom server** — stores the encrypted blobs. Use a public Blossom server, or self-host the minimal reference server included here (the installer already puts `blossomd` beside `tendrils`; to build it yourself:)

  ```sh
  go build -o blossomd ./cmd/blossomd
  # LAN/localhost, no auth — fine when only trusted machines can reach it:
  BLOSSOM_ADDR=0.0.0.0:8091 BLOSSOM_DIR=~/.tendrils-blobs ./blossomd
  ```

  By default `blossomd` **does not authenticate** — anyone who can reach it may upload or download. That is fine on localhost or a trusted LAN. **Before exposing it to the public internet** (e.g. behind a reverse proxy or tunnel so remote devices can reach it), turn on the pubkey allowlist so only your key can use it — otherwise the open server can be filled with junk:

  ```sh
  BLOSSOM_ADDR=0.0.0.0:8091 \
  BLOSSOM_ALLOWED_PUBKEYS=npub1yourkey... \
  ./blossomd
  ```

  With the allowlist set, the server verifies the signed authorization every Tendrils client already sends and rejects everyone else. Blobs are encrypted regardless, but auth is what keeps a public endpoint from being abused.

### Discovery (enrolling later devices with just the key)

The first device you run publishes its Blossom server list to the relay under your key (a [BUD-03](https://github.com/hzrd149/blossom/blob/master/buds/03.md) kind-10063 event). A later device enrolled with only `--key` and `--relay` (no `--blossom`) reads that list and learns where the blobs live — so you only have to name the server once. Loopback addresses (`127.0.0.1`, `localhost`) are never advertised, since no other device could reach them; give the first device the server's real LAN or public address.

## Ignoring files

Drop a **`.tendrilsignore`** file at the root of your synced folder. It uses **`.gitignore` syntax** and is itself synced, so one edit applies to every device.

```gitignore
# per-device editor state
.obsidian/workspace*.json
settings.local.json
.trash/
```

Ignored paths are *frozen*, not deleted: anything already synced stays in place but stops syncing.

## How it works

| Piece | Role |
|---|---|
| **Key** | One Nostr keypair (`nsec`) is the identity for every device; HKDF derives the AES-256 blob key from it. |
| **Events (relay)** | A small [parameterized replaceable event](https://github.com/nostr-protocol/nips) (kind `31337`) per file: path, plaintext hash, mtime, deleted flag, and the sealed blob's address. |
| **Blobs (Blossom)** | The encrypted file bytes, addressed by the SHA-256 of the *stored* (sealed) bytes. |
| **Reconcile** | Each pass compares local disk, the last-synced base (a local bbolt index), and the relay's truth, then acts: publish, pull, trash, or tombstone. Last-writer-wins by mtime; deletes are absolute and recoverable from `.tendrils-trash/`; conflicting edits are preserved as copies. |

Because encryption uses a random nonce per blob, the same file seals to different bytes on each device — so integrity is checked twice: the sealed bytes against the Blossom address, and the decrypted bytes against the plaintext hash.

## For AI agents / automation

Setting this up for a user, non-interactively:

1. **Install**: run the platform installer above (downloads a checksum-verified release; no toolchain needed). Binaries land in `~/.local/bin/tendrils` (Linux/macOS) or `%LOCALAPPDATA%\Programs\tendrils\tendrils.exe` (Windows). Override the target dir with `TENDRILS_BIN_DIR`.
2. **Transport**: ensure a relay and **one shared Blossom server that every device can reach** are up. A per-device *localhost* Blossom does **not** work for multi-device sync — the second device would fetch blobs from its own empty server. Run one `blossomd` on a host all devices can reach (bind `0.0.0.0`; if it's public, set `BLOSSOM_ALLOWED_PUBKEYS` to the user's pubkey) and point every device at that same URL.
3. **Enroll each device with the same key**: `tendrils enroll --key <nsec> --root <folder> --relay <wss/ws url> --blossom <http url>`. `--key` may be omitted after the first enroll (the key is stored `0600` in the state dir). `--blossom` may be omitted on **later** devices: once the first device has run its daemon, the Blossom server is discovered from the relay.
4. **Run continuously**: `tendrils daemon --interval 1m`. Register it as a service so it survives reboot — e.g. a `systemd --user` unit on Linux, or a Startup-folder launcher on Windows.
5. **State locations**: config/key/index live in `$TENDRILS_HOME` if set, else the OS config dir (`~/.config/tendrils`, `%AppData%\tendrils`). Set `TENDRILS_HOME` to isolate multiple instances.

Everything is flag- and env-driven; there are no interactive prompts.

## Security model

- The `nsec` is the master secret: whoever holds it can read all synced files and enroll new devices. **Back it up**; if lost with no backup, the encrypted blobs are unrecoverable.
- The relay sees event metadata including **file paths** (not contents). Self-host or use a private relay if paths are sensitive.
- `blossomd` does not authenticate by default — keep it local/LAN, or set `BLOSSOM_ALLOWED_PUBKEYS` to your key's pubkey before exposing it publicly. Blobs are always encrypted at rest regardless.

## Build & test

```sh
go build ./...        # build all
go test ./...         # run tests
go vet ./...          # static checks
gofmt -l -w .         # format (gofmt is the project standard)
```

CI runs build, vet, `gofmt -l`, `go mod tidy` cleanliness and `go test` on Linux
and Windows for every push and pull request, plus a GoReleaser dry run so a broken
release config fails before a tag exists rather than after. The race detector runs
in a separate Linux job — it needs cgo, and everything above is deliberately
buildable with no C compiler.

## Releasing

Releases are cut from an annotated tag; GoReleaser does the rest.

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

That builds `tendrils` and `blossomd` for linux/darwin/windows × amd64/arm64,
stamps the tag, commit and commit date into both binaries, writes `checksums.txt`,
and publishes a GitHub Release the installers download from. A tag with a
prerelease suffix (`v0.2.0-rc1`) publishes as a prerelease automatically.

Dry-run the whole thing without tagging or publishing:

```sh
goreleaser check
goreleaser release --snapshot --clean --skip=publish
```

## License

Not yet licensed. If you'd like to use or contribute, open an issue.
