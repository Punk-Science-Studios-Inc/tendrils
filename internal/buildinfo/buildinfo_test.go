package buildinfo

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolvePrefersLdflagsStamp(t *testing.T) {
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v9.9.9"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "deadbeefdeadbeef"},
			{Key: "vcs.time", Value: "2020-01-01T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	got := resolve("1.2.3", "a81f7ab0123456", "2026-08-19T10:00:00Z", bi)
	if got.Version != "1.2.3" || got.Commit != "a81f7ab0123456" || got.Date != "2026-08-19T10:00:00Z" {
		t.Fatalf("stamp did not win: %+v", got)
	}
	if !got.Dirty {
		t.Error("vcs.modified=true should still mark the build dirty")
	}
}

func TestResolveFallsBackToBuildInfo(t *testing.T) {
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.4.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "a81f7ab0123456"},
			{Key: "vcs.time", Value: "2026-08-19T10:00:00Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	}
	got := resolve("", "", "", bi)
	// go install path@v0.4.0: the module version is the release, minus the v.
	if got.Version != "0.4.0" {
		t.Errorf("Version = %q, want 0.4.0", got.Version)
	}
	if got.Commit != "a81f7ab0123456" || got.Date != "2026-08-19T10:00:00Z" {
		t.Errorf("VCS data not adopted: %+v", got)
	}
	if got.Dirty {
		t.Error("vcs.modified=false must not mark the build dirty")
	}
}

func TestResolveLocalBuildIsDev(t *testing.T) {
	bi := &debug.BuildInfo{
		Main:     debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "a81f7ab0123456"}},
	}
	got := resolve("", "", "", bi)
	if got.Version != "dev" {
		t.Errorf("Version = %q, want dev — (devel) carries no release information", got.Version)
	}
	if got.String() != "dev (a81f7ab)" {
		t.Errorf("String = %q, want %q", got.String(), "dev (a81f7ab)")
	}
}

func TestResolveNoBuildInfo(t *testing.T) {
	got := resolve("", "", "", nil)
	if got.Version != "dev" || got.String() != "dev" {
		t.Errorf("got %+v / %q, want a bare dev", got, got.String())
	}
}

func TestStringMarksDirty(t *testing.T) {
	i := Info{Version: "1.2.3", Commit: "a81f7ab0123456", Dirty: true}
	if want := "1.2.3 (a81f7ab, dirty)"; i.String() != want {
		t.Errorf("String = %q, want %q", i.String(), want)
	}
}

func TestDetailIncludesEverything(t *testing.T) {
	i := Info{Version: "1.2.3", Commit: "a81f7ab0123456", Date: "2026-08-19T10:00:00Z"}
	got := i.Detail("tendrils")
	for _, want := range []string{"tendrils 1.2.3", "a81f7ab0123456", "2026-08-19T10:00:00Z", "go1."} {
		if !strings.Contains(got, want) {
			t.Errorf("Detail missing %q:\n%s", want, got)
		}
	}
}

func TestGetIsUsable(t *testing.T) {
	// Whatever the toolchain embedded, a version is always reported.
	if Get().Version == "" {
		t.Error("Get().Version must never be empty")
	}
}

func TestResolveIgnoresPseudoVersion(t *testing.T) {
	// A plain `go build` in a git checkout synthesizes a pseudo-version. It is
	// not a release, so it must not be shown as one.
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.0.0-20260728020249-a81f7ab00a90+dirty"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "a81f7ab00a908b502b8a977041eaaa4cd4800748"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	got := resolve("", "", "", bi)
	if got.Version != "dev" {
		t.Errorf("Version = %q, want dev", got.Version)
	}
	if want := "dev (a81f7ab, dirty)"; got.String() != want {
		t.Errorf("String = %q, want %q", got.String(), want)
	}
}
