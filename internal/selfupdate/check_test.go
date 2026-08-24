package selfupdate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func stable(t *testing.T, tag string) *fakeRelease {
	return release(t, tag, "linux", "amd64", map[string]string{"tendrils": "binary " + tag}, nil)
}

func TestCheckReportsANewerRelease(t *testing.T) {
	gh := newFakeGitHub(t, stable(t, "v0.4.0"), stable(t, "v0.3.1"))
	rel, available, err := gh.client().Check(context.Background(), "0.3.1")
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Error("0.4.0 should be offered to a device on 0.3.1")
	}
	if rel.Version != "0.4.0" {
		t.Errorf("latest = %q, want 0.4.0", rel.Version)
	}
}

func TestCheckOnTheLatestReleaseOffersNothing(t *testing.T) {
	gh := newFakeGitHub(t, stable(t, "v0.4.0"))
	_, available, err := gh.client().Check(context.Background(), "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Error("a device already on the latest release must not be offered it")
	}
}

func TestCheckAheadOfTheLatestReleaseOffersNothing(t *testing.T) {
	// A hand-built binary stamped ahead of what is published. Offering it a
	// "newer" release that is older is how an updater talks someone into a
	// downgrade they did not ask for.
	gh := newFakeGitHub(t, stable(t, "v0.4.0"))
	_, available, err := gh.client().Check(context.Background(), "0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Error("a release older than the running build must not be offered")
	}
}

// The state the repository is in today: the pipeline exists, no tag has ever
// been pushed, and /releases/latest answers 404. It must come back as its own
// condition — reported as "up to date" it is a lie that hides the whole
// feature, and reported as an error it makes a fresh install look broken.
func TestCheckWithNoReleasesIsItsOwnState(t *testing.T) {
	gh := newFakeGitHub(t)
	rel, available, err := gh.client().Check(context.Background(), "0.3.1")
	if !errors.Is(err, ErrNoReleases) {
		t.Fatalf("err = %v, want ErrNoReleases", err)
	}
	if available {
		t.Error("nothing can be available when nothing is published")
	}
	if rel.Version != "" {
		t.Errorf("no release should be returned, got %q", rel.Version)
	}
}

// A repository that answers 404 to the listing endpoint too is either gone or
// private. Also ErrNoReleases — there is nothing to install — but the message
// has to name the possibility, because "private repo" is a five-minute fix and
// "no releases yet" is a different one.
func TestCheckWithAnInaccessibleRepositorySaysSo(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.repoMissing = true
	_, _, err := gh.client().Check(context.Background(), "0.3.1")
	if !errors.Is(err, ErrNoReleases) {
		t.Fatalf("err = %v, want ErrNoReleases", err)
	}
	if got := err.Error(); !strings.Contains(got, "not public") {
		t.Errorf("error should mention the repository may not be public: %q", got)
	}
}

// GitHub's /releases/latest excludes prereleases, so a repository whose only
// tag is v0.1.0-rc.1 answers 404 there while very much having a release. A
// check that stopped at the 404 would report "nothing published" for a fleet
// that has something published.
func TestCheckFallsBackToTheListingWhenOnlyPrereleasesExist(t *testing.T) {
	pre := stable(t, "v0.1.0-rc.1")
	pre.prerelease = true
	gh := newFakeGitHub(t, pre)

	// A stable build is not offered a release candidate.
	if _, available, err := gh.client().Check(context.Background(), "0.0.9"); !errors.Is(err, ErrNoReleases) || available {
		t.Errorf("stable build: err = %v, available = %v; want ErrNoReleases, false", err, available)
	}
	// A build that is itself a prerelease is.
	rel, available, err := gh.client().Check(context.Background(), "0.1.0-rc.0")
	if err != nil {
		t.Fatal(err)
	}
	if !available || rel.Version != "0.1.0-rc.1" {
		t.Errorf("prerelease build: got %q available=%v, want 0.1.0-rc.1 available", rel.Version, available)
	}
}

// GitHub's /releases/latest is "most recent", not "highest version", so one
// release tagged something that is not a version can turn up there. Falling
// over on it would disable updates for the whole fleet until the tag was
// deleted; the listing endpoint skips it instead.
func TestCheckFallsBackWhenTheNewestTagIsNotAVersion(t *testing.T) {
	junk := stable(t, "nightly")
	gh := newFakeGitHub(t, junk, stable(t, "v0.4.0"))
	gh.forceLatest = junk

	rel, available, err := gh.client().Check(context.Background(), "0.1.0")
	if err != nil {
		t.Fatalf("one unversioned tag must not break the check: %v", err)
	}
	if !available || rel.Version != "0.4.0" {
		t.Errorf("got %q available=%v, want 0.4.0 available", rel.Version, available)
	}
}

func TestCheckSkipsDrafts(t *testing.T) {
	draft := stable(t, "v0.9.0")
	draft.draft = true
	gh := newFakeGitHub(t, draft, stable(t, "v0.4.0"))
	rel, _, err := gh.client().Check(context.Background(), "0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "0.4.0" {
		t.Errorf("latest = %q, want 0.4.0 — a draft is not published", rel.Version)
	}
}

// An unreleased build cannot be compared, and a source build on a forgotten Pi
// is exactly the stale device this feature exists to surface. It is offered the
// release, and the caller phrases it honestly.
func TestCheckOffersAReleaseToAnUnreleasedBuild(t *testing.T) {
	gh := newFakeGitHub(t, stable(t, "v0.4.0"))
	rel, available, err := gh.client().Check(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !available || rel.Version != "0.4.0" {
		t.Errorf("dev build: got %q available=%v, want 0.4.0 available", rel.Version, available)
	}
}

// A tag that is not a version is not something this can install. It is skipped,
// not fatal: one bad tag in a repository must not disable updates entirely.
func TestCheckIgnoresMalformedTags(t *testing.T) {
	junk := stable(t, "nightly")
	gh := newFakeGitHub(t, junk, stable(t, "v0.4.0"))
	rel, _, err := gh.client().Check(context.Background(), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "0.4.0" {
		t.Errorf("latest = %q, want 0.4.0", rel.Version)
	}
}

// Rate limiting is transient and must never read as "no update available":
// silently reporting current when the API refused to answer is how a fleet
// stops noticing releases altogether.
func TestCheckSurfacesRateLimiting(t *testing.T) {
	gh := newFakeGitHub(t, stable(t, "v0.4.0"))
	gh.rateLimited = true
	_, available, err := gh.client().Check(context.Background(), "0.3.1")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if available {
		t.Error("a rate-limited check knows nothing and must offer nothing")
	}
	if errors.Is(err, ErrNoReleases) {
		t.Error("a rate-limited check must not be mistaken for an empty repository")
	}
}

func TestReleaseByTagPinsAndAllowsDowngrade(t *testing.T) {
	gh := newFakeGitHub(t, stable(t, "v0.4.0"), stable(t, "v0.3.0"))
	rel, err := gh.client().ReleaseByTag(context.Background(), "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "0.3.0" {
		t.Errorf("pinned release = %q, want 0.3.0", rel.Version)
	}
	// The "v" is optional at the command line.
	if _, err := gh.client().ReleaseByTag(context.Background(), "0.3.0"); err != nil {
		t.Errorf("bare version should resolve: %v", err)
	}
	if _, err := gh.client().ReleaseByTag(context.Background(), "v9.9.9"); err == nil {
		t.Error("a tag that does not exist must be an error, not a silent no-op")
	}
}

func TestCheckReadsTheWireFormatMarker(t *testing.T) {
	rel := release(t, "v0.5.0", "linux", "amd64", map[string]string{"tendrils": "x"},
		&ReleaseMeta{WireFormat: 2, MinCompatible: "0.5.0", Notes: "chunked sealing"})
	gh := newFakeGitHub(t, rel)
	got, _, err := gh.client().Check(context.Background(), "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if got.WireFormat != 2 || got.MinCompatible != "0.5.0" || got.Notes != "chunked sealing" {
		t.Fatalf("release.json not folded in: %+v", got)
	}
	if boundary, why := Boundary(got, "0.4.0", 1); !boundary {
		t.Error("wire format 2 against a build speaking 1 is a boundary")
	} else if why == "" {
		t.Error("a boundary must say why")
	}
}

// A release.json that exists but cannot be parsed is treated as a boundary. The
// alternative is deciding "probably fine" about the one file whose entire job
// is to say when it is not.
func TestUnreadableWireFormatMarkerIsABoundary(t *testing.T) {
	rel := stable(t, "v0.5.0")
	rel.assets[ReleaseMetaAsset] = []byte("{ this is not json")
	gh := newFakeGitHub(t, rel)
	got, _, err := gh.client().Check(context.Background(), "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if got.WireFormat != WireFormatUnreadable {
		t.Fatalf("WireFormat = %d, want %d", got.WireFormat, WireFormatUnreadable)
	}
	if boundary, _ := Boundary(got, "0.4.0", 1); !boundary {
		t.Error("an unreadable marker must not be read as 'no boundary'")
	}
}

// Releases predating the marker declare nothing, and must not all become
// boundaries — that would make the confirmation meaningless the first time it
// mattered.
func TestAReleaseWithoutAMarkerIsNotABoundary(t *testing.T) {
	gh := newFakeGitHub(t, stable(t, "v0.4.0"))
	got, _, err := gh.client().Check(context.Background(), "0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if boundary, why := Boundary(got, "0.3.0", 1); boundary {
		t.Errorf("release with no release.json flagged as a boundary: %s", why)
	}
}

// min_compatible catches the other direction: the release speaks this wire
// format, but is too new to interoperate with the build being upgraded from.
func TestMinCompatibleIsABoundary(t *testing.T) {
	rel := Release{Version: "0.6.0", WireFormat: 1, MinCompatible: "0.5.0"}
	if boundary, _ := Boundary(rel, "0.4.0", 1); !boundary {
		t.Error("0.4.0 is below min_compatible 0.5.0 and must be flagged")
	}
	if boundary, _ := Boundary(rel, "0.5.0", 1); boundary {
		t.Error("0.5.0 meets min_compatible and must not be flagged")
	}
}
