package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/engine"
	"ca.punkscience.tendrils/internal/gc"
	"ca.punkscience.tendrils/internal/tree"
)

// relaySnapshot is written by tools/relay_snapshot.py from one LMDB read
// transaction. A complete trailer and matching counts make a truncated export
// unusable. GC still verifies every event's ID and signature before folding it.
type relaySnapshot struct {
	Format           string         `json:"format"`
	Pubkey           string         `json:"pubkey"`
	CapturedAt       int64          `json:"captured_at"`
	Events           []*nostr.Event `json:"events"`
	RawCount         int            `json:"raw_count"`
	SourceEventCount int            `json:"source_event_count"`
	EventCount       int            `json:"event_count"`
	Complete         bool           `json:"complete"`
}

const snapshotFormat = "tendrils-relay-snapshot-v2"
const maxSnapshotAge = 30 * time.Minute

func liveBlobsFromSnapshot(path, pubkey string, now time.Time) (map[string]struct{}, int, int, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, time.Time{}, fmt.Errorf("open relay snapshot: %w", err)
	}
	defer f.Close()

	var snap relaySnapshot
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&snap); err != nil {
		return nil, 0, 0, time.Time{}, fmt.Errorf("decode relay snapshot: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, 0, 0, time.Time{}, fmt.Errorf("relay snapshot has trailing data or is malformed: %v", err)
	}
	if snap.Format != snapshotFormat || !snap.Complete || snap.Pubkey != pubkey ||
		snap.EventCount != len(snap.Events) || snap.EventCount == 0 ||
		snap.SourceEventCount < snap.EventCount || snap.RawCount < snap.SourceEventCount {
		return nil, 0, 0, time.Time{}, fmt.Errorf("relay snapshot format, identity, or completion counts are invalid")
	}
	captured := time.Unix(snap.CapturedAt, 0)
	if captured.After(now.Add(time.Minute)) || now.Sub(captured) > maxSnapshotAge {
		return nil, 0, 0, time.Time{}, fmt.Errorf("relay snapshot is stale or has an invalid capture time: %s", captured.Format(time.RFC3339))
	}
	seen := make(map[string]struct{}, len(snap.Events))
	for i, evt := range snap.Events {
		if evt == nil || evt.Kind != 31337 || evt.PubKey != pubkey || !evt.CheckID() {
			return nil, 0, 0, time.Time{}, fmt.Errorf("relay snapshot event %d has an invalid kind, author, or ID", i)
		}
		if _, duplicate := seen[evt.ID]; duplicate {
			return nil, 0, 0, time.Time{}, fmt.Errorf("relay snapshot repeats event ID %s", evt.ID)
		}
		seen[evt.ID] = struct{}{}
	}
	current, skipped := engine.FoldRemote(snap.Events)
	if len(skipped) != 0 {
		return nil, 0, 0, time.Time{}, fmt.Errorf("relay snapshot contains %d invalid events; first: %w", len(skipped), skipped[0])
	}
	if len(current) != len(snap.Events) {
		return nil, 0, 0, time.Time{}, fmt.Errorf("relay snapshot repeats a file path")
	}
	entries := make([]*tree.Entry, 0, len(current))
	for _, e := range current {
		entries = append(entries, e)
	}
	return gc.LiveBlobs(entries), snap.SourceEventCount, len(current), captured, nil
}
