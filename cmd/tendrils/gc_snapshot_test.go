package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/tree"
)

func snapshotEvent(t *testing.T, id *keys.Identity, path, blob string, published time.Time) *nostr.Event {
	t.Helper()
	evt, err := nostrevent.Build(&tree.Entry{
		Path: path, Sha256: strings.Repeat("a", 64), BlobHash: blob,
		Size: 1, ModTime: published,
	})
	if err != nil {
		t.Fatal(err)
	}
	evt.CreatedAt = nostr.Timestamp(published.Unix())
	if err := evt.Sign(id.SecretHex()); err != nil {
		t.Fatal(err)
	}
	return evt
}

func writeSnapshot(t *testing.T, snap relaySnapshot) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSnapshotKeepsCurrentBlobAndReportsSourceCount(t *testing.T) {
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	newBlob := strings.Repeat("c", 64)
	snap := relaySnapshot{
		Format: snapshotFormat, Pubkey: id.PublicHex(), CapturedAt: now.Unix(),
		Events: []*nostr.Event{
			snapshotEvent(t, id, "a.md", newBlob, now),
		},
		RawCount: 3, SourceEventCount: 2, EventCount: 1, Complete: true,
	}
	live, events, paths, captured, err := liveBlobsFromSnapshot(writeSnapshot(t, snap), id.PublicHex(), now)
	if err != nil {
		t.Fatal(err)
	}
	if events != 2 || paths != 1 || !captured.Equal(now) {
		t.Fatalf("events=%d paths=%d captured=%s", events, paths, captured)
	}
	if _, ok := live[newBlob]; !ok || len(live) != 1 {
		t.Fatalf("current keep-set is %v", live)
	}
}

func TestSnapshotRejectsIncompleteStaleOrTamperedData(t *testing.T) {
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	evt := snapshotEvent(t, id, "a.md", strings.Repeat("b", 64), now)
	base := relaySnapshot{
		Format: snapshotFormat, Pubkey: id.PublicHex(), CapturedAt: now.Unix(),
		Events: []*nostr.Event{evt}, RawCount: 1, SourceEventCount: 1, EventCount: 1, Complete: true,
	}
	cases := []struct {
		name string
		edit func(*relaySnapshot)
	}{
		{"incomplete", func(s *relaySnapshot) { s.Complete = false }},
		{"count mismatch", func(s *relaySnapshot) { s.EventCount = 2 }},
		{"source count too small", func(s *relaySnapshot) { s.SourceEventCount = 0 }},
		{"raw count too small", func(s *relaySnapshot) { s.RawCount = 0 }},
		{"duplicate path", func(s *relaySnapshot) {
			s.Events = []*nostr.Event{evt, snapshotEvent(t, id, "a.md", strings.Repeat("c", 64), now.Add(time.Second))}
			s.RawCount, s.SourceEventCount, s.EventCount = 2, 2, 2
		}},
		{"stale", func(s *relaySnapshot) { s.CapturedAt -= 31 * 60 }},
		{"wrong identity", func(s *relaySnapshot) { s.Pubkey = strings.Repeat("0", 64) }},
		{"bad event ID", func(s *relaySnapshot) {
			copy := *s.Events[0]
			copy.ID = strings.Repeat("0", 64)
			s.Events = []*nostr.Event{&copy}
		}},
		{"bad signature", func(s *relaySnapshot) {
			copy := *s.Events[0]
			copy.Sig = strings.Repeat("0", 128)
			s.Events = []*nostr.Event{&copy}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.edit(&s)
			if _, _, _, _, err := liveBlobsFromSnapshot(writeSnapshot(t, s), id.PublicHex(), now); err == nil {
				t.Fatal("accepted an unsafe relay snapshot")
			}
		})
	}
}
