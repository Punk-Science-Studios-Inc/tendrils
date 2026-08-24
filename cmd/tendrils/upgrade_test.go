package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/selfupdate"
)

// A minimal stand-in for the releases API. The full-fidelity fake lives in
// internal/selfupdate; this one exists so the command's own decisions — exit
// codes, the empty-repository message, the coordinated-upgrade refusal — can be
// asserted without a network.
type fakeAPI struct {
	server *httptest.Server
	// releases is what the API reports; empty means a repository that has
	// published nothing, which is the state the real one is in today.
	releases []map[string]any
}

func newFakeAPI(t *testing.T, releases ...map[string]any) *fakeAPI {
	t.Helper()
	f := &fakeAPI{releases: releases}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			if len(f.releases) == 0 {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(f.releases[0])
		case strings.HasSuffix(r.URL.Path, "/releases"):
			json.NewEncoder(w).Encode(f.releases)
		case r.URL.Path == "/release.json":
			w.Write([]byte(`{"wire_format": 99, "min_compatible": "9.0.0", "notes": "sealing format changed"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) client() *selfupdate.Client {
	c := selfupdate.New()
	c.API = f.server.URL
	c.Slug = "test/tendrils"
	return c
}

// releaseJSON builds one API release. withMeta attaches a release.json asset
// declaring a wire format this build does not speak.
func releaseJSON(f *fakeAPI, tag string, withMeta bool) map[string]any {
	assets := []map[string]any{}
	if withMeta {
		assets = append(assets, map[string]any{
			"name":                 selfupdate.ReleaseMetaAsset,
			"browser_download_url": f.server.URL + "/release.json",
			"size":                 100,
		})
	}
	return map[string]any{
		"tag_name": tag, "draft": false, "prerelease": false,
		"published_at": "2026-08-01T00:00:00Z", "assets": assets,
	}
}

// testCmd is a command wired to buffers: no terminal, so every confirmation is
// non-interactive, which is the case that must refuse rather than hang.
func testCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("TENDRILS_HOME", t.TempDir()) // isolate the update cache
	cmd := &cobra.Command{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetIn(strings.NewReader(""))
	return cmd, out, errOut
}

// Acceptance criterion 1: --check exits non-zero when an upgrade is available,
// so a script or a monitoring job can act on it.
func TestUpgradeCheckExitsNonZeroWhenAnUpgradeExists(t *testing.T) {
	f := newFakeAPI(t)
	f.releases = []map[string]any{releaseJSON(f, "v9.9.9", false)}
	cmd, out, _ := testCmd(t)

	err := runUpgrade(context.Background(), cmd, f.client(), upgradeOptions{check: true})
	var code exitCode
	if !errors.As(err, &code) || int(code) != 1 {
		t.Fatalf("err = %v, want exit code 1", err)
	}
	if !strings.Contains(out.String(), "9.9.9") {
		t.Errorf("output should name the available release: %q", out)
	}
	if !strings.Contains(out.String(), "tendrils upgrade") {
		t.Errorf("output should say what to run: %q", out)
	}
}

// The state the repository is in today, and therefore the first thing that will
// actually run. It must be neither an error nor "you are up to date".
func TestUpgradeWithNoReleasesSaysSoAndSucceeds(t *testing.T) {
	f := newFakeAPI(t) // no releases at all: /releases/latest 404s
	cmd, out, _ := testCmd(t)

	if err := runUpgrade(context.Background(), cmd, f.client(), upgradeOptions{}); err != nil {
		t.Fatalf("an empty repository is not a failure: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "No releases have been published") {
		t.Errorf("output should say no releases exist: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "up to date") || strings.Contains(strings.ToLower(got), "latest release") {
		t.Errorf("nothing published is not the same as being current: %q", got)
	}

	// --check agrees, and exits zero: there is nothing to upgrade to.
	cmd2, out2, _ := testCmd(t)
	if err := runUpgrade(context.Background(), cmd2, f.client(), upgradeOptions{check: true}); err != nil {
		t.Fatalf("--check against an empty repository must exit zero: %v", err)
	}
	if !strings.Contains(out2.String(), "No releases have been published") {
		t.Errorf("--check output: %q", out2)
	}
}

// Acceptance criterion 8. --yes deliberately does not satisfy this: it means
// "do not ask me the routine questions", and moving one device across a wire
// format boundary is not routine — it manufactures a fleet that disagrees
// invisibly, which is the failure the marker exists to prevent.
func TestUpgradeRefusesToCrossAWireFormatBoundaryWithoutForce(t *testing.T) {
	f := newFakeAPI(t)
	f.releases = []map[string]any{releaseJSON(f, "v9.9.9", true)}

	cmd, out, _ := testCmd(t)
	err := runUpgrade(context.Background(), cmd, f.client(), upgradeOptions{yes: true})
	if err == nil {
		t.Fatal("crossing a declared wire-format boundary must not happen on --yes alone")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal should name the flag that would allow it: %v", err)
	}
	got := out.String()
	for _, want := range []string{"COORDINATED UPGRADE", "every device", "UPGRADING.md", "sealing format changed"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q should mention %q", got, want)
		}
	}

	// --check reports the same boundary rather than quietly offering it.
	cmd2, out2, _ := testCmd(t)
	if err := runUpgrade(context.Background(), cmd2, f.client(), upgradeOptions{check: true}); err == nil {
		t.Error("--check should still exit non-zero: an upgrade is available")
	}
	if !strings.Contains(out2.String(), "COORDINATED UPGRADE") {
		t.Errorf("--check should surface the boundary: %q", out2)
	}
}

// A check performed by the command feeds the same cache the background check
// writes, so running `upgrade` silences a notice it has just answered and does
// not leave a duplicate check queued behind it.
func TestUpgradeRecordsItsCheck(t *testing.T) {
	f := newFakeAPI(t)
	f.releases = []map[string]any{releaseJSON(f, "v9.9.9", false)}
	cmd, _, _ := testCmd(t)

	_ = runUpgrade(context.Background(), cmd, f.client(), upgradeOptions{check: true})

	path, err := updateCachePath()
	if err != nil {
		t.Fatal(err)
	}
	cache, found := selfupdate.LoadCache(path)
	if !found {
		t.Fatal("the check should have been cached")
	}
	if cache.Latest != "9.9.9" {
		t.Errorf("cached latest = %q, want 9.9.9", cache.Latest)
	}
}

// An empty repository is cached too — as its own status. Otherwise every
// command would re-ask an API that has already answered.
func TestUpgradeCachesTheEmptyRepositoryAnswer(t *testing.T) {
	f := newFakeAPI(t)
	cmd, _, _ := testCmd(t)

	if err := runUpgrade(context.Background(), cmd, f.client(), upgradeOptions{}); err != nil {
		t.Fatal(err)
	}
	path, err := updateCachePath()
	if err != nil {
		t.Fatal(err)
	}
	cache, found := selfupdate.LoadCache(path)
	if !found {
		t.Fatal("the answer should have been cached")
	}
	if cache.Status != selfupdate.StatusNone {
		t.Errorf("status = %q, want %q", cache.Status, selfupdate.StatusNone)
	}
	if cache.Notice("0.1.0", 1) != "" {
		t.Error("an empty repository must produce no notice")
	}
}

// Pinning to a tag that does not exist is an error, not a silent no-op: an
// owner rolling back to a specific version needs to know it did not happen.
func TestUpgradePinnedToAMissingTagFails(t *testing.T) {
	f := newFakeAPI(t)
	f.releases = []map[string]any{releaseJSON(f, "v9.9.9", false)}
	cmd, _, _ := testCmd(t)

	err := runUpgrade(context.Background(), cmd, f.client(), upgradeOptions{version: "v0.0.1", yes: true})
	if err == nil || !strings.Contains(err.Error(), "0.0.1") {
		t.Fatalf("err = %v, want a refusal naming the missing tag", err)
	}
}

// The notice rides along with ordinary commands, but not with the ones whose
// output is the answer to a question about this build, and not with the ones
// that are already about updates.
func TestUpdateNoticeEligibility(t *testing.T) {
	for name, want := range map[string]bool{
		"status": true, "gc": true, "repair": true, "enroll": true,
		"version": false, "upgrade": false, "daemon": false, updateCheckCmdName: false,
	} {
		if got := updateNoticeEligible(&cobra.Command{Use: name}); got != want {
			t.Errorf("%s: eligible = %v, want %v", name, got, want)
		}
	}
	if updateNoticeEligible(nil) {
		t.Error("no command, no notice")
	}
}

// The banner line the daemon prints. It reports; it never upgrades itself.
func TestDaemonBannerSummary(t *testing.T) {
	t.Setenv("TENDRILS_HOME", t.TempDir())
	path, err := updateCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if got := updateSummary("0.3.1", 1); got != "" {
		t.Errorf("no cache, no banner line: %q", got)
	}
	if err := selfupdate.SaveCache(path, selfupdate.Cache{
		Status: selfupdate.StatusOK, Latest: "0.4.0", Tag: "v0.4.0",
	}); err != nil {
		t.Fatal(err)
	}
	if got := updateSummary("0.3.1", 1); !strings.Contains(got, "0.4.0") {
		t.Errorf("banner line = %q, want it to name the release", got)
	}
	if got := updateSummary("0.4.0", 1); got != "" {
		t.Errorf("a current daemon says nothing: %q", got)
	}

	// A coordinated upgrade is called out in the banner, not just in the log.
	if err := selfupdate.SaveCache(path, selfupdate.Cache{
		Status: selfupdate.StatusOK, Latest: "0.5.0", WireFormat: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if got := updateSummary("0.4.0", 1); !strings.Contains(got, "COORDINATED") {
		t.Errorf("banner line = %q, want the boundary called out", got)
	}
}

// Opt-out is checked before anything reads a cache or schedules a call.
func TestOptOutSilencesEverything(t *testing.T) {
	t.Setenv("TENDRILS_HOME", t.TempDir())
	path, err := updateCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := selfupdate.SaveCache(path, selfupdate.Cache{
		Status: selfupdate.StatusOK, Latest: "9.9.9", Tag: "v9.9.9",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TENDRILS_NO_UPDATE_CHECK", "1")

	if got := updateSummary("0.1.0", 1); got != "" {
		t.Errorf("opted out, but the daemon banner still said %q", got)
	}
	cmd := &cobra.Command{Use: "status"}
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	cmd.SetOut(&bytes.Buffer{})
	maybeUpdateNotice(cmd)
	if errBuf.Len() != 0 {
		t.Errorf("opted out, but a notice was printed: %q", errBuf.String())
	}
}

// The notice goes to stderr: `tendrils status | grep` must see exactly the
// status, and a device telling its owner it is stale must not be able to
// corrupt anything parsing stdout.
func TestNoticeGoesToStderr(t *testing.T) {
	t.Setenv("TENDRILS_HOME", t.TempDir())
	path, err := updateCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := selfupdate.SaveCache(path, selfupdate.Cache{
		Status: selfupdate.StatusOK, Latest: "9.9.9", Tag: "v9.9.9",
		NextCheck: fakeFuture(),
	}); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "status"}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	maybeUpdateNotice(cmd)

	if out.Len() != 0 {
		t.Errorf("the notice must not touch stdout: %q", out)
	}
	if !strings.Contains(errOut.String(), "9.9.9") {
		t.Errorf("stderr = %q, want the notice", errOut)
	}
}

func fakeFuture() (t time.Time) {
	return time.Now().Add(selfupdate.CheckInterval)
}

// The background check re-executes this binary. Under `go test` that binary is
// the test suite, and launching it with an unrecognised argument would run
// every test again — including this one, which would launch another. The guard
// is what stands between a background update check and a fork bomb, and it is
// invisible in production, so it is asserted here.
func TestBackgroundCheckNeverReExecutesATestBinary(t *testing.T) {
	if exe, ok := reExecutable(); ok {
		t.Errorf("a test binary must never be re-executed as a checker, got %q", exe)
	}
}

// A due check writes its lease before anything is launched. Without it, a
// script running `tendrils status` in a loop would launch a checker on every
// iteration — a check that dies without reporting back must still hold the
// next one off.
func TestDueCheckClaimsTheLeaseBeforeLaunching(t *testing.T) {
	t.Setenv("TENDRILS_HOME", t.TempDir())
	path, err := updateCachePath()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "status"}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	before := time.Now()
	maybeUpdateNotice(cmd) // no cache at all: the check is due
	cache, found := selfupdate.LoadCache(path)
	if !found {
		t.Fatal("a due check must record its claim")
	}
	if cache.Due(before.Add(time.Minute)) {
		t.Error("the claim must hold off the next check")
	}
	if cache.Available("0.1.0") || cache.Notice("0.1.0", 1) != "" {
		t.Error("a claim is not a result and must announce nothing")
	}
}

// What an upgrade will touch, and in what order. The blob server is replaced
// only where it already sits beside tendrils — installing one on a device whose
// owner never asked for it is not an upgrade — and it is replaced *first*,
// because docs/UPGRADING.md moves the Blossom host before the sync client.
func TestReplacementsFollowTheUpgradeOrdering(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, selfupdate.BinaryName("tendrils", runtime.GOOS))
	if err := os.WriteFile(exe, []byte("tendrils"), 0o755); err != nil {
		t.Fatal(err)
	}

	reps := replacementsFor(exe)
	if len(reps) != 1 || reps[0].Program != "tendrils" {
		t.Fatalf("without a blob server installed: %+v", reps)
	}

	blossom := filepath.Join(dir, selfupdate.BinaryName("blossomd", runtime.GOOS))
	if err := os.WriteFile(blossom, []byte("blossomd"), 0o755); err != nil {
		t.Fatal(err)
	}
	reps = replacementsFor(exe)
	if len(reps) != 2 {
		t.Fatalf("with a blob server installed: %+v", reps)
	}
	if reps[0].Program != "blossomd" || reps[1].Program != "tendrils" {
		t.Errorf("order = %s then %s, want blossomd first", reps[0].Program, reps[1].Program)
	}
	if reps[0].Path != blossom {
		t.Errorf("blossomd path = %q, want %q", reps[0].Path, blossom)
	}
}

// The daemon is never restarted by the upgrade; the command says what to run.
func TestRestartHintNamesTheBlobServerToo(t *testing.T) {
	t.Setenv("TENDRILS_HOME", t.TempDir()) // no daemon address file: none running
	var out bytes.Buffer
	printRestartHint(&out, []string{"/opt/bin/blossomd", "/opt/bin/tendrils"})
	got := out.String()
	if !strings.Contains(got, "blossomd") {
		t.Errorf("a replaced blob server must be mentioned: %q", got)
	}

	out.Reset()
	printRestartHint(&out, []string{"/opt/bin/tendrils"})
	if strings.Contains(out.String(), "blossomd") {
		t.Errorf("nothing was said about blossomd, and nothing should be: %q", out.String())
	}
}
