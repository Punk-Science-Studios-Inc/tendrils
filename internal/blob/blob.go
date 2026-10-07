// Package blob is Tendrils' client for a Blossom server (BUD-01/02): the place
// file bytes live, addressed by the SHA-256 of the stored bytes. It is the
// "file-content transfer layer", kept deliberately separate from the tiny Nostr
// events that describe the tree — either can change without touching the other.
//
// This layer knows nothing about encryption or plaintext hashes. It moves opaque
// bytes and their content address. The engine seals a file with internal/crypt,
// hands the ciphertext here, and records the returned address in the event it
// publishes; a pulling device fetches those bytes by that address and unseals
// them. Because the content address is the hash of the *stored* (sealed) bytes,
// it is what the engine must publish — not the plaintext hash used for file
// identity and dedup.
//
// Every request carries a short-lived BUD-01 authorization: a signed kind-24242
// Nostr event, base64-encoded in the Authorization header, proving the owner's
// key authorized this upload/get. The same key that encrypts is the key that
// authorizes, so no separate credential exists to manage.
package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/keys"
)

// authKind is the BUD-01 authorization event kind.
const authKind = 24242

// authTTL is how long a signed authorization stays valid. Short by design: the
// header is minted per request, so a leaked one expires quickly.
const authTTL = 60 * time.Second

// ErrNotFound is returned when a Blossom server has no blob for a hash. It lets
// the engine tell "the server doesn't have it (yet)" apart from a transport
// failure it should retry.
var ErrNotFound = errors.New("blob: not found")

// ErrTooLarge wraps a rejection for the blob's size (HTTP 413). It is almost
// always a reverse proxy in front of the server, not the server itself — a
// Cloudflare-fronted host caps request bodies at 100 MB on the free plan. The
// engine needs to tell it apart from a transient failure because retrying it on
// the next pass cannot possibly succeed: the same bytes will be refused again.
var ErrTooLarge = errors.New("blob: too large for server")

// ErrStalled is returned when a response body stops arriving for StallTimeout.
// Transient: the next attempt, or the next server, may well get through.
var ErrStalled = errors.New("blob: transfer stalled")

// ErrWrongSize is returned when a blob is not the size it must be, or exceeds
// the limit for a blob of unknown size. The download is discarded unread.
var ErrWrongSize = errors.New("blob: wrong size")

// Defaults for a Client's download bounds.
const (
	DefaultStallTimeout = 60 * time.Second
	DefaultMaxBytes     = 2 << 30
)

// maxErrBody caps how much of a server's error body is quoted back in an error.
const maxErrBody = 200

// maxIdleConns is how many connections the client keeps warm per Blossom server.
// Sized for the most concurrent callers any command uses (the blob sweep), so
// connection reuse is the rule rather than the exception.
const maxIdleConns = 16

// detail renders a server error body as a short suffix for an error message, or
// "" when there is nothing worth quoting. Proxies answer failures with a full
// HTML error page whose only real information is the status line we already
// report, so those are dropped — quoting one whole turned every rejected upload
// into kilobytes of log.
func detail(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if s == "" || strings.Contains(strings.ToLower(s), "<html") {
		return ""
	}
	if len(s) > maxErrBody {
		s = s[:maxErrBody] + "…"
	}
	return ": " + s
}

// Descriptor is a Blossom server's record of a stored blob (BUD-02).
type Descriptor struct {
	// URL is the server-provided direct URL to the blob.
	URL string `json:"url"`
	// SHA256 is the content address: the lowercase-hex SHA-256 of the stored bytes.
	SHA256 string `json:"sha256"`
	// Size is the stored byte length.
	Size int64 `json:"size"`
}

// Client talks to a single Blossom server as one enrolled identity. Multi-server
// mirroring is out of v1 scope; the engine composes one Client per configured
// server if it needs several.
type Client struct {
	server string // base URL, no trailing slash
	id     *keys.Identity
	http   *http.Client

	// StallTimeout is how long a response body may deliver nothing before the
	// transfer is abandoned. Zero means DefaultStallTimeout.
	StallTimeout time.Duration
	// MaxBytes bounds a download whose size is not known in advance. Zero means
	// DefaultMaxBytes.
	MaxBytes int64
}

func (c *Client) stallTimeout() time.Duration {
	if c.StallTimeout > 0 {
		return c.StallTimeout
	}
	return DefaultStallTimeout
}

func (c *Client) maxBytes() int64 {
	if c.MaxBytes > 0 {
		return c.MaxBytes
	}
	return DefaultMaxBytes
}

// New returns a Client for server, authorizing as id. Its HTTP client uses
// granular transport timeouts rather than a single overall deadline: a blanket
// http.Client.Timeout caps the *whole* request including the body transfer, so a
// large blob (a multi-megabyte file) fails the moment it exceeds the cap no
// matter how healthily it is streaming. Instead we bound only the stall points —
// connect, TLS handshake, and the wait for response headers — and let the body
// stream for as long as it makes progress. The per-request context carries the
// hard cap when a caller wants one.
func New(server string, id *keys.Identity) *Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          maxIdleConns,
		// Go's default is 2 per host, which quietly throttles any caller that runs
		// more than two requests at once: the extra connections are closed after
		// each response and every following request pays a fresh dial. Against a
		// small home server that measured ~250 ms per request (an mDNS lookup plus
		// a slow accept), which dominated everything else for small blobs. Keeping
		// one idle connection per concurrent caller makes reuse the normal case.
		MaxIdleConnsPerHost: maxIdleConns,
	}
	return NewWithHTTP(server, id, &http.Client{Transport: transport})
}

// NewWithHTTP is New with a caller-supplied *http.Client (for tests, custom
// timeouts, or proxies).
func NewWithHTTP(server string, id *keys.Identity, hc *http.Client) *Client {
	return &Client{server: strings.TrimRight(server, "/"), id: id, http: hc}
}

// Upload stores data and returns its descriptor. The content address is computed
// locally and cross-checked against the server's, so a mangled upload is caught
// here rather than surfacing as a corrupt download later.
func (c *Client) Upload(ctx context.Context, data []byte) (Descriptor, error) {
	sum := hashHex(data)

	auth, err := c.authHeader("upload", sum)
	if err != nil {
		return Descriptor{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.server+"/upload", bytes.NewReader(data))
	if err != nil {
		return Descriptor{}, fmt.Errorf("blob: build upload request: %w", err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return Descriptor{}, fmt.Errorf("blob: upload to %s: %w", c.server, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		return Descriptor{}, fmt.Errorf("blob: %s refused %d bytes as too large (%s)%s: %w",
			c.server, len(data), resp.Status, detail(body), ErrTooLarge)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Descriptor{}, newStatusError(resp.StatusCode, "blob: upload rejected (%s)%s", resp.Status, detail(body))
	}

	var d Descriptor
	if err := json.Unmarshal(body, &d); err != nil {
		return Descriptor{}, fmt.Errorf("blob: parse upload response: %w", err)
	}
	if d.SHA256 != sum {
		return Descriptor{}, fmt.Errorf("blob: server stored %s but we sent %s", d.SHA256, sum)
	}
	return d, nil
}

// Download fetches the blob at sha256 when its size is not known in advance. It
// is bounded by MaxBytes rather than trusting the server to say how large it is.
func (c *Client) Download(ctx context.Context, sha256 string) ([]byte, error) {
	return c.DownloadSize(ctx, sha256, 0)
}

// DownloadSize fetches the blob at sha256, which must be exactly size bytes
// (unknown if size <= 0, in which case MaxBytes bounds it), and verifies the
// bytes hash back to the address, so a wrong, short, long or corrupted response
// never reaches the caller. Returns ErrNotFound when the server has no such blob,
// and ErrStalled when the body stops arriving for StallTimeout.
func (c *Client) DownloadSize(ctx context.Context, sha256 string, size int64) ([]byte, error) {
	auth, err := c.authHeader("get", sha256)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server+"/"+sha256, nil)
	if err != nil {
		return nil, fmt.Errorf("blob: build download request: %w", err)
	}
	req.Header.Set("Authorization", auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: download from %s: %w", c.server, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, newStatusError(resp.StatusCode, "blob: download failed (%s)%s", resp.Status, detail(msg))
	}

	limit := size
	if limit <= 0 {
		limit = c.maxBytes()
	}
	if resp.ContentLength > limit || size > 0 && resp.ContentLength >= 0 && resp.ContentLength != size {
		return nil, fmt.Errorf("blob: %s… is %d bytes, want %s: %w", sha256[:min(12, len(sha256))], resp.ContentLength, sizeWant(size, limit), ErrWrongSize)
	}

	body := &progressReader{r: resp.Body, timer: time.AfterFunc(c.stallTimeout(), func() { cancel(ErrStalled) }), d: c.stallTimeout()}
	defer body.timer.Stop()
	buf := make([]byte, 0, min(limit, 1<<20))
	if size > 0 {
		buf = make([]byte, 0, size)
	}
	w := bytes.NewBuffer(buf)
	n, err := io.Copy(w, io.LimitReader(body, limit+1))
	if err != nil {
		if errors.Is(context.Cause(ctx), ErrStalled) {
			return nil, fmt.Errorf("blob: download from %s: no data for %s: %w", c.server, c.stallTimeout(), ErrStalled)
		}
		return nil, fmt.Errorf("blob: read blob body: %w", err)
	}
	if n > limit || size > 0 && n != size {
		return nil, fmt.Errorf("blob: %s… delivered %d bytes, want %s: %w", sha256[:min(12, len(sha256))], n, sizeWant(size, limit), ErrWrongSize)
	}
	data := w.Bytes()
	if got := hashHex(data); got != sha256 {
		return nil, fmt.Errorf("blob: integrity check failed: got %s want %s", got, sha256)
	}
	return data, nil
}

func sizeWant(size, limit int64) string {
	if size > 0 {
		return strconv.FormatInt(size, 10)
	}
	return "at most " + strconv.FormatInt(limit, 10)
}

// progressReader pushes a deadline back every time bytes arrive, so a transfer
// is cut off only when it stops moving, never for being large or slow.
type progressReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.timer.Reset(p.d)
	}
	return n, err
}

// Has reports whether the server holds the blob at sha256 *and* that it is
// wantSize bytes long, via a HEAD request.
//
// The size is not decoration. Callers use Has to skip an upload the server does
// not need, so a false positive does not cost bandwidth — it silently abandons
// the file: the blob is never repaired, every later pass skips it again, and the
// publishing device reports success. A server that leaves a truncated blob at a
// real content address (an earlier blossomd did exactly this when its disk filled)
// turns one transient error into permanent, invisible data loss.
//
// So this is deliberately conservative. A missing or unparseable Content-Length
// counts as "not present", and the caller re-uploads. Re-sending bytes the server
// already has is harmless — it is the same content address either way — and is
// the cheaper mistake by a wide margin.
func (c *Client) Has(ctx context.Context, sha256 string, wantSize int64) (bool, error) {
	auth, err := c.authHeader("get", sha256)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.server+"/"+sha256, nil)
	if err != nil {
		return false, fmt.Errorf("blob: build head request: %w", err)
	}
	req.Header.Set("Authorization", auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("blob: head to %s: %w", c.server, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if resp.ContentLength != wantSize {
			// Present but the wrong length: the address cannot describe these bytes.
			// Report absent so the caller overwrites it rather than trusting it.
			return false, nil
		}
		return true, nil
	default:
		return false, newStatusError(resp.StatusCode, "blob: head failed (%s)", resp.Status)
	}
}

// Stored is one blob the server holds, as reported by List.
type Stored struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Uploaded is when the *server* stored it, not a client-supplied time. A
	// sweeper needs it to leave recently-written blobs alone, since an upload
	// whose describing event has not yet propagated is not an orphan.
	Uploaded int64 `json:"uploaded"`
}

// List enumerates every blob the server holds. It is what makes orphan
// collection possible: nothing can decide a blob is unreferenced without first
// knowing the blob exists.
func (c *Client) List(ctx context.Context, pubkey string) ([]Stored, error) {
	auth, err := c.authHeader("list", "")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server+"/list/"+pubkey, nil)
	if err != nil {
		return nil, fmt.Errorf("blob: build list request: %w", err)
	}
	req.Header.Set("Authorization", auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: list from %s: %w", c.server, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, newStatusError(resp.StatusCode, "blob: list failed (%s)%s", resp.Status, detail(msg))
	}
	var out []Stored
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("blob: parse list response: %w", err)
	}
	return out, nil
}

// Delete removes the blob at sha256. It is idempotent: deleting a blob the server
// does not have succeeds, so a retried sweep does not fail on its own progress.
func (c *Client) Delete(ctx context.Context, sha256 string) error {
	auth, err := c.authHeader("delete", sha256)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.server+"/"+sha256, nil)
	if err != nil {
		return fmt.Errorf("blob: build delete request: %w", err)
	}
	req.Header.Set("Authorization", auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("blob: delete at %s: %w", c.server, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return newStatusError(resp.StatusCode, "blob: delete rejected (%s)%s", resp.Status, detail(msg))
	}
	return nil
}

// authHeader mints a signed BUD-01 authorization for one verb ("upload", "get")
// bound to one blob hash, valid for authTTL. Returns "Nostr <base64-event>".
func (c *Client) authHeader(verb, sha256 string) (string, error) {
	now := time.Now()
	evt := nostr.Event{
		CreatedAt: nostr.Timestamp(now.Unix()),
		Kind:      authKind,
		Content:   "tendrils " + verb,
		Tags: nostr.Tags{
			{"t", verb},
			{"expiration", strconv.FormatInt(now.Add(authTTL).Unix(), 10)},
			{"x", sha256},
		},
	}
	if err := evt.Sign(c.id.SecretHex()); err != nil {
		return "", fmt.Errorf("blob: sign authorization: %w", err)
	}
	raw, err := json.Marshal(&evt)
	if err != nil {
		return "", fmt.Errorf("blob: marshal authorization: %w", err)
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(raw), nil
}

// hashHex is the lowercase-hex SHA-256 of data — the Blossom content address.
func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
