package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The apply path is the dangerous half of this feature and the one this project
// has already been bitten by: "a build over a running executable" shipped once
// as a Windows-only defect, invisible on Linux, which is why Windows is in the
// CI matrix at all. So these tests are written to run identically on both, and
// the ones that matter use *real* executables and a *real* running process
// rather than a mock that cannot reproduce a file lock.
//
// The trick is that the test binary is itself a working executable: copying it
// gives a file that runs on whatever platform the test is running on, and the
// TestMain below makes that copy behave like `tendrils version` when asked.

const (
	fakeVersionEnv = "TENDRILS_SELFUPDATE_TEST_VERSION"
	fakeSleepEnv   = "TENDRILS_SELFUPDATE_TEST_SLEEP"
)

func TestMain(m *testing.M) {
	// Re-executed as a stand-in binary rather than as a test.
	if d := os.Getenv(fakeSleepEnv); d != "" {
		dur, err := time.ParseDuration(d)
		if err != nil {
			os.Exit(2)
		}
		time.Sleep(dur)
		os.Exit(0)
	}
	if v := os.Getenv(fakeVersionEnv); v != "" {
		// The same shape buildinfo.Info.Detail prints.
		fmt.Printf("%s %s\n  commit: 0123456789abcdef\n", filepath.Base(os.Args[0]), v)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// copyExecutable copies the running test binary to dir/name — a genuine
// executable for this platform, which is what makes the exec-verification and
// running-binary tests real on Windows as well as POSIX.
func copyExecutable(t *testing.T, dir, program string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dest := filepath.Join(dir, BinaryName(program, runtime.GOOS))
	dst, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return dest
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// alwaysValid stands in for verification where the test is about the move, not
// about the check. Every test that is about the check uses the real one.
func alwaysValid(context.Context, string, string, string) error { return nil }

func TestApplyReplacesTheInstalledBinary(t *testing.T) {
	for _, policy := range []struct {
		name   string
		policy AsidePolicy
	}{
		{"direct rename (POSIX)", AsideNever},
		{"rename aside (Windows)", AsideAlways},
	} {
		t.Run(policy.name, func(t *testing.T) {
			dir := t.TempDir()
			installed := writeFile(t, filepath.Join(dir, "tendrils"), "old", 0o755)
			staged := writeFile(t, filepath.Join(dir, "staged-tendrils"), "new", 0o600)

			a := Applier{Verify: alwaysValid, Aside: policy.policy}
			replaced, err := a.Apply(context.Background(), "0.4.0", []Replacement{
				{Program: "tendrils", Path: installed, New: staged},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(replaced) != 1 || replaced[0] != installed {
				t.Fatalf("replaced = %v, want [%s]", replaced, installed)
			}
			if got := readFile(t, installed); got != "new" {
				t.Errorf("installed content = %q, want %q", got, "new")
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Error("the staged file should have been moved, not copied")
			}
			// The mode of the binary being replaced is kept: an owner who chose
			// a private 0700 install keeps it, and a 0755 install stays runnable
			// by everyone who could run the old one.
			if runtime.GOOS != "windows" {
				fi, err := os.Stat(installed)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode().Perm() != 0o755 {
					t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
				}
			}
		})
	}
}

// Windows cannot overwrite a running image but can rename it, so the previous
// binary is moved aside and swept up later. The sweep has to be safe to run at
// any time and to find the file whatever it was named.
func TestRenameAsideLeavesTheOldBinaryForLaterCleanup(t *testing.T) {
	dir := t.TempDir()
	installed := writeFile(t, filepath.Join(dir, "tendrils"), "old", 0o755)
	staged := writeFile(t, filepath.Join(dir, "staged"), "new", 0o600)

	// A leftover from an even earlier upgrade that could not be removed then.
	stale := writeFile(t, installed+".old.1", "ancient", 0o755)

	a := Applier{Verify: alwaysValid, Aside: AsideAlways}
	if _, err := a.Apply(context.Background(), "0.4.0", []Replacement{
		{Program: "tendrils", Path: installed, New: staged},
	}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, installed); got != "new" {
		t.Fatalf("installed = %q", got)
	}
	// Apply removes its own aside when it can, so only the stale one is left.
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the earlier leftover should still be there for the sweep: %v", err)
	}
	if n := CleanupOld(installed); n < 1 {
		t.Errorf("CleanupOld removed %d, want at least the stale leftover", n)
	}
	matches, _ := filepath.Glob(installed + ".old*")
	if len(matches) != 0 {
		t.Errorf("leftovers remain after cleanup: %v", matches)
	}
	if got := readFile(t, installed); got != "new" {
		t.Errorf("cleanup damaged the installed binary: %q", got)
	}
}

// Acceptance criterion 3: an interrupted or failed upgrade always leaves a
// working binary in place. The failure is injected after verification, at the
// moment the new binary is moved in — the one window where the installed
// binary has already been moved aside.
func TestApplyRestoresTheOriginalWhenTheMoveFails(t *testing.T) {
	dir := t.TempDir()
	installed := writeFile(t, filepath.Join(dir, "tendrils"), "old", 0o755)
	staged := writeFile(t, filepath.Join(dir, "staged"), "new", 0o600)

	// Fail the move of the new binary into place — the one window where the
	// installed binary has already been moved aside — and let the rollback that
	// follows succeed.
	moves := 0
	a := Applier{
		Verify: alwaysValid,
		Aside:  AsideAlways,
		rename: func(oldpath, newpath string) error {
			moves++
			if moves == 2 {
				return errors.New("simulated failure installing the new binary")
			}
			return os.Rename(oldpath, newpath)
		},
	}
	replaced, err := a.Apply(context.Background(), "0.4.0", []Replacement{
		{Program: "tendrils", Path: installed, New: staged},
	})
	if moves < 3 {
		t.Fatalf("only %d moves: the rollback was never attempted", moves)
	}
	if err == nil {
		t.Fatal("the move could not have succeeded")
	}
	if len(replaced) != 0 {
		t.Errorf("replaced = %v, want nothing", replaced)
	}
	if got := readFile(t, installed); got != "old" {
		t.Fatalf("the working binary was lost: %q", got)
	}
	if matches, _ := filepath.Glob(installed + ".old*"); len(matches) != 0 {
		t.Errorf("rollback left the binary aside instead of putting it back: %v", matches)
	}
	if got := readFile(t, staged); got != "new" {
		t.Errorf("the staged binary should be untouched for a retry: %q", got)
	}
}

// The staged binary disappears between verification and the move — a cleaner, a
// crash, an antivirus quarantine. Nothing has been moved yet at that point, so
// the installed binary is never even disturbed.
func TestApplyStopsWhenTheStagedBinaryVanishes(t *testing.T) {
	dir := t.TempDir()
	installed := writeFile(t, filepath.Join(dir, "tendrils"), "old", 0o755)
	staged := writeFile(t, filepath.Join(dir, "staged"), "new", 0o600)

	a := Applier{
		Aside: AsideAlways,
		Verify: func(_ context.Context, _, path, _ string) error {
			return os.Remove(path)
		},
	}
	replaced, err := a.Apply(context.Background(), "0.4.0", []Replacement{
		{Program: "tendrils", Path: installed, New: staged},
	})
	if err == nil || len(replaced) != 0 {
		t.Fatalf("replaced = %v, err = %v; want nothing replaced and an error", replaced, err)
	}
	if got := readFile(t, installed); got != "old" {
		t.Fatalf("the working binary was lost: %q", got)
	}
	if matches, _ := filepath.Glob(installed + ".old*"); len(matches) != 0 {
		t.Errorf("the installed binary should not have been moved at all: %v", matches)
	}
}

// Two binaries, one archive: verification happens for both before either is
// moved. A host left with a new tendrils and an old blossomd is worse than one
// left entirely alone, and docs/UPGRADING.md is explicit that the Blossom host
// moves first.
func TestApplyVerifiesEverythingBeforeMovingAnything(t *testing.T) {
	dir := t.TempDir()
	blossom := writeFile(t, filepath.Join(dir, "blossomd"), "old blossomd", 0o755)
	tendrils := writeFile(t, filepath.Join(dir, "tendrils"), "old tendrils", 0o755)
	stagedB := writeFile(t, filepath.Join(dir, "staged-blossomd"), "new blossomd", 0o600)
	stagedT := writeFile(t, filepath.Join(dir, "staged-tendrils"), "new tendrils", 0o600)

	a := Applier{Verify: func(_ context.Context, program, _, _ string) error {
		if program == "tendrils" {
			return errors.New("does not run")
		}
		return nil
	}}
	replaced, err := a.Apply(context.Background(), "0.4.0", []Replacement{
		{Program: "blossomd", Path: blossom, New: stagedB},
		{Program: "tendrils", Path: tendrils, New: stagedT},
	})
	if err == nil {
		t.Fatal("expected the bad binary to stop the whole upgrade")
	}
	if len(replaced) != 0 {
		t.Errorf("replaced = %v; a partial install is worse than none", replaced)
	}
	if got := readFile(t, blossom); got != "old blossomd" {
		t.Errorf("blossomd was replaced even though tendrils failed verification: %q", got)
	}
	if got := readFile(t, tendrils); got != "old tendrils" {
		t.Errorf("tendrils = %q", got)
	}
}

// Presence must not stand in for correctness. A file at the right path with the
// right name proves nothing; running it and asking its version is what proves
// it, and it happens before anything is moved.
func TestVerifyByExecAcceptsOnlyTheExpectedVersion(t *testing.T) {
	dir := t.TempDir()
	binary := copyExecutable(t, dir, "tendrils")
	t.Setenv(fakeVersionEnv, "0.4.0")

	if err := VerifyByExec(context.Background(), "tendrils", binary, "0.4.0"); err != nil {
		t.Errorf("a binary reporting the expected version should pass: %v", err)
	}
	// A "v" prefix on the wanted version is the tag form and must not matter.
	if err := VerifyByExec(context.Background(), "tendrils", binary, "v0.4.0"); err != nil {
		t.Errorf("v-prefixed version should pass: %v", err)
	}
	err := VerifyByExec(context.Background(), "tendrils", binary, "0.5.0")
	if err == nil {
		t.Fatal("a binary reporting a different version must be refused")
	}
	if !strings.Contains(err.Error(), "0.4.0") {
		t.Errorf("the refusal should say what it actually reported: %v", err)
	}
}

// A file that is not a runnable binary at all: a truncated unpack, an archive
// built for another architecture, a proxy's HTML error page saved as an exe.
func TestVerifyByExecRefusesSomethingThatDoesNotRun(t *testing.T) {
	dir := t.TempDir()
	notABinary := writeFile(t, filepath.Join(dir, "tendrils"), "<html>404 Not Found</html>", 0o755)
	if err := VerifyByExec(context.Background(), "tendrils", notABinary, "0.4.0"); err == nil {
		t.Fatal("a file that cannot run must not be installed")
	}
}

// The whole point, on the platform it matters: replace the binary of a process
// that is running right now. On Windows the image is locked against being
// overwritten or deleted but can be renamed, which is what the aside policy is
// for; on POSIX the rename simply lands and the running process keeps its
// inode. Either way the upgrade succeeds and the running process survives.
func TestApplyOverARunningBinary(t *testing.T) {
	dir := t.TempDir()
	installed := copyExecutable(t, dir, "tendrils")
	staged := copyExecutable(t, stagingDir(t, dir), "tendrils")

	// Hold the installed binary open by running it.
	running := exec.Command(installed)
	running.Env = append(os.Environ(), fakeSleepEnv+"=30s")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = running.Process.Kill()
		_, _ = running.Process.Wait()
	}()
	// Give the process a moment to actually map the image.
	time.Sleep(200 * time.Millisecond)

	t.Setenv(fakeVersionEnv, "0.4.0")
	replaced, err := Applier{}.Apply(context.Background(), "0.4.0", []Replacement{
		{Program: "tendrils", Path: installed, New: staged},
	})
	if err != nil {
		t.Fatalf("replacing a running binary must work on every platform: %v", err)
	}
	if len(replaced) != 1 {
		t.Fatalf("replaced = %v", replaced)
	}
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("no binary at %s after the upgrade: %v", installed, err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("the staged binary should have been moved into place")
	}

	// The still-running process holds the aside on Windows, so the sweep is
	// expected to be a no-op until it exits — and must never be an error.
	CleanupOld(installed)
	_ = running.Process.Kill()
	_, _ = running.Process.Wait()
	time.Sleep(200 * time.Millisecond)
	CleanupOld(installed)
	if matches, _ := filepath.Glob(installed + ".old*"); len(matches) != 0 {
		t.Errorf("aside binaries survived the sweep after the process exited: %v", matches)
	}
}

// stagingDir makes a subdirectory of the destination, mimicking the staging
// directory Download creates there so the final move is a same-filesystem
// rename.
func stagingDir(t *testing.T, dest string) string {
	t.Helper()
	dir := filepath.Join(dest, "staging")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCleanupOldIsHarmlessWhenThereIsNothingToClean(t *testing.T) {
	dir := t.TempDir()
	installed := writeFile(t, filepath.Join(dir, "tendrils"), "binary", 0o755)
	if n := CleanupOld(installed, filepath.Join(dir, "blossomd")); n != 0 {
		t.Errorf("removed %d files when there were none", n)
	}
	if got := readFile(t, installed); got != "binary" {
		t.Errorf("cleanup touched the installed binary: %q", got)
	}
}
