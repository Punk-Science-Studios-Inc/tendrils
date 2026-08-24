package selfupdate

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ArchiveName mirrors .goreleaser.yaml's name_template by hand: nothing at
// build time reconciles the two. CI's release-dry-run runs
// `build --snapshot --single-target`, which produces binaries but no archives,
// so it never renders that template either.
//
// A mismatch is invisible until a real release exists and then breaks every
// device at once: the updater asks for a file name the release does not carry,
// gets a 404, and reports ErrNoAsset forever. Every other test in this package
// calls ArchiveName on both sides of its assertion, so all of them would keep
// passing while upgrades were impossible.
//
// These tests read the actual config and fail if the two ever drift apart.

func goreleaserConfig(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %s", err)
	}
	return string(raw)
}

func TestArchiveNameMatchesTheGoreleaserTemplate(t *testing.T) {
	cfg := goreleaserConfig(t)

	m := regexp.MustCompile(`name_template:\s*"([^"]+)"`).FindStringSubmatch(cfg)
	if m == nil {
		t.Fatal("no archives name_template found in .goreleaser.yaml")
	}
	template := m[1]

	// Render the template the way GoReleaser would. .Version is the tag with
	// its leading "v" stripped, which is what ArchiveName's TrimPrefix mirrors.
	for _, tc := range []struct{ version, goos, goarch string }{
		{"0.1.0", "linux", "amd64"},
		{"0.1.0", "linux", "arm64"},
		{"0.1.0", "darwin", "arm64"},
		{"1.2.3", "darwin", "amd64"},
		{"0.0.1-next", "linux", "amd64"},
	} {
		rendered := strings.NewReplacer(
			"{{ .Version }}", tc.version,
			"{{ .Os }}", tc.goos,
			"{{ .Arch }}", tc.goarch,
		).Replace(template) + ".tar.gz"

		if got := ArchiveName(tc.version, tc.goos, tc.goarch); got != rendered {
			t.Errorf("ArchiveName(%q, %q, %q) = %q, but .goreleaser.yaml produces %q",
				tc.version, tc.goos, tc.goarch, got, rendered)
		}
	}

	// The tag form must resolve identically: a release is tagged v0.1.0 while
	// the asset is named 0.1.0.
	if tagged, bare := ArchiveName("v0.1.0", "linux", "amd64"), ArchiveName("0.1.0", "linux", "amd64"); tagged != bare {
		t.Errorf("ArchiveName is not v-prefix invariant: %q vs %q", tagged, bare)
	}
}

func TestWindowsArchivesAreZipInBothPlaces(t *testing.T) {
	cfg := goreleaserConfig(t)

	// format_overrides must still put windows on zip; ArchiveName assumes it.
	idx := strings.Index(cfg, "format_overrides:")
	if idx < 0 {
		t.Fatal("no format_overrides in .goreleaser.yaml: ArchiveName expects windows to be zip")
	}
	override := cfg[idx:min(idx+200, len(cfg))]
	if !strings.Contains(override, "goos: windows") || !strings.Contains(override, "zip") {
		t.Errorf("windows/zip override missing or changed; ArchiveName still expects zip:\n%s", override)
	}

	if got := ArchiveName("0.1.0", "windows", "amd64"); got != "tendrils_0.1.0_windows_amd64.zip" {
		t.Errorf("ArchiveName for windows = %q, want a .zip", got)
	}
}

// TestArchiveNameMatchesRealBuiltArtifacts closes the loop against archives an
// actual `goreleaser release --snapshot` produced, when they are present. It
// skips rather than fails when dist/ is absent so the ordinary `go test ./...`
// needs no toolchain, but it is the check that would have caught a drift the
// string comparison above could not.
func TestArchiveNameMatchesRealBuiltArtifacts(t *testing.T) {
	entries, err := os.ReadDir("../../dist")
	if err != nil {
		t.Skip("no dist/: run `goreleaser release --snapshot --clean` to exercise this")
	}

	found := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "tendrils_") {
			continue
		}
		if !strings.HasSuffix(name, ".tar.gz") && !strings.HasSuffix(name, ".zip") {
			continue
		}
		// tendrils_<version>_<os>_<arch>.<ext>
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".tar.gz"), ".zip")
		parts := strings.Split(base, "_")
		if len(parts) != 4 {
			t.Errorf("archive %q does not split into tendrils_<version>_<os>_<arch>", name)
			continue
		}
		version, goos, goarch := parts[1], parts[2], parts[3]
		if got := ArchiveName(version, goos, goarch); got != name {
			t.Errorf("built artifact is %q but ArchiveName produces %q", name, got)
		}
		found++
	}
	if found == 0 {
		t.Skip("dist/ holds no tendrils archives")
	}
	t.Logf("checked %d built archives against ArchiveName", found)
}
