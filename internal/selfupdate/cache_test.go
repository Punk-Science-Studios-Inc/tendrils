package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cachePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "update.json")
}

func TestCacheRoundTrip(t *testing.T) {
	path := cachePath(t)
	if _, found := LoadCache(path); found {
		t.Error("a cache that has never been written is not found")
	}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	rel := Release{Tag: "v0.4.0", Version: "0.4.0", WireFormat: 2, MinCompatible: "0.4.0",
		Assets: []Asset{{Name: ChecksumsAsset, URL: "https://example/checksums.txt"}}}
	if err := Record(path, now, rel, nil); err != nil {
		t.Fatal(err)
	}
	c, found := LoadCache(path)
	if !found {
		t.Fatal("cache should be found after Record")
	}
	if c.Status != StatusOK || c.Latest != "0.4.0" || c.WireFormat != 2 || c.URL == "" {
		t.Fatalf("cache = %+v", c)
	}
	if !c.NextCheck.Equal(now.Add(CheckInterval)) {
		t.Errorf("next check = %v, want %v", c.NextCheck, now.Add(CheckInterval))
	}
	if c.Due(now.Add(23 * time.Hour)) {
		t.Error("a successful check must not be repeated within the interval")
	}
	if !c.Due(now.Add(25 * time.Hour)) {
		t.Error("the check is due again after the interval")
	}
}

// A cache file that cannot be parsed is treated as absent, so the next command
// checks again. The other reading — trusting a record that could not be read —
// is how a notice would be suppressed forever by one bad write.
func TestCorruptCacheIsTreatedAsAbsent(t *testing.T) {
	path := cachePath(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found := LoadCache(path); found {
		t.Error("a corrupt cache must not be trusted")
	}
}

// The state the repository is in today. It is recorded as its own status and
// held for the full interval — it is a stable answer, not a hiccup — and it
// produces no notice, because there is nothing to tell anyone.
func TestNoReleasesIsCachedAsItsOwnStatusAndSaysNothing(t *testing.T) {
	path := cachePath(t)
	now := time.Now()
	if err := Record(path, now, Release{}, ErrNoReleases); err != nil {
		t.Fatal(err)
	}
	c, found := LoadCache(path)
	if !found {
		t.Fatal("the check happened and must be recorded")
	}
	if c.Status != StatusNone {
		t.Errorf("status = %q, want %q", c.Status, StatusNone)
	}
	if c.Status == StatusError || c.Error != "" {
		t.Error("an empty repository is not a failed check")
	}
	if c.Available("0.3.1") {
		t.Error("nothing published means nothing available")
	}
	if notice := c.Notice("0.3.1", 1); notice != "" {
		t.Errorf("notice = %q, want silence", notice)
	}
	if c.Due(now.Add(time.Hour)) {
		t.Error("an answered check must not be repeated an hour later")
	}
}

// A device offline for a week must not hammer the API on every command, and
// must never print a network error from a command that has nothing to do with
// the network. But the wait is bounded: nothing here is abandoned, only
// deferred.
func TestFailedChecksBackOffAndStaySilent(t *testing.T) {
	path := cachePath(t)
	now := time.Now()
	netErr := errors.New("dial tcp: no route to host")

	var last time.Duration
	for i := 1; i <= 6; i++ {
		if err := Record(path, now, Release{}, netErr); err != nil {
			t.Fatal(err)
		}
		c, _ := LoadCache(path)
		if c.Status != StatusError || c.Failures != i {
			t.Fatalf("after %d failures: status=%q failures=%d", i, c.Status, c.Failures)
		}
		if c.Available("0.3.1") || c.Notice("0.3.1", 1) != "" {
			t.Error("a failed check knows nothing and must say nothing")
		}
		wait := c.NextCheck.Sub(now)
		if wait < last {
			t.Errorf("backoff shrank: %v then %v", last, wait)
		}
		if wait > failureCap {
			t.Errorf("backoff %v exceeds the %v ceiling", wait, failureCap)
		}
		last = wait
	}
	if last != failureCap {
		t.Errorf("backoff should reach its ceiling, got %v", last)
	}
	// A recovered check clears the failure count rather than staying penalised.
	if err := Record(path, now, Release{Tag: "v0.4.0", Version: "0.4.0"}, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := LoadCache(path); c.Failures != 0 || c.Error != "" {
		t.Errorf("a success must clear the failure state: %+v", c)
	}
}

// The lease: a check that is started but never reports back must still hold off
// the next one, or a script running `tendrils status` in a loop launches a
// checker every iteration.
func TestClaimHoldsOffTheNextCheck(t *testing.T) {
	path := cachePath(t)
	now := time.Now()
	if err := ClaimCheck(path, now); err != nil {
		t.Fatal(err)
	}
	c, found := LoadCache(path)
	if !found {
		t.Fatal("the claim must be persisted")
	}
	if c.Due(now.Add(time.Minute)) {
		t.Error("a claimed check must not be re-launched a minute later")
	}
	if !c.Due(now.Add(LeaseInterval + time.Minute)) {
		t.Error("a claim that was never fulfilled must expire")
	}
	// A claim must not invent a release to announce.
	if c.Notice("0.3.1", 1) != "" {
		t.Error("a claim is not a result")
	}
}

// A claim over a good cache must not lose the release it already knows about,
// or every check would silence the notice it just printed.
func TestClaimKeepsTheKnownRelease(t *testing.T) {
	path := cachePath(t)
	now := time.Now()
	if err := Record(path, now, Release{Tag: "v0.4.0", Version: "0.4.0"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := ClaimCheck(path, now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadCache(path)
	if !c.Available("0.3.1") {
		t.Error("claiming the next check discarded the release already known")
	}
}

func TestNoticeSaysWhatToDo(t *testing.T) {
	path := cachePath(t)
	if err := Record(path, time.Now(), Release{Tag: "v0.4.0", Version: "0.4.0"}, nil); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadCache(path)

	notice := c.Notice("0.3.1", 1)
	for _, want := range []string{"0.4.0", "0.3.1", "tendrils upgrade"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q should mention %q", notice, want)
		}
	}
	if c.Notice("0.4.0", 1) != "" {
		t.Error("a device on the latest release must not be nagged")
	}
	if c.Notice("0.5.0", 1) != "" {
		t.Error("a device ahead of the latest release must not be nagged")
	}
	// An unreleased build cannot be compared, so the notice says so instead of
	// pretending to a version arithmetic it cannot do.
	dev := c.Notice("dev", 1)
	if !strings.Contains(dev, "unreleased") {
		t.Errorf("dev notice = %q, want it to say the build is unreleased", dev)
	}
}

// The notice, not just the prompt, carries the coordinated-upgrade warning: by
// the time someone types `tendrils upgrade` on device three of five, the damage
// is a fleet that disagrees invisibly.
func TestNoticeWarnsAboutAWireFormatBoundary(t *testing.T) {
	path := cachePath(t)
	rel := Release{Tag: "v0.5.0", Version: "0.5.0", WireFormat: 2, Notes: "chunked sealing"}
	if err := Record(path, time.Now(), rel, nil); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadCache(path)
	notice := c.Notice("0.4.0", 1)
	for _, want := range []string{"coordinated", "every device", "UPGRADING.md", "chunked sealing"} {
		if !strings.Contains(strings.ToLower(notice), strings.ToLower(want)) {
			t.Errorf("notice %q should mention %q", notice, want)
		}
	}
	// Same release, same wire format: no warning.
	if got := (Cache{Status: StatusOK, Latest: "0.5.0", WireFormat: 1}).Notice("0.4.0", 1); strings.Contains(strings.ToLower(got), "coordinated") {
		t.Errorf("no boundary here: %q", got)
	}
}

// SaveCache writes through a temp file and a rename, the same discipline as
// every other write in this project: a crash mid-write must not leave a
// half-parsed cache that suppresses every future notice.
func TestSaveCacheLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update.json")
	if err := SaveCache(path, Cache{Status: StatusOK, Latest: "0.4.0"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "update.json" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("state dir holds %v, want only update.json", names)
	}
}
