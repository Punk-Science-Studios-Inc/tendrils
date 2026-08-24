package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// verifyTimeout bounds executing a staged binary to ask its version. A binary
// that does not answer promptly is not one to install.
const verifyTimeout = 30 * time.Second

// Replacement is one binary to swap: the staged file that will take the place
// of the installed one.
type Replacement struct {
	Program string // "tendrils" or "blossomd", for messages and verification
	Path    string // the installed binary being replaced
	New     string // the staged, verified binary
}

// AsidePolicy decides whether the installed binary is renamed out of the way
// before the new one is moved in.
//
// POSIX does not need it: rename(2) over a running binary is fine, the running
// process keeps its inode, and the swap is one atomic step. Windows does: the
// image of a running executable is locked against being overwritten or deleted,
// but it can still be renamed. This project has already shipped a Windows-only
// "write over a running executable" defect once (fixed in 46d2c20 by building
// to a temp exe), which is why the policy is explicit rather than implied.
type AsidePolicy int

const (
	// AsideAuto renames aside on Windows only. The zero value, and the right
	// answer everywhere outside tests.
	AsideAuto AsidePolicy = iota
	// AsideAlways renames aside on every platform. Tests use it so the Windows
	// path is exercised on POSIX too; the Windows CI leg exercises it for real.
	AsideAlways
	// AsideNever renames straight over the destination.
	AsideNever
)

func (p AsidePolicy) enabled() bool {
	switch p {
	case AsideAlways:
		return true
	case AsideNever:
		return false
	default:
		return runtime.GOOS == "windows"
	}
}

// Applier installs staged binaries. The zero value is ready to use.
type Applier struct {
	// Verify confirms a staged binary runs and reports the expected version.
	// Nil means VerifyByExec — actually running it. Tests substitute this only
	// where the point of the test is something else; the exec path has its own.
	Verify func(ctx context.Context, program, path, wantVersion string) error
	// Aside selects the rename-aside policy. See AsidePolicy.
	Aside AsidePolicy
	// rename is os.Rename unless a test needs one to fail on demand. The
	// rollback below — putting the previous binary back when the new one cannot
	// be moved into place — decides whether a failed upgrade leaves a working
	// binary, and there is no portable way to make a rename fail at exactly that
	// moment. So the seam exists, unexported, for that one test.
	rename func(oldpath, newpath string) error
}

func (a Applier) move(oldpath, newpath string) error {
	if a.rename != nil {
		return a.rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

// Apply verifies each staged binary and moves it into place.
//
// Every step is ordered so that a failure leaves a working binary installed:
//
//  1. The staged binary is executed and asked its version. If it does not
//     answer with wantVersion, nothing is moved at all. Presence of a plausible
//     file at a plausible path is not evidence that it runs.
//  2. The installed binary is renamed aside (Windows) or replaced by rename
//     (POSIX). Both are atomic; neither can produce a half-written binary.
//  3. If the move of the new binary fails after an aside, the aside is undone,
//     so the previous binary is back before the error is returned.
//
// Order the replacements deliberately: they are applied as given, and
// docs/UPGRADING.md requires the Blossom host to move before the sync client.
// On a host running both, blossomd goes first. If it fails, tendrils is left
// untouched — both stale is a consistent state, and mixed is not.
//
// It returns the paths actually replaced, which is the truth even when err is
// non-nil.
func (a Applier) Apply(ctx context.Context, wantVersion string, reps []Replacement) ([]string, error) {
	verify := a.Verify
	if verify == nil {
		verify = VerifyByExec
	}
	// Verify everything before moving anything: a mixed install is worse than a
	// refused one, and the archive either holds two good binaries or is not
	// worth trusting for either.
	for _, r := range reps {
		if err := verify(ctx, r.Program, r.New, wantVersion); err != nil {
			return nil, err
		}
	}

	var replaced []string
	for _, r := range reps {
		if err := a.install(r); err != nil {
			return replaced, err
		}
		replaced = append(replaced, r.Path)
	}
	return replaced, nil
}

// install swaps one binary.
func (a Applier) install(r Replacement) error {
	perm := os.FileMode(0o755)
	fi, statErr := os.Stat(r.Path)
	if statErr == nil {
		// Keep whatever the owner chose (a 0700 private install stays private),
		// but a binary must be executable by whoever could run the old one.
		perm = fi.Mode().Perm() | 0o100
	}
	if err := os.Chmod(r.New, perm); err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}
	if statErr == nil {
		// Best effort, and only meaningful for a privileged upgrade of a binary
		// owned by someone else (sudo tendrils upgrade over ~/.local/bin).
		preserveOwner(r.Path, r.New)
	}

	aside := ""
	if a.Aside.enabled() && statErr == nil {
		var err error
		if aside, err = a.renameAside(r.Path); err != nil {
			return fmt.Errorf("selfupdate: cannot move %s aside to install the new one: %w", r.Path, err)
		}
	}

	if err := a.move(r.New, r.Path); err != nil {
		if aside != "" {
			// Put the working binary back before reporting. An upgrade that
			// fails must cost nothing but bandwidth.
			if rerr := a.move(aside, r.Path); rerr != nil {
				return fmt.Errorf("selfupdate: installing %s failed (%v) and restoring the previous binary from %s also failed: %w", r.Path, err, aside, rerr)
			}
		}
		return fmt.Errorf("selfupdate: install %s: %w", r.Path, err)
	}
	if aside != "" {
		// Usually fails while the old process still holds the image; that is
		// what CleanupOld is for.
		_ = os.Remove(aside)
	}
	return syncDir(filepath.Dir(r.Path))
}

// renameAside moves an installed binary to <path>.old and returns where it
// went. A previous .old is cleared first when it can be; when it cannot (an
// older process is still running from it) a uniquely suffixed name is used
// rather than failing the upgrade, and CleanupOld sweeps both later.
func (a Applier) renameAside(path string) (string, error) {
	old := path + ".old"
	if _, err := os.Stat(old); err == nil {
		if err := os.Remove(old); err != nil {
			old = fmt.Sprintf("%s.old.%d", path, time.Now().UnixNano())
		}
	}
	if err := a.move(path, old); err != nil {
		return "", err
	}
	return old, nil
}

// CleanupOld removes binaries left aside by a previous upgrade, for each of the
// given installed paths. Removing one fails while a process is still running
// from it, so this is best effort by design and is called on later runs until
// it succeeds. It returns how many it removed.
func CleanupOld(paths ...string) int {
	removed := 0
	for _, p := range paths {
		matches, err := filepath.Glob(p + ".old*")
		if err != nil {
			continue
		}
		sort.Strings(matches)
		for _, m := range matches {
			if err := os.Remove(m); err == nil {
				removed++
			}
		}
	}
	return removed
}

// VerifyByExec runs a staged binary and confirms it reports wantVersion.
//
// This is the check that keeps "a file arrived" from standing in for "a working
// binary arrived". A truncated unpack, an archive built for another
// architecture, a release whose build forgot its version stamp: all of them
// produce a file at the right path, and only running it tells them apart. It
// happens before any rename, so a failure costs a download and nothing else.
func VerifyByExec(ctx context.Context, program, path, wantVersion string) error {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	// Both binaries answer `version` as their first argument.
	cmd := exec.CommandContext(ctx, path, "version")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("selfupdate: the downloaded %s did not run (%w); the installed binary was left untouched: %s", program, err, firstLine(errOut.String()))
	}
	got, err := parseVersionOutput(out.String())
	if err != nil {
		return fmt.Errorf("selfupdate: the downloaded %s did not report a version (%w); the installed binary was left untouched", program, err)
	}
	if got != strings.TrimPrefix(wantVersion, "v") {
		return fmt.Errorf("selfupdate: the downloaded %s reports version %s, not the %s it was published as; refusing to install it", program, got, wantVersion)
	}
	return nil
}

// parseVersionOutput reads the first line of `<binary> version`, which both
// binaries render as "<program> <version>" via buildinfo.Info.Detail.
func parseVersionOutput(s string) (string, error) {
	fields := strings.Fields(firstLine(s))
	if len(fields) < 2 {
		return "", fmt.Errorf("unexpected output %q", firstLine(s))
	}
	return strings.TrimPrefix(fields[1], "v"), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// syncDir flushes a directory entry so a completed rename survives a power cut,
// the same discipline blossomd's storeStream follows. Directories cannot be
// opened for sync on Windows, where the rename is durable by other means.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return fmt.Errorf("selfupdate: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return fmt.Errorf("selfupdate: sync %s: %w", dir, err)
	}
	return nil
}
