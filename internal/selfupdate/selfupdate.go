// Package selfupdate finds, verifies and installs a newer Tendrils release.
//
// It exists for a correctness reason, not a convenience one. A device on a
// stale build cannot always tell that it is broken: the created_at change of
// 2026-07-23 (see "Two clocks" in AGENTS.md) means a pre-change device has
// every event it publishes dropped by the relay and is never told. The only
// way to diagnose a fleet that has drifted is to ask each device what it runs
// — so a device that is behind should say so on its own.
//
// Three rules shape this package, and all three come from failures this project
// has already had:
//
//   - An unverified binary is worse than a stale one. The archive is checked
//     against the release's checksums.txt, a missing entry is a refusal, and
//     the replacement is executed and asked its version *before* it is moved
//     into place. Presence must never stand in for correctness.
//   - The replacement is a rename, never a write over the destination. A failed
//     upgrade leaves the working binary exactly where it was.
//   - Crossing a declared wire-format boundary is a fleet-wide event. Moving
//     one device across it manufactures the split-brain the boundary warns
//     about, so it takes an explicit act.
//
// The package is pure and testable: the GitHub endpoint is a field, so tests
// point it at an httptest.Server. It knows nothing about cobra or config.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultSlug is the repository releases are published from.
const DefaultSlug = "Punk-Science-Studios-Inc/tendrils"

// DefaultAPI is the public GitHub REST endpoint. No token, ever: a sync daemon
// on a Pi should not need a GitHub credential to learn it is out of date, and a
// token in the state dir would be a new secret to protect for no gain.
const DefaultAPI = "https://api.github.com"

// ReleaseMetaAsset is the release asset carrying the coordinated-upgrade
// marker. See ReleaseMeta.
const ReleaseMetaAsset = "release.json"

// ChecksumsAsset is the name every installer and this updater verifies
// against. Renaming it breaks every installer in the field.
const ChecksumsAsset = "checksums.txt"

// WireFormatUnreadable marks a release that published a release.json which
// could not be parsed. It is deliberately *not* the same as "did not publish
// one": a marker that exists but cannot be read is treated as a boundary,
// because the alternative is crossing one silently.
const WireFormatUnreadable = -1

var (
	// ErrNoReleases means the repository has published nothing this build could
	// upgrade to. It is a state, not a failure: it is what the API says both
	// before the first release is cut and when every release so far is a
	// prerelease and this build is not. Callers must report it as its own thing
	// — reporting it as "up to date" is a lie, and reporting it as an error
	// makes a healthy new install look broken.
	ErrNoReleases = errors.New("selfupdate: no releases published")

	// ErrRateLimited is the anonymous API's 60-requests-per-hour ceiling. It is
	// transient and must never be shown as "no update".
	ErrRateLimited = errors.New("selfupdate: GitHub API rate limit reached")

	// ErrNoAsset means the release has no archive for this OS/arch.
	ErrNoAsset = errors.New("selfupdate: no release archive for this platform")

	// ErrChecksumMissing means the archive is not listed in checksums.txt. This
	// is a refusal, not a warning: an unlisted archive cannot be verified, and
	// an unverified binary is worse than a stale one.
	ErrChecksumMissing = errors.New("selfupdate: archive is not listed in the release checksums")

	// ErrChecksumMismatch means the bytes downloaded are not the bytes the
	// release published.
	ErrChecksumMismatch = errors.New("selfupdate: checksum mismatch")
)

// Asset is one file published with a release.
type Asset struct {
	Name string
	URL  string
	Size int64
}

// Release is one published release, plus whatever its release.json declared.
type Release struct {
	Tag         string // e.g. "v0.4.0"
	Version     string // e.g. "0.4.0" — the tag without its "v"
	Prerelease  bool
	PublishedAt time.Time
	Assets      []Asset

	// WireFormat is the protocol generation this release speaks, from its
	// release.json. Zero means the release declared nothing (releases predating
	// the marker); WireFormatUnreadable means it declared something unparsable.
	WireFormat int
	// MinCompatible is the oldest version this release can still interoperate
	// with. A device below it is crossing a boundary by upgrading.
	MinCompatible string
	// Notes is the one-line operational note from release.json, if any.
	Notes string
}

// Asset returns the named asset.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// ReleaseMeta is the release.json asset: the coordinated-upgrade marker.
//
// A marker file rather than a convention in the release notes, because notes
// are prose — easy to publish, easy to typo, and impossible to act on
// mechanically. This is the one piece of release data a machine must not
// misread.
type ReleaseMeta struct {
	// WireFormat is bumped whenever old and new devices would disagree
	// invisibly: the event codec, the sealing format, or an arbitration rule.
	WireFormat int `json:"wire_format"`
	// MinCompatible is the oldest version this release interoperates with.
	MinCompatible string `json:"min_compatible,omitempty"`
	// Notes is one line for the owner, shown when the boundary is hit.
	Notes string `json:"notes,omitempty"`
}

// Client queries a GitHub releases API. The zero value is not usable; use New.
type Client struct {
	// API is the base URL of the releases API. Tests point it at an
	// httptest.Server; nothing else should change it.
	API string
	// Slug is the "owner/repo" releases are published under.
	Slug string
	// HTTP is the transport. Requests carry their own deadlines, so this client
	// deliberately has no global timeout: a check has seconds, a download has
	// minutes, and one timeout cannot serve both.
	HTTP *http.Client
	// UserAgent identifies this build to the API.
	UserAgent string
	// AllowPrerelease offers prereleases. Check sets this from the running
	// version when it is left false: a device already on an -rc build should be
	// told about the next -rc, and a stable build should never be.
	AllowPrerelease bool
	// MaxArchiveBytes caps a download. A release archive holds two Go binaries;
	// anything far past that is not one.
	MaxArchiveBytes int64
}

// New returns a Client pointed at the public GitHub API.
func New() *Client {
	return &Client{
		API:             DefaultAPI,
		Slug:            DefaultSlug,
		HTTP:            &http.Client{},
		UserAgent:       "tendrils-selfupdate",
		MaxArchiveBytes: 512 << 20,
	}
}

// checkTimeout bounds a background check. It must be short: the check runs
// beside real work and its failure is meant to be invisible.
const checkTimeout = 20 * time.Second

// Check reports the newest applicable release and whether it is newer than
// current.
//
// current is buildinfo's version, which is "dev" for anything unreleased. A
// dev build cannot be compared, so any release counts as available — a Pi
// built from source and then forgotten is exactly the stale device this
// feature exists to surface.
//
// A repository with nothing to offer returns ErrNoReleases. Callers must
// distinguish it; see the comment on that error.
func (c *Client) Check(ctx context.Context, current string) (Release, bool, error) {
	ctx, cancel := withTimeout(ctx, checkTimeout)
	defer cancel()

	allowPre := c.AllowPrerelease || IsPrerelease(current)
	rel, err := c.latest(ctx, allowPre)
	if err != nil {
		return Release{}, false, err
	}
	c.loadMeta(ctx, &rel)

	if !IsRelease(current) {
		// Unreleased build: nothing to compare against, so offer the release and
		// let the caller phrase it honestly ("you are running a dev build").
		return rel, true, nil
	}
	cmp, err := CompareVersions(rel.Version, current)
	if err != nil {
		return Release{}, false, fmt.Errorf("selfupdate: comparing %q with %q: %w", rel.Version, current, err)
	}
	return rel, cmp > 0, nil
}

// ReleaseByTag fetches one exact release, for --version pinning and rollback.
func (c *Client) ReleaseByTag(ctx context.Context, tag string) (Release, error) {
	ctx, cancel := withTimeout(ctx, checkTimeout)
	defer cancel()

	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	var gh ghRelease
	if err := c.getJSON(ctx, c.API+"/repos/"+c.Slug+"/releases/tags/"+tag, &gh); err != nil {
		if errors.Is(err, errNotFound) {
			return Release{}, fmt.Errorf("selfupdate: no release tagged %s", tag)
		}
		return Release{}, err
	}
	rel, err := gh.toRelease()
	if err != nil {
		return Release{}, err
	}
	c.loadMeta(ctx, &rel)
	return rel, nil
}

// latest resolves the newest applicable release.
//
// /releases/latest is tried first because it is one request and it is what the
// installers use — but it 404s both when nothing has ever been published and
// when every release so far is a prerelease, and those are different states.
// The listing endpoint separates them, so a 404 falls through to it rather than
// being reported as "nothing there".
func (c *Client) latest(ctx context.Context, allowPre bool) (Release, error) {
	var gh ghRelease
	err := c.getJSON(ctx, c.API+"/repos/"+c.Slug+"/releases/latest", &gh)
	switch {
	case err == nil && !gh.Draft && (!gh.Prerelease || allowPre):
		if rel, rerr := gh.toRelease(); rerr == nil {
			return rel, nil
		}
		// The newest release is tagged with something that is not a version.
		// The listing below skips it and finds the newest that is: one oddly
		// tagged release must not disable updates for a whole fleet.
	case err != nil && !errors.Is(err, errNotFound):
		return Release{}, err
	}

	var list []ghRelease
	if err := c.getJSON(ctx, c.API+"/repos/"+c.Slug+"/releases?per_page=30", &list); err != nil {
		if errors.Is(err, errNotFound) {
			// The repository itself is unreachable anonymously: it does not exist,
			// or it is private. Not "up to date", and not a transient failure.
			return Release{}, fmt.Errorf("%w (repository %s published none, or is not public)", ErrNoReleases, c.Slug)
		}
		return Release{}, err
	}

	var candidates []Release
	for _, g := range list {
		if g.Draft || (g.Prerelease && !allowPre) {
			continue
		}
		rel, err := g.toRelease()
		if err != nil {
			continue // a tag that is not a version is not a release this can install
		}
		candidates = append(candidates, rel)
	}
	if len(candidates) == 0 {
		return Release{}, ErrNoReleases
	}
	sort.Slice(candidates, func(i, j int) bool {
		ci, _ := parseSemver(candidates[i].Version)
		cj, _ := parseSemver(candidates[j].Version)
		return ci.compare(cj) > 0
	})
	return candidates[0], nil
}

// loadMeta folds release.json into the release. A release that publishes no
// marker leaves WireFormat zero — releases predate the marker, and treating
// "absent" as a boundary would refuse every one of them. A marker that is
// present but unparsable is a different matter: see WireFormatUnreadable.
func (c *Client) loadMeta(ctx context.Context, rel *Release) {
	asset, ok := rel.Asset(ReleaseMetaAsset)
	if !ok {
		return
	}
	body, err := c.getBytes(ctx, asset.URL, 64<<10)
	if err != nil {
		return // transient: a fetch failure must not invent a boundary
	}
	var meta ReleaseMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		rel.WireFormat = WireFormatUnreadable
		return
	}
	rel.WireFormat, rel.MinCompatible, rel.Notes = meta.WireFormat, meta.MinCompatible, meta.Notes
}

// Boundary reports whether upgrading a build at current to rel crosses a
// declared wire-format boundary, and why.
//
// wireFormat is the protocol generation the running build speaks
// (buildinfo.WireFormat). Two things count as a crossing:
//
//   - the release speaks a different generation than this build, or
//   - this build is older than the release's min_compatible, so afterwards it
//     could no longer interoperate with devices left behind.
//
// A release that declares nothing crosses nothing: it cannot, because nothing
// before the marker existed changed the wire format.
func Boundary(rel Release, current string, wireFormat int) (bool, string) {
	switch {
	case rel.WireFormat == WireFormatUnreadable:
		return true, fmt.Sprintf("release %s publishes a %s that could not be read, so whether it changes the wire format is unknown", rel.Version, ReleaseMetaAsset)
	case rel.WireFormat != 0 && rel.WireFormat != wireFormat:
		return true, fmt.Sprintf("release %s speaks wire format %d; this build speaks %d", rel.Version, rel.WireFormat, wireFormat)
	}
	if rel.MinCompatible != "" && IsRelease(current) {
		if cmp, err := CompareVersions(current, rel.MinCompatible); err == nil && cmp < 0 {
			return true, fmt.Sprintf("release %s interoperates only with %s and newer; this build is %s", rel.Version, rel.MinCompatible, current)
		}
	}
	return false, ""
}

// --- GitHub JSON -------------------------------------------------------------

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func (g ghRelease) toRelease() (Release, error) {
	sv, err := parseSemver(g.TagName)
	if err != nil {
		return Release{}, fmt.Errorf("selfupdate: release tag %q is not a version: %w", g.TagName, err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(g.TagName), "v")
	rel := Release{
		Tag:         g.TagName,
		Version:     version,
		Prerelease:  g.Prerelease || sv.pre != "",
		PublishedAt: g.PublishedAt,
	}
	for _, a := range g.Assets {
		rel.Assets = append(rel.Assets, Asset{Name: a.Name, URL: a.URL, Size: a.Size})
	}
	return rel, nil
}

// errNotFound is the 404 sentinel latest() branches on. It never escapes this
// package unwrapped: 404 means different things at different endpoints, and
// each caller says which.
var errNotFound = errors.New("selfupdate: not found")

func (c *Client) getJSON(ctx context.Context, url string, into any) error {
	body, err := c.getBytes(ctx, url, 4<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("selfupdate: %s returned unreadable JSON: %w", url, err)
	}
	return nil
}

func (c *Client) getBytes(ctx context.Context, url string, limit int64) ([]byte, error) {
	resp, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("selfupdate: read %s: %w", url, err)
	}
	return body, nil
}

// get performs one request and classifies the failure. Every non-200 must come
// back as something a caller can act on: rate limiting and a missing release
// look identical in a body, and confusing either with "no update" is how an
// update check silently stops working.
func (c *Client) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: %w", err)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: fetch %s: %w", url, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp, nil
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, errNotFound
	case resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0",
		resp.StatusCode == http.StatusTooManyRequests:
		resp.Body.Close()
		return nil, ErrRateLimited
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("selfupdate: %s returned %s", url, resp.Status)
	}
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
