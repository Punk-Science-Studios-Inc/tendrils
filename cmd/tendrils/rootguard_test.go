package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/tree"
)

// runCLI runs one tendrils command against an isolated state directory.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("TENDRILS_NO_UPDATE_CHECK", "1")
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// legacyEnrollment writes the state an older build leaves behind: a key, a
// config with no root identity, and an index that remembers synced files.
func legacyEnrollment(t *testing.T, root string, synced ...string) {
	t.Helper()
	t.Setenv("TENDRILS_HOME", t.TempDir())
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveKey(id); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(config.Config{SyncRoot: root, Relays: []string{"ws://127.0.0.1:1"}}); err != nil {
		t.Fatal(err)
	}
	path, err := config.IndexPath()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	for _, p := range synced {
		if err := idx.Put(&tree.Entry{Path: p, Sha256: "00", Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
}

func touch(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonRefusesUnadoptedRoot(t *testing.T) {
	root := t.TempDir()
	legacyEnrollment(t, root, "a.md")
	_, err := runCLI(t, "daemon")
	if err == nil || !strings.Contains(err.Error(), "tendrils adopt") {
		t.Fatalf("daemon on an unadopted root: %v, want a pointer to adopt", err)
	}
}

func TestAdoptRefusesEmptyMountpoint(t *testing.T) {
	root := t.TempDir()
	legacyEnrollment(t, root, "a.md", "b.md")

	_, err := runCLI(t, "adopt")
	if err == nil || !strings.Contains(err.Error(), "none of the 2 files") {
		t.Fatalf("adopt of an empty folder: %v, want a refusal", err)
	}
	if _, has, _ := rootid.HasMarker(root); has {
		t.Fatal("a refused adoption still marked the folder")
	}
	if cfg, _, _ := config.Load(); !cfg.Root.IsZero() {
		t.Fatal("a refused adoption still recorded an identity")
	}
}

func TestAdoptRecordsAVerifiedRoot(t *testing.T) {
	root := t.TempDir()
	legacyEnrollment(t, root, "a.md", "b.md")
	touch(t, root, "a.md")
	touch(t, root, "b.md")

	out, err := runCLI(t, "adopt")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !strings.Contains(out, "Found 2 of the 2") {
		t.Errorf("adopt output did not report the survey:\n%s", out)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := rootid.Verify(root, cfg.Root); err != nil {
		t.Fatalf("adopted root does not verify: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.md")); err != nil {
		t.Fatal("adoption touched existing files")
	}
}

func TestAdoptForceAcceptsAnEmptyFolder(t *testing.T) {
	root := t.TempDir()
	legacyEnrollment(t, root, "a.md")
	if _, err := runCLI(t, "adopt", "--force"); err != nil {
		t.Fatalf("adopt --force: %v", err)
	}
}

// Re-enrolling a device with history onto an unmarked folder must not quietly
// mark it: if it is an empty mountpoint, the next pass deletes everything.
func TestEnrollRefusesUnmarkedFolderWhenDeviceHasHistory(t *testing.T) {
	root := t.TempDir()
	legacyEnrollment(t, root, "a.md")
	_, err := runCLI(t, "enroll", "--root", root)
	if err == nil || !strings.Contains(err.Error(), "tendrils adopt") {
		t.Fatalf("enroll: %v, want a pointer to adopt", err)
	}
}

func TestFreshEnrollMarksRoot(t *testing.T) {
	t.Setenv("TENDRILS_HOME", t.TempDir())
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "vault")
	if _, err := runCLI(t, "enroll", "--key", id.SecretHex(), "--root", root); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := rootid.Verify(root, cfg.Root); err != nil {
		t.Fatalf("enrolled root does not verify: %v", err)
	}
}

// Daemonless status must not scan an unverified root: an empty mountpoint would
// otherwise report every file as a pending deletion.
func TestStatusReportsPausedRoot(t *testing.T) {
	root := t.TempDir()
	legacyEnrollment(t, root, "a.md")
	touch(t, root, "a.md")
	if _, err := runCLI(t, "adopt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, rootid.MarkerName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "a.md")); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "Paused:") {
		t.Errorf("status did not report the pause:\n%s", out)
	}
	if !strings.Contains(out, "Pending changes: 0") {
		t.Errorf("status counted an unverified folder's missing files as pending:\n%s", out)
	}
}

// A synced path behind a folder that is now a link out of the root is skipped
// by the survey, not an error that stops adoption even with --force.
func TestAdoptSkipsPathsBehindAnEscapingLink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	touch(t, outside, "b.md")
	touch(t, root, "a.md")
	if err := os.Symlink(outside, filepath.Join(root, "notes")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	legacyEnrollment(t, root, "a.md", "notes/b.md")
	if _, err := runCLI(t, "adopt", "--force"); err != nil {
		t.Fatalf("adopt --force: %v", err)
	}
}
