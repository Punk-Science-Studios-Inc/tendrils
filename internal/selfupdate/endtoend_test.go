package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The whole path in one test, on whatever platform it is running: a published
// release, an archive with real executables in it, checksums.txt, the download,
// the verification, running the new binary to confirm what it is, and the
// swap. Each half is covered on its own above; this asserts they compose, and
// that the archive format is the one GoReleaser actually publishes for this
// platform (tar.gz everywhere, zip on Windows).
func TestDownloadThenApplyEndToEnd(t *testing.T) {
	// The test binary is a genuine executable for this platform, and TestMain
	// makes it answer `version` like a real one.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}

	goos, goarch := runtime.GOOS, runtime.GOARCH
	files := map[string][]byte{
		BinaryName("tendrils", goos): binary,
		BinaryName("blossomd", goos): binary,
	}
	var archive []byte
	if goos == "windows" {
		archive = zipArchive(t, files)
	} else {
		archive = tarGzArchive(t, files)
	}
	name := ArchiveName("0.4.0", goos, goarch)
	gh := newFakeGitHub(t, &fakeRelease{tag: "v0.4.0", assets: map[string][]byte{
		name:           archive,
		ChecksumsAsset: checksums(map[string][]byte{name: archive}),
	}})

	// A destination that looks like a real install: both binaries side by side.
	dest := t.TempDir()
	installedTendrils := writeFile(t, filepath.Join(dest, BinaryName("tendrils", goos)), "old tendrils", 0o755)
	installedBlossom := writeFile(t, filepath.Join(dest, BinaryName("blossomd", goos)), "old blossomd", 0o755)

	rel, err := gh.client().ReleaseByTag(context.Background(), "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	dl, err := gh.client().Download(context.Background(), rel, goos, goarch, dest, []string{"tendrils", "blossomd"})
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Cleanup()

	t.Setenv(fakeVersionEnv, "0.4.0")
	// blossomd first: docs/UPGRADING.md upgrades the Blossom host before the
	// sync client, and the updater must not invert that on a host running both.
	replaced, err := Applier{}.Apply(context.Background(), "0.4.0", []Replacement{
		{Program: "blossomd", Path: installedBlossom, New: dl.Binaries["blossomd"]},
		{Program: "tendrils", Path: installedTendrils, New: dl.Binaries["tendrils"]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) != 2 || replaced[0] != installedBlossom {
		t.Fatalf("replaced = %v, want blossomd first then tendrils", replaced)
	}
	for _, p := range replaced {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != int64(len(binary)) {
			t.Errorf("%s is %d bytes, want the %d-byte binary from the archive", p, fi.Size(), len(binary))
		}
	}
	if err := dl.Cleanup(); err != nil {
		t.Fatal(err)
	}
	// Nothing left over in the install directory but the two binaries.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("install directory holds %v, want just the two binaries", names)
	}
}

// The same release, with one bit flipped in the archive. Nothing is downloaded
// into place, nothing is executed, and both installed binaries are exactly what
// they were: the property acceptance criterion 2 asks for.
func TestCorruptArchiveLeavesTheInstallUntouched(t *testing.T) {
	goos, goarch := runtime.GOOS, runtime.GOARCH
	files := map[string][]byte{BinaryName("tendrils", goos): []byte("pretend binary")}
	var archive []byte
	if goos == "windows" {
		archive = zipArchive(t, files)
	} else {
		archive = tarGzArchive(t, files)
	}
	name := ArchiveName("0.4.0", goos, goarch)
	sums := checksums(map[string][]byte{name: archive})
	archive[len(archive)-1] ^= 0xff // one flipped bit, after the checksums were computed
	gh := newFakeGitHub(t, &fakeRelease{tag: "v0.4.0", assets: map[string][]byte{
		name: archive, ChecksumsAsset: sums,
	}})

	dest := t.TempDir()
	installed := writeFile(t, filepath.Join(dest, BinaryName("tendrils", goos)), "old tendrils", 0o755)

	rel, err := gh.client().ReleaseByTag(context.Background(), "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gh.client().Download(context.Background(), rel, goos, goarch, dest, []string{"tendrils"}); err == nil {
		t.Fatal("a corrupted archive must be refused")
	}
	if got := readFile(t, installed); got != "old tendrils" {
		t.Errorf("installed binary = %q, want it untouched", got)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 1 {
		t.Errorf("the refused download left something behind: %d entries", len(entries))
	}
}
