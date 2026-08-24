// Package relay is the concrete Nostr relay adapter behind engine.EventStore: it
// publishes signed file-entry events and fetches the owner's current set from one
// or more relays over websockets (go-nostr).
//
// Connections are held open and reused across a device's lifetime — a Sync pass
// publishes one event per changed file, so reconnecting each time would be
// wasteful — and lazily re-established if a relay drops. Writes go to every
// configured relay and succeed if any accepts; reads union the results and dedupe
// by event ID, so one unreachable relay neither loses a publish nor hides an
// event another relay still has.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/serverlist"
)

// Client talks to a fixed set of relay URLs. Safe for concurrent use.
type Client struct {
	urls []string

	// pageTimeout bounds one REQ, and pageAttempts/pageRetryDelay how many times
	// a page that failed to complete is re-asked before the relay is written off
	// for this fetch. They are fields rather than constants so tests can drive the
	// retry path without sleeping.
	pageTimeout    time.Duration
	pageAttempts   int
	pageRetryDelay time.Duration

	// log records why a fetch came back incomplete. Without it "complete=false"
	// reaches the operator as an unexplained boolean, which is the same class of
	// mistake as the silence this package was fixed to stop reporting.
	log *slog.Logger

	mu    sync.Mutex
	conns map[string]*nostr.Relay
}

// New returns a Client for the given relay URLs (ws:// or wss://).
func New(urls []string) *Client {
	return &Client{
		urls:           urls,
		pageTimeout:    defaultPageTimeout,
		pageAttempts:   defaultPageAttempts,
		pageRetryDelay: defaultPageRetryDelay,
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		conns:          make(map[string]*nostr.Relay),
	}
}

// SetLogger gives the client somewhere to report page retries and gaps it could
// not read past. Call it before the client is used; nil restores silence.
func (c *Client) SetLogger(log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c.log = log
}

// Publish sends evt to every configured relay, succeeding if at least one
// accepts it. It returns an error only when no relay took the event, so a single
// down relay never drops a change.
func (c *Client) Publish(ctx context.Context, evt *nostr.Event) error {
	if len(c.urls) == 0 {
		return errors.New("relay: no relays configured")
	}
	var errs []error
	accepted := false
	for _, url := range c.urls {
		r, err := c.relay(ctx, url)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		// go-nostr returns a nil error when the connection dies before the relay's
		// OK arrives, so a publish that never landed reports success. Ask the
		// connection whether it survived and treat a corpse as a failure: a
		// needless republish next pass costs one event, a believed-but-lost
		// publish costs an index row that says a file is on the relay when it is
		// not.
		connCtx := r.Context()
		if err := r.Publish(ctx, *evt); err != nil {
			c.drop(url)
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		if err := connCtx.Err(); err != nil {
			c.drop(url)
			errs = append(errs, fmt.Errorf("%s: connection lost before the relay acknowledged the event: %w", url, err))
			continue
		}
		accepted = true
	}
	if accepted {
		return nil
	}
	return fmt.Errorf("relay: publish rejected by all relays: %w", errors.Join(errs...))
}

// pageSize is how many events one paginated REQ asks for. Relays impose their
// own maximum (the reference relay behind this deployment caps at 400) and will
// return fewer, which pagination handles — asking for more than a relay allows
// costs nothing, asking for fewer only costs round trips.
const pageSize = 500

// maxPages bounds pagination so a misbehaving relay — one ignoring `until`, say —
// cannot spin forever. Hitting it means the result is incomplete, which Fetch
// reports rather than passing off a partial set as the whole truth.
const maxPages = 1000

const (
	// defaultPageTimeout bounds one REQ. It is deliberately generous: a tree of
	// thousands of files needs hundreds of pages, and the reference relay is a
	// Raspberry Pi that also serves blobs. go-nostr's own default is a silent
	// 7 seconds, which on a loaded relay turns into an empty page and, before this
	// was fixed, into a full-tree republish.
	defaultPageTimeout = 45 * time.Second
	// defaultPageAttempts is how many times one page is re-asked before the relay
	// is written off for this fetch. A fetch is hundreds of pages long; failing the
	// whole pass because one of them hiccuped would stall a large tree for good.
	defaultPageAttempts = 3
	// defaultPageRetryDelay spaces those attempts, and gives a relay that has just
	// dropped the connection a moment before the redial.
	defaultPageRetryDelay = 2 * time.Second
)

// Fetch returns every file-entry event the owner's key has published, unioned
// across reachable relays and deduped by event ID, plus whether that answer is
// the *whole* set.
//
// The completeness flag is the load-bearing part of this signature and callers
// must not ignore it. "This path has no event" and "I could not read the events"
// are different facts, and only the first justifies acting: the engine
// republishes a file it cannot see on the relay, so a truncated read that passes
// itself off as complete makes a device re-upload and re-announce its entire
// tree. That is not hypothetical — it is what produced 114,747 events for a
// 5,188-file tree on the reference fleet.
//
// It errors only when no relay could be reached at all. A relay that failed
// part-way through, or one that could not be walked to a proven end, still
// contributes what it returned, but the result is reported incomplete.
//
// Each relay is walked with NIP-01 pagination rather than a single query, because
// a relay caps how many events one REQ may return. Without paging, a tree larger
// than that cap yields a truncated view in which most paths look like they were
// never published.
func (c *Client) Fetch(ctx context.Context, pubkey string) (evts []*nostr.Event, complete bool, err error) {
	if len(c.urls) == 0 {
		return nil, false, errors.New("relay: no relays configured")
	}

	seen := make(map[string]struct{})
	var out []*nostr.Event
	var errs []error
	reached := false
	complete = true
	for _, url := range c.urls {
		whole, err := c.fetchAll(ctx, url, pubkey, seen, &out)
		if err != nil {
			c.drop(url)
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			complete = false
			continue
		}
		reached = true
		if !whole {
			complete = false
		}
	}
	if !reached {
		return nil, false, fmt.Errorf("relay: no relay reachable: %w", errors.Join(errs...))
	}
	return out, complete, nil
}

// fetchAll pages through one relay's copy of the owner's file-entry events,
// appending newly-seen ones to out, and reports whether it walked the whole
// history to a proven end.
//
// The cursor is `until`, which NIP-01 defines inclusively (created_at <= until),
// so each page re-reads the boundary timestamp; duplicates are absorbed by the
// seen set. When a page fails to move the cursor the cursor is forced strictly
// downward, because a page whose events all share one created_at would otherwise
// be re-requested forever.
//
// Forcing it steps over any events at that second the relay would not fit in one
// page, and that is a real hole: the reference relay has three of them, left by
// publish storms that put more than its 400-event cap into a single second. So
// the walk finishes — termination is not negotiable — but says it is incomplete
// rather than passing the skipped second off as empty.
//
// "Would not fit" has to be proven, not assumed, because the last page of every
// ordinary walk also fails to advance: it re-reads the boundary timestamp and
// finds only events already seen. The proof is capProven — a page length the
// relay has demonstrably cut short, which we learn when a later page turns up
// events the earlier one had room to include but did not. Until the relay has
// shown it truncates, a non-advancing page is just the end of history. Getting
// this backwards would report every fetch as incomplete, and an engine that never
// trusts a complete view can never repair a relay that genuinely lost an event.
func (c *Client) fetchAll(ctx context.Context, url, pubkey string, seen map[string]struct{}, out *[]*nostr.Event) (complete bool, err error) {
	var until *nostr.Timestamp
	complete = true
	capProven, prevLen := 0, 0
	for page := 1; ; page++ {
		if page > maxPages {
			return false, fmt.Errorf("pagination did not terminate after %d pages; result would be incomplete", maxPages)
		}
		filter := nostr.Filter{
			Kinds:   []int{nostrevent.KindFileEntry},
			Authors: []string{pubkey},
			Limit:   pageSize,
			Until:   until,
		}
		evts, err := c.queryPage(ctx, url, filter)
		if err != nil {
			return false, fmt.Errorf("page %d: %w", page, err)
		}
		if len(evts) == 0 {
			return complete, nil // walked past the oldest event, and the relay said so
		}

		oldest := evts[0].CreatedAt
		fresh := 0
		for _, e := range evts {
			if e.CreatedAt < oldest {
				oldest = e.CreatedAt
			}
			if _, dup := seen[e.ID]; dup {
				continue
			}
			seen[e.ID] = struct{}{}
			fresh++
			*out = append(*out, e)
		}

		// Events this page turned up that the previous one did not means the
		// previous page was cut off by the relay rather than by history: its
		// length is a cap this relay really enforces. (The seen set is shared
		// across relays, so a second relay holding the same events proves nothing
		// and is assumed complete — safe, because the relay that did supply them
		// is the one whose walk is being judged.)
		if fresh > 0 && prevLen > capProven {
			capProven = prevLen
		}
		prevLen = len(evts)

		next := oldest
		if until != nil && next >= *until {
			next = *until - 1 // force progress; the page did not move the cursor
			if capProven > 0 && len(evts) >= capProven {
				// A full page at one timestamp: there may be more at that second
				// than the relay will ever hand over, and we just stepped past them.
				complete = false
				c.log.Warn("relay holds more events at one timestamp than it will return; stepping past them leaves a gap",
					"relay", url, "timestamp", int64(*until), "page_events", len(evts), "page_cap", capProven)
			}
		}
		if next < 0 {
			return complete, nil // exhausted the timestamp range
		}
		until = &next
	}
}

// queryPage runs one REQ and returns its stored events, retrying a page that
// could not be read to its end.
//
// "To its end" means EOSE. This is the whole point of not using go-nostr's
// QuerySync: that helper returns (whatever arrived, nil) when its 7-second
// deadline expires, when the relay CLOSEs the subscription (auth-required, rate
// limited, filter refused), and when the websocket dies underneath it. All three
// look exactly like "this relay holds no more events", and the caller cannot tell
// the difference. A relay that has gone quiet must fail loudly, not answer
// "nothing".
func (c *Client) queryPage(ctx context.Context, url string, filter nostr.Filter) ([]*nostr.Event, error) {
	var errs []error
	for attempt := 1; attempt <= c.pageAttempts; attempt++ {
		if attempt > 1 {
			// The previous attempt may have failed because the connection is dead
			// while still claiming otherwise; drop it so this one redials.
			c.drop(url)
			select {
			case <-ctx.Done():
				return nil, errors.Join(append(errs, ctx.Err())...)
			case <-time.After(c.pageRetryDelay):
			}
		}
		r, err := c.relay(ctx, url)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		evts, err := queryPageOnce(ctx, r, filter, c.pageTimeout)
		if err == nil {
			return evts, nil
		}
		c.log.Warn("relay did not finish answering a query; retrying",
			"relay", url, "attempt", attempt, "of", c.pageAttempts, "err", err)
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("no complete answer after %d attempts: %w", c.pageAttempts, errors.Join(errs...))
}

// queryPageOnce fires one subscription and collects events until the relay
// signals end-of-stored-events. Anything else that ends the subscription — a
// CLOSED message, the connection dropping, the deadline — is an error, however
// many events had already arrived: a page is either the relay's whole answer to
// this filter or it is not an answer at all.
//
// go-nostr guarantees the ordering this relies on: dispatchEose waits for every
// pre-EOSE event to be handed to the (unbuffered) Events channel before EOSE is
// signalled, so nothing can still be in flight when this returns.
func queryPageOnce(ctx context.Context, r *nostr.Relay, filter nostr.Filter, timeout time.Duration) ([]*nostr.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sub, err := r.Subscribe(ctx, nostr.Filters{filter})
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Unsub()

	out := make([]*nostr.Event, 0, pageSize)
	for {
		select {
		case evt, ok := <-sub.Events:
			if !ok {
				return nil, fmt.Errorf("subscription ended after %d events without end-of-stored-events", len(out))
			}
			out = append(out, evt)
		case <-sub.EndOfStoredEvents:
			return out, nil
		case reason := <-sub.ClosedReason:
			return nil, fmt.Errorf("relay closed the subscription after %d events: %s", len(out), reason)
		case <-r.Context().Done():
			return nil, fmt.Errorf("connection lost after %d events, before end-of-stored-events", len(out))
		case <-ctx.Done():
			return nil, fmt.Errorf("no end-of-stored-events after %d events: %w", len(out), context.Cause(ctx))
		}
	}
}

// FetchServerList returns the Blossom servers the owner's key advertises via its
// kind-10063 event (BUD-03), so a device enrolled with only the key + a relay can
// discover where blobs live. The newest event across reachable relays wins. It
// returns (nil, nil) when the key has published no list — an empty result is not
// an error, only an unreachable relay is.
func (c *Client) FetchServerList(ctx context.Context, pubkey string) ([]string, error) {
	if len(c.urls) == 0 {
		return nil, errors.New("relay: no relays configured")
	}
	filter := nostr.Filter{
		Kinds:   []int{serverlist.Kind},
		Authors: []string{pubkey},
		Limit:   1,
	}

	var newest *nostr.Event
	var errs []error
	reached := false
	for _, url := range c.urls {
		// Same rule as Fetch: a relay that did not finish answering has not said
		// "no list". The caller publishes the union of what it discovered and what
		// it has configured, so a read failure passed off as an empty list would
		// quietly drop every server this device does not already know about.
		evts, err := c.queryPage(ctx, url, filter)
		if err != nil {
			c.drop(url)
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		reached = true
		for _, e := range evts {
			if newest == nil || e.CreatedAt > newest.CreatedAt {
				newest = e
			}
		}
	}
	if !reached {
		return nil, fmt.Errorf("relay: no relay reachable: %w", errors.Join(errs...))
	}
	if newest == nil {
		return nil, nil
	}
	return serverlist.Parse(newest)
}

// Close disconnects every open relay connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for url, r := range c.conns {
		if err := r.Close(); err != nil {
			errs = append(errs, err)
		}
		delete(c.conns, url)
	}
	return errors.Join(errs...)
}

// relay returns a live connection to url, reusing an open one or dialing a fresh
// connection (also replacing one that has since dropped).
func (c *Client) relay(ctx context.Context, url string) (*nostr.Relay, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.conns[url]; r != nil && r.IsConnected() {
		return r, nil
	}
	r, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		return nil, err
	}
	c.conns[url] = r
	return r, nil
}

// drop forgets a connection so the next call re-dials it.
func (c *Client) drop(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.conns[url]; r != nil {
		r.Close()
		delete(c.conns, url)
	}
}
