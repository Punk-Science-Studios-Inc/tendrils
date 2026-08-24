// Package nostrevent maps between a tree.Entry and the Nostr event that carries
// it across the relay. There is no monolithic manifest: each changed file is
// one small event, and the latest event per path is the current truth.
//
// Event model — parameterized replaceable event (NIP-01 addressable range):
//
//	kind:       KindFileEntry
//	d tag:      the sync-root-relative path (the replaceable identifier)
//	x tag:      SHA-256 of the plaintext content (NIP-94 convention); absent on a tombstone
//	blob tag:   SHA-256 of the sealed (encrypted) bytes — the Blossom fetch
//	            address, distinct from x because encryption randomizes the bytes;
//	            absent on a tombstone
//	size tag:   plaintext byte length; absent on a tombstone
//	mtime tag:  file modification time as unix seconds (drives last-writer-wins)
//	deleted tag: present iff this is a tombstone
//	created_at: the moment the event is published — NOT the file mtime.
//	content:    empty — an entry is metadata; the bytes live on Blossom.
//
// Why created_at is publication time, not mtime: a relay keeps the newest event
// per (pubkey, kind, d) by created_at, so created_at decides which *publish*
// survives, while the mtime tag decides which *version of the file* wins. Those
// are different questions and conflating them broke three cases:
//
//   - Re-creating a deleted file was unpublishable. A tombstone is stamped at
//     delete time (now), so a file restored afterwards — carrying its original,
//     older mtime — produced an event the relay silently discarded as stale. The
//     reconciler's "re-creation is honoured" rule could never reach other devices.
//   - Restoring any older version was likewise a no-op on the relay.
//   - A device whose event was dropped never learns of it, so it republishes the
//     same losing event on every pass, forever.
//
// With created_at = now, the latest publish always lands, and arbitration between
// versions happens where it belongs: in internal/reconcile, over the mtime tag.
// Parse has always preferred the mtime tag over created_at, so events written by
// earlier builds still decode identically — only the relay's tie-breaking changes.
package nostrevent

import (
	"fmt"
	"strconv"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/tree"
)

// KindFileEntry is Tendrils' parameterized replaceable event kind for one
// file's state. It sits in the addressable range (30000–39999) so relays retain
// only the latest event per (pubkey, kind, d=path).
const KindFileEntry = 31337

// MaxIndexedTagValue is the longest `d` value a relay can be relied on to index,
// and therefore the longest path whose events a relay will actually replace.
//
// NIP-01 sets no limit, but real relays do. The reference relay here (khatru over
// fiatjaf's eventstore) silently declines to index a tag value longer than 100
// bytes: a `#d` filter for such a path returns nothing even when the relay is
// holding hundreds of matching events, so the replaceable-event lookup that would
// retire the previous version finds nothing to retire and every publish is kept
// forever. On the reference fleet that is 1,103 of 5,188 paths, and 109,000 of
// the relay's 114,747 events.
//
// This is why the fold in engine.FoldRemote is not optional and why it applies
// NIP-01's created_at rule itself: for a long path the client has to do the
// arbitration the relay could not.
//
// Fixing it at the source means putting something bounded in the `d` tag — a hash
// of the path, with the path itself in a separate tag — and that is a wire-format
// change no device can make unilaterally: an old build reads the `d` tag as the
// path and would create files named after hashes. See AGENTS.md.
const MaxIndexedTagValue = 100

// PathTooLongForRelay reports whether a path exceeds what a relay will index as a
// d tag, so its events will pile up unreplaced rather than superseding each other.
func PathTooLongForRelay(path string) bool { return len(path) > MaxIndexedTagValue }

// Build constructs an unsigned event from an entry.
func Build(e *tree.Entry) (*nostr.Event, error) {
	if e == nil || e.Path == "" {
		return nil, fmt.Errorf("nostrevent: entry has no path")
	}
	tags := nostr.Tags{
		{"d", e.Path},
		{"mtime", strconv.FormatInt(e.ModTime.Unix(), 10)},
	}
	if e.Deleted {
		tags = append(tags, nostr.Tag{"deleted"})
	} else {
		tags = append(tags,
			nostr.Tag{"x", e.Sha256},
			nostr.Tag{"size", strconv.FormatInt(e.Size, 10)},
		)
		if e.BlobHash != "" {
			tags = append(tags, nostr.Tag{"blob", e.BlobHash})
		}
		for _, c := range e.Chunks {
			tags = append(tags, nostr.Tag{"chunk", c.BlobHash, strconv.FormatInt(c.Size, 10)})
		}
	}
	return &nostr.Event{
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Kind:      KindFileEntry,
		Tags:      tags,
		Content:   "",
	}, nil
}

// Sign builds and signs an event for entry e using the hex secret key.
func Sign(e *tree.Entry, secretHex string) (*nostr.Event, error) {
	evt, err := Build(e)
	if err != nil {
		return nil, err
	}
	if err := evt.Sign(secretHex); err != nil {
		return nil, fmt.Errorf("nostrevent: sign %s: %w", e.Path, err)
	}
	return evt, nil
}

// Parse converts a received event back into a tree.Entry. It verifies the
// signature and rejects events of the wrong kind or without a path.
func Parse(evt *nostr.Event) (*tree.Entry, error) {
	if evt.Kind != KindFileEntry {
		return nil, fmt.Errorf("nostrevent: wrong kind %d", evt.Kind)
	}
	ok, err := evt.CheckSignature()
	if err != nil {
		return nil, fmt.Errorf("nostrevent: check signature: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("nostrevent: invalid signature")
	}

	path := evt.Tags.GetD()
	if path == "" {
		return nil, fmt.Errorf("nostrevent: event has no d/path tag")
	}

	e := &tree.Entry{Path: path, ModTime: modTime(evt)}
	if evt.Tags.GetFirst([]string{"deleted"}) != nil {
		e.Deleted = true
		return e, nil
	}
	if x := evt.Tags.GetFirst([]string{"x"}); x != nil {
		e.Sha256 = x.Value()
	}
	if b := evt.Tags.GetFirst([]string{"blob"}); b != nil {
		e.BlobHash = b.Value()
	}
	if s := evt.Tags.GetFirst([]string{"size"}); s != nil {
		e.Size, _ = strconv.ParseInt(s.Value(), 10, 64)
	}
	for _, t := range evt.Tags {
		if len(t) < 3 || t[0] != "chunk" {
			continue
		}
		size, err := strconv.ParseInt(t[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("nostrevent: chunk %d of %q has unparseable size %q: %w", len(e.Chunks), path, t[2], err)
		}
		e.Chunks = append(e.Chunks, tree.Chunk{BlobHash: t[1], Size: size})
	}
	return e, nil
}

// modTime prefers the explicit mtime tag, falling back to created_at.
func modTime(evt *nostr.Event) time.Time {
	if m := evt.Tags.GetFirst([]string{"mtime"}); m != nil {
		if secs, err := strconv.ParseInt(m.Value(), 10, 64); err == nil {
			return time.Unix(secs, 0)
		}
	}
	return evt.CreatedAt.Time()
}
