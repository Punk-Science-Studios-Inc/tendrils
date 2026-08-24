package buildinfo

// WireFormat is the protocol generation this build speaks: the event codec, the
// sealing format, and the arbitration rules that decide which version of a file
// wins. Every device in a fleet must agree on it.
//
// Bump it — and publish the bump in release.json (see .goreleaser.yaml) —
// whenever old and new devices would disagree *invisibly*. The precedent is the
// created_at change of 2026-07-23 (see "Two clocks" in AGENTS.md): a device on
// the old build had every event it published silently dropped by the relay, and
// nothing on that device could tell. Changes in that class include:
//
//   - the Nostr event codec (kind, tags, the meaning of a field),
//   - the sealing format or how a blob address is derived,
//   - any rule that decides which of two entries wins.
//
// Do not bump it for a change old devices merely lack. A device that cannot do
// something new still interoperates; a device that quietly loses does not.
//
// 1 — the format shipped since 2026-07-23: kind 31337 parameterized replaceable
// events with created_at = publication time, mtime arbitration, AES-256-GCM
// blobs sealed with a deterministic keyed nonce.
const WireFormat = 1
