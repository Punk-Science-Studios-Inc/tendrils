package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/serverlist"
	"ca.punkscience.tendrils/internal/tree"
)

// testRelay is a minimal in-process NIP-01 relay: enough of REQ/EVENT/EOSE/OK to
// exercise the client, with parameterized-replaceable semantics (newest event
// per kind+pubkey+d tag) so it behaves like the real thing for our events.
type testRelay struct {
	mu     sync.Mutex
	events map[string]*nostr.Event // key: kind:pubkey:dtag
	// maxLimit caps how many events one REQ can return, mirroring the real
	// relay's UseEventstore(db, 400). A client that does not paginate silently
	// sees only this many, which is the bug this cap exists to catch.
	maxLimit int

	// The ways a relay stops answering without saying so. Each of these used to
	// reach the engine as "this key has published nothing", because go-nostr's
	// QuerySync reports its own timeout, a CLOSED, and a dead socket all as an
	// empty result with no error.
	silent      bool   // send the events but never EOSE
	closeReason string // answer a REQ with CLOSED instead of events
	dieAfterREQ int    // hang up after this many REQs on a connection (-1: on the first)
}

func newTestRelay(t *testing.T) (url string, r *testRelay) {
	r = &testRelay{events: map[string]*nostr.Event{}, maxLimit: 400}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		r.serve(conn)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), r
}

func (tr *testRelay) serve(conn *websocket.Conn) {
	ctx := context.Background()
	defer conn.Close(websocket.StatusNormalClosure, "")
	reqs := 0
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg []json.RawMessage
		if err := json.Unmarshal(data, &msg); err != nil || len(msg) == 0 {
			continue
		}
		var cmd string
		json.Unmarshal(msg[0], &cmd)
		switch cmd {
		case "EVENT":
			var evt nostr.Event
			if err := json.Unmarshal(msg[1], &evt); err != nil {
				continue
			}
			tr.save(&evt)
			writeJSON(ctx, conn, []any{"OK", evt.ID, true, ""})
		case "REQ":
			var subID string
			json.Unmarshal(msg[1], &subID)
			reqs++
			if tr.closeReason != "" {
				writeJSON(ctx, conn, []any{"CLOSED", subID, tr.closeReason})
				continue
			}
			if tr.dieAfterREQ != 0 && reqs > tr.dieAfterREQ {
				conn.Close(websocket.StatusAbnormalClosure, "")
				return
			}
			for i := 2; i < len(msg); i++ {
				var f nostr.Filter
				if err := json.Unmarshal(msg[i], &f); err != nil {
					continue
				}
				for _, e := range tr.match(f) {
					writeJSON(ctx, conn, []any{"EVENT", subID, e})
				}
			}
			if tr.silent {
				continue // events delivered, end-of-stored-events never sent
			}
			writeJSON(ctx, conn, []any{"EOSE", subID})
		case "CLOSE":
			// no-op: single-shot queries
		}
	}
}

func (tr *testRelay) save(evt *nostr.Event) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	key := replaceableKey(evt)
	if prev, ok := tr.events[key]; !ok || evt.CreatedAt >= prev.CreatedAt {
		tr.events[key] = evt
	}
}

// match applies the parts of NIP-01 the client's pagination depends on: kind and
// author filtering, the `until` cursor, newest-first ordering, and a limit capped
// by the relay's own maximum.
func (tr *testRelay) match(f nostr.Filter) []*nostr.Event {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []*nostr.Event
	for _, e := range tr.events {
		if len(f.Kinds) > 0 && !contains(f.Kinds, e.Kind) {
			continue
		}
		if len(f.Authors) > 0 && !contains(f.Authors, e.PubKey) {
			continue
		}
		if f.Until != nil && e.CreatedAt > *f.Until {
			continue
		}
		out = append(out, e)
	}
	// Newest first, as NIP-01 requires when a limit is applied. Ties broken by ID
	// so the order is stable across calls (Go map iteration is not).
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})

	limit := tr.maxLimit
	if f.Limit > 0 && f.Limit < limit {
		limit = f.Limit
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func replaceableKey(evt *nostr.Event) string {
	return string(rune(evt.Kind)) + ":" + evt.PubKey + ":" + evt.Tags.GetD()
}

func contains[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	conn.Write(ctx, websocket.MessageText, data)
}

func mustID(t *testing.T) *keys.Identity {
	t.Helper()
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func signEntry(t *testing.T, id *keys.Identity, e *tree.Entry) *nostr.Event {
	t.Helper()
	evt, err := nostrevent.Sign(e, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	return evt
}

// signEntryAt signs with an explicit created_at. Pagination walks the created_at
// axis, so those tests need to place events on it deliberately — and the stamp
// must be set before signing or go-nostr rejects the event as unsigned.
func signEntryAt(t *testing.T, id *keys.Identity, e *tree.Entry, createdAt int64) *nostr.Event {
	t.Helper()
	evt, err := nostrevent.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	evt.CreatedAt = nostr.Timestamp(createdAt)
	if err := evt.Sign(id.SecretHex()); err != nil {
		t.Fatal(err)
	}
	return evt
}

func TestPublishThenFetch(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	c := New([]string{url})
	defer c.Close()

	ctx := context.Background()
	evt := signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "h", BlobHash: "b", Size: 1, ModTime: time.Unix(1_700_000_000, 0)})
	if err := c.Publish(ctx, evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got, complete, err := c.Fetch(ctx, id.PublicHex())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !complete {
		t.Error("a relay that answered in full should report a complete set")
	}
	if len(got) != 1 {
		t.Fatalf("fetched %d events, want 1", len(got))
	}
	entry, err := nostrevent.Parse(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if entry.Path != "a.md" || entry.BlobHash != "b" {
		t.Errorf("round-trip entry wrong: %+v", entry)
	}
}

// The connection is reused across calls, and a later publish for the same path
// replaces the earlier one (parameterized-replaceable), so Fetch sees one entry.
func TestReplacePreservesLatest(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	c := New([]string{url})
	defer c.Close()
	ctx := context.Background()

	c.Publish(ctx, signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "old", ModTime: time.Unix(1_700_000_000, 0)}))
	c.Publish(ctx, signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "new", ModTime: time.Unix(1_700_000_500, 0)}))

	got, complete, err := c.Fetch(ctx, id.PublicHex())
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Error("a relay that answered in full should report a complete set")
	}
	if len(got) != 1 {
		t.Fatalf("fetched %d events, want 1 (replaceable)", len(got))
	}
	entry, _ := nostrevent.Parse(got[0])
	if entry.Sha256 != "new" {
		t.Errorf("kept %q, want the newest %q", entry.Sha256, "new")
	}
}

// A tree larger than the relay's per-REQ cap must still fetch in full. Without
// pagination the client sees only the newest maxLimit events, every other path
// looks unpublished, and the engine republishes the whole tree every pass.
func TestFetchPaginatesPastRelayLimit(t *testing.T) {
	url, r := newTestRelay(t)
	id := mustID(t)
	const total = 950 // comfortably more than two pages of the 400-event cap

	for i := 0; i < total; i++ {
		// Spread created_at so the `until` cursor has room to walk backwards.
		r.save(signEntryAt(t, id, &tree.Entry{
			Path:    fmt.Sprintf("f%04d.md", i),
			Sha256:  fmt.Sprintf("h%04d", i),
			ModTime: time.Unix(1_700_000_000, 0),
		}, 1_700_000_000+int64(i)))
	}

	c := New([]string{url})
	defer c.Close()
	got, complete, err := c.Fetch(context.Background(), id.PublicHex())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !complete {
		t.Error("a relay that answered in full should report a complete set")
	}
	if len(got) != total {
		t.Fatalf("fetched %d events, want all %d (relay caps one REQ at %d)", len(got), total, r.maxLimit)
	}
}

// A publish storm can put more events into one second than a relay will return
// in one page. Pagination must still terminate — the `until` cursor has nowhere
// to go, so it is forced downward — and it must report the result incomplete,
// because forcing it steps over every event at that second past the relay's cap.
//
// This is the shape the reference relay is actually in: a long spread history
// (which is what proves the relay caps a page at 400) with dense seconds buried
// in it. Reporting that walk as complete is what let the engine conclude those
// paths were never published and republish them, and republishing is what made
// the dense seconds in the first place.
func TestFetchReportsIncompleteOnIdenticalTimestamps(t *testing.T) {
	url, r := newTestRelay(t)
	id := mustID(t)
	const (
		spread = 900 // one per second: enough to prove the relay's per-page cap
		pile   = 600 // more than one page, all at a single instant below them
	)
	const base = 1_700_000_000

	for i := 0; i < spread; i++ {
		r.save(signEntryAt(t, id, &tree.Entry{
			Path:    fmt.Sprintf("spread%04d.md", i),
			Sha256:  fmt.Sprintf("h%04d", i),
			ModTime: time.Unix(base, 0),
		}, base+1+int64(i)))
	}
	for i := 0; i < pile; i++ {
		r.save(signEntryAt(t, id, &tree.Entry{
			Path:    fmt.Sprintf("pile%04d.md", i),
			Sha256:  fmt.Sprintf("p%04d", i),
			ModTime: time.Unix(base, 0),
		}, base))
	}

	c := New([]string{url})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, complete, err := c.Fetch(ctx, id.PublicHex())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("fetched nothing")
	}
	if len(got) >= spread+pile {
		t.Fatalf("fetched %d of %d; this test needs the relay's cap to bite", len(got), spread+pile)
	}
	if complete {
		t.Error("a walk that stepped over events at the boundary timestamp reported itself complete")
	}
}

// The failure this whole file exists for: a relay that goes quiet must not be
// read as a relay with nothing to say.
//
// go-nostr's QuerySync returns (whatever arrived, nil) when its deadline expires,
// when the relay CLOSEs the subscription, and when the websocket dies. All three
// arrive at the engine as "no events for this key", and the engine's response to
// no events is to publish the entire tree. On the reference fleet that produced a
// 5,048-path republish in a single 33-minute pass — every file re-announced, and
// on a relay that cannot replace long paths, every one of those events kept
// forever.
//
// Each subtest is one of those three, and every one must be an error rather than
// an empty answer.
func TestFetchFailsRatherThanReportingASilentRelayAsEmpty(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testRelay)
		want  string
	}{
		{
			name:  "events but no end-of-stored-events",
			setup: func(r *testRelay) { r.silent = true },
			want:  "end-of-stored-events",
		},
		{
			name:  "subscription closed by the relay",
			setup: func(r *testRelay) { r.closeReason = "auth-required: we only serve authenticated users" },
			want:  "closed the subscription",
		},
		{
			name:  "connection dropped without answering",
			setup: func(r *testRelay) { r.dieAfterREQ = -1 },
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, r := newTestRelay(t)
			id := mustID(t)
			for i := 0; i < 3; i++ {
				r.save(signEntryAt(t, id, &tree.Entry{
					Path:    fmt.Sprintf("f%d.md", i),
					Sha256:  fmt.Sprintf("h%d", i),
					ModTime: time.Unix(1_700_000_000, 0),
				}, 1_700_000_000+int64(i)))
			}
			tc.setup(r)

			c := New([]string{url})
			defer c.Close()
			c.pageTimeout = 300 * time.Millisecond
			c.pageRetryDelay = 0

			got, complete, err := c.Fetch(context.Background(), id.PublicHex())
			if err == nil {
				t.Fatalf("fetch returned %d events and complete=%v; a relay that never finished answering must be an error", len(got), complete)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A connection that drops part-way through a walk is retried, not written off.
// A fetch of a large tree is hundreds of pages long; failing the pass on the
// first hiccup would stall a big tree permanently, and the point of the stricter
// pagination is to stop guessing, not to stop syncing. Every event must still be
// there afterwards.
func TestFetchRetriesAPageAndStillReturnsEverything(t *testing.T) {
	url, r := newTestRelay(t)
	id := mustID(t)
	const total = 950 // three pages at the 400 cap
	for i := 0; i < total; i++ {
		r.save(signEntryAt(t, id, &tree.Entry{
			Path:    fmt.Sprintf("f%04d.md", i),
			Sha256:  fmt.Sprintf("h%04d", i),
			ModTime: time.Unix(1_700_000_000, 0),
		}, 1_700_000_000+int64(i)))
	}
	r.dieAfterREQ = 1 // every connection answers one page, then hangs up

	c := New([]string{url})
	defer c.Close()
	c.pageTimeout = 5 * time.Second
	c.pageRetryDelay = 0

	got, complete, err := c.Fetch(context.Background(), id.PublicHex())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != total {
		t.Fatalf("fetched %d events, want all %d despite the dropped connections", len(got), total)
	}
	if !complete {
		t.Error("a walk that recovered and reached the end should report a complete set")
	}
}

// Two relays holding the same event: Fetch unions and dedupes by event ID.
func TestFetchDedupesAcrossRelays(t *testing.T) {
	url1, r1 := newTestRelay(t)
	url2, r2 := newTestRelay(t)
	id := mustID(t)
	ctx := context.Background()

	evt := signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "h", ModTime: time.Unix(1_700_000_000, 0)})
	r1.save(evt)
	r2.save(evt) // same event on both relays

	c := New([]string{url1, url2})
	defer c.Close()
	got, complete, err := c.Fetch(ctx, id.PublicHex())
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Error("two relays that both answered in full should report a complete set")
	}
	if len(got) != 1 {
		t.Errorf("fetched %d events, want 1 after dedup", len(got))
	}
}

// A publish still succeeds when one of two relays is unreachable.
func TestPublishToleratesDownRelay(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	// Second URL points nowhere reachable.
	c := New([]string{url, "ws://127.0.0.1:1"})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	evt := signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "h", ModTime: time.Unix(1, 0)})
	if err := c.Publish(ctx, evt); err != nil {
		t.Errorf("publish should succeed via the reachable relay, got: %v", err)
	}
}

// A device publishes its Blossom server list; another device with only the key
// and the relay discovers where blobs live via FetchServerList.
func TestFetchServerListRoundTrip(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	c := New([]string{url})
	defer c.Close()
	ctx := context.Background()

	evt, err := serverlist.Sign([]string{"https://blossom.towerofsong.ca"}, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, evt); err != nil {
		t.Fatalf("publish server list: %v", err)
	}

	got, err := c.FetchServerList(ctx, id.PublicHex())
	if err != nil {
		t.Fatalf("fetch server list: %v", err)
	}
	if len(got) != 1 || got[0] != "https://blossom.towerofsong.ca" {
		t.Fatalf("discovered %v, want [https://blossom.towerofsong.ca]", got)
	}
}

// No published list is an empty result, not an error.
func TestFetchServerListEmpty(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	c := New([]string{url})
	defer c.Close()

	got, err := c.FetchServerList(context.Background(), id.PublicHex())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no servers, got %v", got)
	}
}

func TestPublishNoRelaysConfigured(t *testing.T) {
	if err := New(nil).Publish(context.Background(), &nostr.Event{}); err == nil {
		t.Error("expected error with no relays configured")
	}
}
