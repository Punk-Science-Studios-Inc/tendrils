package blob

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"ca.punkscience.tendrils/internal/keys"
)

// Pool spreads blob operations across several Blossom servers, trying them in
// configured order until one answers. It satisfies the same interface as a single
// Client, so the engine is unaware there is more than one.
//
// This is failover, deliberately not mirroring. An upload goes to the first
// server that accepts it, not to all of them, so the pool buys reachability
// rather than redundancy. Two things follow, and both are intended:
//
//   - Order is preference. Put the closest or least restricted server first. A
//     LAN server ahead of a proxied public one means large files skip the proxy's
//     request-size cap, and a laptop that leaves the network still syncs — it just
//     falls through to the next server.
//   - A reader must try every server, because any one of them may be the only one
//     holding a given blob.
//
// Unreachable servers are remembered briefly. Without that, a laptop away from
// its LAN would pay the dial timeout on every single operation before falling
// through, which is slower than having no LAN server configured at all.
type Pool struct {
	clients []*Client

	mu       sync.Mutex
	downAt   map[string]time.Time
	downFor  time.Duration
	nowFunc  func() time.Time
	fallback bool // set once all servers have been marked down, to retry anyway
}

// downFor is how long a server that failed to answer is skipped. Short: a server
// coming back should be picked up quickly, and the cost of an occasional wasted
// dial is one timeout.
const defaultDownFor = 60 * time.Second

// NewPool returns a Pool over servers, in preference order.
func NewPool(servers []string, id *keys.Identity) *Pool {
	clients := make([]*Client, 0, len(servers))
	for _, s := range servers {
		clients = append(clients, New(s, id))
	}
	return &Pool{
		clients: clients,
		downAt:  map[string]time.Time{},
		downFor: defaultDownFor,
		nowFunc: time.Now,
	}
}

// NewPoolWithClients is NewPool over already-built clients, for tests.
func NewPoolWithClients(clients ...*Client) *Pool {
	return &Pool{
		clients: clients,
		downAt:  map[string]time.Time{},
		downFor: defaultDownFor,
		nowFunc: time.Now,
	}
}

// Servers lists the configured servers in preference order.
func (p *Pool) Servers() []string {
	out := make([]string, 0, len(p.clients))
	for _, c := range p.clients {
		out = append(out, c.server)
	}
	return out
}

func (p *Pool) now() time.Time {
	if p.nowFunc != nil {
		return p.nowFunc()
	}
	return time.Now()
}

// order returns the clients worth trying: healthy ones first in preference
// order, then any currently marked down. Marked-down servers are still tried
// last rather than skipped outright — refusing to try them at all would turn a
// transient blip into a hard outage when it is the only server that has the blob.
func (p *Pool) order() []*Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()

	healthy := make([]*Client, 0, len(p.clients))
	var down []*Client
	for _, c := range p.clients {
		if until, ok := p.downAt[c.server]; ok && now.Before(until) {
			down = append(down, c)
			continue
		}
		healthy = append(healthy, c)
	}
	return append(healthy, down...)
}

// markDown records that a server could not be reached. Only transport failures
// count: a 404 or a size rejection is a server answering, not a server that is
// gone, and treating those as unhealthy would push traffic away from a perfectly
// healthy store.
func (p *Pool) markDown(c *Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.downAt[c.server] = p.now().Add(p.downFor)
}

func (p *Pool) markUp(c *Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.downAt, c.server)
}

// Upload stores data on the first server that accepts it.
//
// Errors from every server are joined, so a caller asking errors.Is(err,
// ErrTooLarge) still gets the right answer when all of them refused the blob for
// its size — which is what lets the engine tell "no server will ever take this"
// from "nothing was reachable just now".
func (p *Pool) Upload(ctx context.Context, data []byte) (Descriptor, error) {
	clients := p.order()
	if len(clients) == 0 {
		return Descriptor{}, errors.New("blob: no Blossom servers configured")
	}
	var errs []error
	for _, c := range clients {
		desc, err := c.Upload(ctx, data)
		if err == nil {
			p.markUp(c)
			return desc, nil
		}
		if isUnreachable(err) {
			p.markDown(c)
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return Descriptor{}, errors.Join(errs...)
}

// Download fetches the blob from the first server that has it. A server that
// does not have it is not an error — under failover any one server may be the
// only one holding a given blob — so the search continues. ErrNotFound comes back
// only when every server was asked and none had it.
func (p *Pool) Download(ctx context.Context, sha256 string) ([]byte, error) {
	return p.DownloadSize(ctx, sha256, 0)
}

// DownloadSize is Download for a blob whose exact size is known.
func (p *Pool) DownloadSize(ctx context.Context, sha256 string, size int64) ([]byte, error) {
	clients := p.order()
	if len(clients) == 0 {
		return nil, errors.New("blob: no Blossom servers configured")
	}
	var errs []error
	sawNotFound := false
	for _, c := range clients {
		data, err := c.DownloadSize(ctx, sha256, size)
		if err == nil {
			p.markUp(c)
			return data, nil
		}
		if errors.Is(err, ErrNotFound) {
			p.markUp(c) // it answered; it simply does not have this blob
			sawNotFound = true
			continue
		}
		if isUnreachable(err) {
			p.markDown(c)
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) == 0 && sawNotFound {
		return nil, ErrNotFound
	}
	return nil, errors.Join(errs...)
}

// Has reports whether any server already holds this blob at this size. One
// server having it is enough to skip the upload: the point of the check is
// whether the bytes are retrievable, and Download searches every server too.
func (p *Pool) Has(ctx context.Context, sha256 string, size int64) (bool, error) {
	clients := p.order()
	var errs []error
	for _, c := range clients {
		ok, err := c.Has(ctx, sha256, size)
		if err != nil {
			if isUnreachable(err) {
				p.markDown(c)
			}
			errs = append(errs, err)
			continue
		}
		p.markUp(c)
		if ok {
			return true, nil
		}
	}
	if len(errs) > 0 {
		// Every server that answered said no, and at least one could not be asked.
		// Report the uncertainty: the caller re-uploads, which is the safe move.
		return false, errors.Join(errs...)
	}
	return false, nil
}

// isUnreachable distinguishes "could not talk to this server" from "this server
// gave an answer I did not like". Only the former should steer traffic away.
func isUnreachable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrTooLarge) {
		return false
	}
	var status *statusError
	return !errors.As(err, &status)
}

// statusError marks an error as an HTTP status the server actually returned,
// rather than a failure to reach it.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func newStatusError(code int, format string, args ...any) error {
	return &statusError{code: code, msg: fmt.Sprintf(format, args...)}
}
