package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// downloadInto runs a download into a fresh destination directory and returns
// both the result and the directory, so every test can assert the same thing
// about failure: the destination is exactly as it was.
func downloadInto(t *testing.T, gh *fakeGitHub, tag, goos, goarch string, programs ...string) (*Downloaded, string, error) {
	t.Helper()
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "tendrils"), []byte("the installed binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := gh.client().ReleaseByTag(context.Background(), tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(programs) == 0 {
		programs = []string{"tendrils", "blossomd"}
	}
	dl, err := gh.client().Download(context.Background(), rel, goos, goarch, dest, programs)
	return dl, dest, err
}

// assertUntouched is the property every failure path shares: an upgrade that
// did not happen costs bandwidth and nothing else. No staging directory left
// behind, and the installed binary is byte-for-byte what it was.
func assertUntouched(t *testing.T, dest string) {
	t.Helper()
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "tendrils" {
			t.Errorf("failed download left %s behind in the destination", e.Name())
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "tendrils"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the installed binary" {
		t.Errorf("installed binary changed: %q", got)
	}
}

func TestDownloadVerifiesAndUnpacksBothBinaries(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			gh := newFakeGitHub(t, release(t, "v0.4.0", goos, "amd64", map[string]string{
				"tendrils": "new tendrils", "blossomd": "new blossomd",
			}, nil))
			dl, dest, err := downloadInto(t, gh, "v0.4.0", goos, "amd64")
			if err != nil {
				t.Fatal(err)
			}
			defer dl.Cleanup()

			for program, want := range map[string]string{"tendrils": "new tendrils", "blossomd": "new blossomd"} {
				path, ok := dl.Binaries[program]
				if !ok {
					t.Fatalf("%s was not unpacked", program)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != want {
					t.Errorf("%s = %q, want %q", program, got, want)
				}
				if !strings.HasPrefix(path, dest) {
					t.Errorf("%s was staged outside the destination: %s", program, path)
				}
			}
			// Staging happens inside the destination so the final move is a
			// rename on one filesystem, which is the entire atomicity guarantee.
			if filepath.Dir(dl.Dir) != dest {
				t.Errorf("staging dir %s is not inside %s", dl.Dir, dest)
			}
			if err := dl.Cleanup(); err != nil {
				t.Fatal(err)
			}
			assertUntouched(t, dest)
		})
	}
}

// The defect this whole package is built around: bytes that arrive are not
// bytes that are correct. A tampered or corrupted archive whose checksum does
// not match the release must never reach the destination.
func TestDownloadRefusesAChecksumMismatch(t *testing.T) {
	rel := release(t, "v0.4.0", "linux", "amd64", map[string]string{"tendrils": "good"}, nil)
	name := ArchiveName("0.4.0", "linux", "amd64")
	rel.assets[name] = append(rel.assets[name], "tampered"...) // checksums.txt still lists the original
	gh := newFakeGitHub(t, rel)

	_, dest, err := downloadInto(t, gh, "v0.4.0", "linux", "amd64", "tendrils")
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	assertUntouched(t, dest)
}

// An archive nobody signed for. install.sh refuses this and so does the
// updater: an unverifiable binary is worse than a stale one, and "the entry is
// missing" is the shape a substitution attack takes.
func TestDownloadRefusesAMissingChecksumEntry(t *testing.T) {
	rel := release(t, "v0.4.0", "linux", "amd64", map[string]string{"tendrils": "good"}, nil)
	rel.assets[ChecksumsAsset] = []byte("0000000000000000000000000000000000000000000000000000000000000000  something_else.tar.gz\n")
	gh := newFakeGitHub(t, rel)

	_, dest, err := downloadInto(t, gh, "v0.4.0", "linux", "amd64", "tendrils")
	if !errors.Is(err, ErrChecksumMissing) {
		t.Fatalf("err = %v, want ErrChecksumMissing", err)
	}
	assertUntouched(t, dest)
}

func TestDownloadRefusesAReleaseWithNoChecksumsAsset(t *testing.T) {
	rel := release(t, "v0.4.0", "linux", "amd64", map[string]string{"tendrils": "good"}, nil)
	delete(rel.assets, ChecksumsAsset)
	gh := newFakeGitHub(t, rel)

	_, dest, err := downloadInto(t, gh, "v0.4.0", "linux", "amd64", "tendrils")
	if err == nil || !strings.Contains(err.Error(), ChecksumsAsset) {
		t.Fatalf("err = %v, want a refusal naming %s", err, ChecksumsAsset)
	}
	assertUntouched(t, dest)
}

// A dropped connection halfway through. The failure has to be named as a short
// transfer rather than reported as a checksum mismatch, which reads like
// tampering and sends whoever is debugging it in the wrong direction.
func TestDownloadRefusesATruncatedTransfer(t *testing.T) {
	rel := release(t, "v0.4.0", "linux", "amd64", map[string]string{"tendrils": strings.Repeat("x", 4096)}, nil)
	rel.truncate = map[string]bool{ArchiveName("0.4.0", "linux", "amd64"): true}
	gh := newFakeGitHub(t, rel)

	_, dest, err := downloadInto(t, gh, "v0.4.0", "linux", "amd64", "tendrils")
	if err == nil {
		t.Fatal("a truncated download must not be accepted")
	}
	assertUntouched(t, dest)
}

// The same failure when the transfer ends cleanly but short of the size the
// release declared.
func TestDownloadRefusesAShortTransferAgainstTheDeclaredSize(t *testing.T) {
	rel := release(t, "v0.4.0", "linux", "amd64", map[string]string{"tendrils": "good"}, nil)
	name := ArchiveName("0.4.0", "linux", "amd64")
	rel.declaredSize = map[string]int64{name: int64(len(rel.assets[name]) + 1024)}
	gh := newFakeGitHub(t, rel)

	_, dest, err := downloadInto(t, gh, "v0.4.0", "linux", "amd64", "tendrils")
	if err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Fatalf("err = %v, want a short-transfer refusal", err)
	}
	assertUntouched(t, dest)
}

// A release that simply has no build for this machine. Common in practice — a
// platform added or dropped between releases — and it must be a clear refusal,
// not a download of something for another architecture.
func TestDownloadRefusesWhenThePlatformHasNoArchive(t *testing.T) {
	gh := newFakeGitHub(t, release(t, "v0.4.0", "linux", "amd64", map[string]string{"tendrils": "x"}, nil))
	_, dest, err := downloadInto(t, gh, "v0.4.0", "linux", "riscv64", "tendrils")
	if !errors.Is(err, ErrNoAsset) {
		t.Fatalf("err = %v, want ErrNoAsset", err)
	}
	assertUntouched(t, dest)
}

// The archive holds tendrils but not blossomd. Not fatal at this layer: what
// was found is reported, and the caller decides. A host that runs blossomd
// keeps the one it has rather than losing the upgrade over it.
func TestDownloadReportsWhichBinariesTheArchiveHeld(t *testing.T) {
	gh := newFakeGitHub(t, release(t, "v0.4.0", "linux", "amd64", map[string]string{"tendrils": "x"}, nil))
	dl, _, err := downloadInto(t, gh, "v0.4.0", "linux", "amd64", "tendrils", "blossomd")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Cleanup()
	if _, ok := dl.Binaries["tendrils"]; !ok {
		t.Error("tendrils should have been unpacked")
	}
	if _, ok := dl.Binaries["blossomd"]; ok {
		t.Error("blossomd is not in this archive and must not be reported as unpacked")
	}
}

// An archive entry named ../../something must not be able to write outside the
// staging directory. Entries are matched on base name and written to the
// staging directory by construction, so this asserts the property rather than
// the check.
func TestDownloadCannotWriteOutsideTheStagingDirectory(t *testing.T) {
	files := map[string][]byte{
		"../../tendrils":                     []byte("escaped"),
		BinaryName("tendrils", runtime.GOOS): []byte("legitimate"),
	}
	archive := tarGzArchive(t, files)
	name := ArchiveName("0.4.0", "linux", "amd64")
	rel := &fakeRelease{tag: "v0.4.0", assets: map[string][]byte{
		name:           archive,
		ChecksumsAsset: checksums(map[string][]byte{name: archive}),
	}}
	gh := newFakeGitHub(t, rel)

	dest := t.TempDir()
	sentinel := filepath.Join(filepath.Dir(dest), "tendrils")
	r, err := gh.client().ReleaseByTag(context.Background(), "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	dl, err := gh.client().Download(context.Background(), r, "linux", "amd64", dest, []string{"tendrils"})
	if err != nil {
		// Rejecting the archive outright is also correct.
		if !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("unexpected error: %v", err)
		}
	} else {
		defer dl.Cleanup()
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("archive entry escaped the staging directory and wrote %s", sentinel)
	}
}

func TestChecksumForToleratesTheBinaryModeMarker(t *testing.T) {
	body := []byte("aa\n" + strings.Repeat("b", 64) + " *tendrils_0.4.0_linux_amd64.tar.gz\n")
	got, err := checksumFor(body, "tendrils_0.4.0_linux_amd64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.Repeat("b", 64) {
		t.Errorf("sum = %q", got)
	}
}

func TestChecksumForRejectsAMalformedHash(t *testing.T) {
	body := []byte("notahash  tendrils_0.4.0_linux_amd64.tar.gz\n")
	if _, err := checksumFor(body, "tendrils_0.4.0_linux_amd64.tar.gz"); err == nil {
		t.Error("a malformed hash must be a refusal, not a comparison that happens to fail")
	}
}
