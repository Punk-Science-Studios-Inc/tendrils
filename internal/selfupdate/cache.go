package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// How often a device asks, and how it backs off when asking fails.
const (
	// CheckInterval is the gap between successful checks. A day is often enough
	// to catch a fleet-wide fix and rare enough that sixty anonymous API
	// requests an hour is never a constraint.
	CheckInterval = 24 * time.Hour

	// LeaseInterval is how long a check that was *started* holds off the next
	// one. It is written before the background check is launched, so a check
	// that dies without recording anything still cannot be relaunched by every
	// command in a shell loop.
	LeaseInterval = time.Hour

	// failureBase and failureCap bound the backoff after a failed check. A
	// laptop offline for a week must not hammer the API, and must never print a
	// network error from a command that has nothing to do with the network.
	failureBase = 15 * time.Minute
	failureCap  = 6 * time.Hour
)

// Cache status values. They are three different states and the difference
// matters: "none" is what the repository legitimately reports before its first
// release, and collapsing it into either of the others is how an update check
// starts lying.
const (
	StatusOK    = "ok"    // a release was found
	StatusNone  = "none"  // the API answered, and there is nothing to offer
	StatusError = "error" // the check could not be completed
)

// Cache is the record of the last update check, written to update.json in the
// state directory. It is advisory: losing it costs one extra check.
type Cache struct {
	CheckedAt time.Time `json:"checked_at"`
	// NextCheck is when this device may ask again. Storing the decision rather
	// than recomputing it means the backoff is one comparison, and a test can
	// set it directly.
	NextCheck time.Time `json:"next_check"`
	Status    string    `json:"status"`

	Latest        string `json:"latest,omitempty"`
	Tag           string `json:"tag,omitempty"`
	URL           string `json:"url,omitempty"`
	WireFormat    int    `json:"wire_format,omitempty"`
	MinCompatible string `json:"min_compatible,omitempty"`
	Notes         string `json:"notes,omitempty"`

	Failures int    `json:"failures,omitempty"`
	Error    string `json:"error,omitempty"`
}

// LoadCache reads update.json. A missing file and an unreadable one both come
// back as found=false, which means "check again" — the conservative direction
// for a cache, since the cost is one request and the alternative is acting on
// a record that cannot be trusted. It never returns an error a caller has to
// handle: no command may fail because of an update check.
func LoadCache(path string) (Cache, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Cache{}, false
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		return Cache{}, false
	}
	if c.Status == "" {
		return Cache{}, false
	}
	return c, true
}

// SaveCache writes update.json atomically — temp file in the same directory,
// fsync, rename — so a crash mid-write cannot leave a half-parsed cache that
// suppresses every future notice.
func SaveCache(path string, c Cache) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".update-*.json")
	if err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("selfupdate: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("selfupdate: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}
	return nil
}

// Due reports whether this device may check again.
func (c Cache) Due(now time.Time) bool {
	return !now.Before(c.NextCheck)
}

// ClaimCheck records that a check is about to be attempted, holding the next
// one off by LeaseInterval. Call it before launching a background check: a
// child that never reports back (killed with its shell, crashed, out of disk)
// must not leave the cache in a state where every subsequent command launches
// another one.
func ClaimCheck(path string, now time.Time) error {
	c, ok := LoadCache(path)
	if !ok {
		c = Cache{Status: StatusError, Error: "check in progress"}
	}
	c.NextCheck = now.Add(LeaseInterval)
	return SaveCache(path, c)
}

// Record folds the outcome of a check into the cache. A successful check that
// found a release, a successful check with nothing to offer, and a failed check
// are three distinct records; err is classified by the caller passing it
// through unchanged.
func Record(path string, now time.Time, rel Release, err error) error {
	prev, _ := LoadCache(path)
	c := Cache{CheckedAt: now}
	switch {
	case err == nil:
		c.Status = StatusOK
		c.Latest, c.Tag = rel.Version, rel.Tag
		c.WireFormat, c.MinCompatible, c.Notes = rel.WireFormat, rel.MinCompatible, rel.Notes
		if a, ok := rel.Asset(ChecksumsAsset); ok {
			c.URL = a.URL
		}
		c.NextCheck = now.Add(CheckInterval)
	case isNoReleases(err):
		// Not an error and not "up to date": the repository answered, and the
		// answer was that there is nothing to install. Cached for the full
		// interval, because it is a stable state, not a hiccup.
		c.Status = StatusNone
		c.NextCheck = now.Add(CheckInterval)
	default:
		c.Status = StatusError
		c.Error = err.Error()
		c.Failures = prev.Failures + 1
		c.NextCheck = now.Add(FailureBackoff(c.Failures))
	}
	return SaveCache(path, c)
}

func isNoReleases(err error) bool {
	return errors.Is(err, ErrNoReleases)
}

// FailureBackoff is the wait after n consecutive failed checks: fifteen
// minutes doubling to a six-hour ceiling. Bounded above so a device that was
// offline for a month still notices a release the day it comes back — nothing
// here is ever abandoned, only deferred.
func FailureBackoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	if failures > 20 {
		failures = 20
	}
	d := time.Duration(math.Pow(2, float64(failures-1))) * failureBase
	if d > failureCap || d <= 0 {
		return failureCap
	}
	return d
}

// Available reports whether the cache holds a release newer than current. A
// cache from a failed or empty check offers nothing — the only safe reading of
// "we do not know" is silence.
func (c Cache) Available(current string) bool {
	if c.Status != StatusOK || c.Latest == "" {
		return false
	}
	if !IsRelease(current) {
		return true // unreleased build: any published release is news
	}
	cmp, err := CompareVersions(c.Latest, current)
	return err == nil && cmp > 0
}

// Notice is the short message a command prints when a newer release exists, or
// "" when there is nothing to say. It is deliberately two or three lines: it
// appears beside output the owner actually asked for.
//
// wireFormat is the protocol generation this build speaks. When the cached
// release declares a different one, the notice says so — a fleet that upgrades
// one device across a wire-format boundary is worse off than one that upgrades
// none, so the warning belongs at the first mention, not only at the prompt.
func (c Cache) Notice(current string, wireFormat int) string {
	if !c.Available(current) {
		return ""
	}
	msg := fmt.Sprintf("A new version is available: %s (you have %s)\n", c.Latest, current)
	if !IsRelease(current) {
		msg = fmt.Sprintf("A release is available: %s (you are running an unreleased build: %s)\n", c.Latest, current)
	}
	if boundary, why := Boundary(c.Release(), current, wireFormat); boundary {
		msg += fmt.Sprintf("This is a coordinated upgrade — %s.\nUpgrade every device together; see docs/UPGRADING.md.\n", why)
	}
	if c.Notes != "" {
		msg += c.Notes + "\n"
	}
	return msg + "Run 'tendrils upgrade' to install it.\n"
}

// Release reconstructs enough of a cached check for Boundary to judge it, so a
// caller with only the cache (the daemon banner, a CLI notice) applies exactly
// the same rule as one holding the live release.
func (c Cache) Release() Release {
	return Release{
		Tag:           c.Tag,
		Version:       c.Latest,
		WireFormat:    c.WireFormat,
		MinCompatible: c.MinCompatible,
		Notes:         c.Notes,
	}
}
