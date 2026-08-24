package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/buildinfo"
	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/selfupdate"
)

// updateCheckCmdName is the hidden command a background check runs as. It is
// this binary, re-executed: a check must not be able to delay the command the
// owner actually ran, and a goroutine cannot outlive a process that exits in
// twenty milliseconds. The child writes update.json; the *next* invocation
// reads it. Nothing ever waits on the network to print its output.
const updateCheckCmdName = "update-check"

func newUpdateCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:    updateCheckCmdName,
		Short:  "Run the background update check and cache the result",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runBackgroundCheck(cmd.Context())
			// Always zero. Offline is silent: a failed check is a cached
			// failure, never an error anyone sees.
			return nil
		},
	}
}

// updateCachePath is where the update check records what it learned. One
// helper so every caller — CLI notice, background child, daemon, upgrade —
// reads and writes the same file under the same $TENDRILS_HOME.
func updateCachePath() (string, error) { return config.UpdatePath() }

// runBackgroundCheck performs one check and records the outcome. Every exit
// path is quiet.
func runBackgroundCheck(ctx context.Context) {
	if !config.UpdateCheckEnabled() {
		return
	}
	path, err := updateCachePath()
	if err != nil {
		return
	}
	rel, _, err := selfupdate.New().Check(ctx, buildinfo.Get().Version)
	_ = selfupdate.Record(path, time.Now(), rel, err)
	cleanupSupersededBinaries()
}

// maybeUpdateNotice prints the cached notice, then schedules the next check if
// one is due. Called from the root command's PersistentPostRun, so it runs
// after a command has produced its output and only when that command succeeded.
//
// The notice goes to stderr on purpose: `tendrils status | grep` must see
// exactly the status, and a device telling its owner it is stale must not be
// able to corrupt anything parsing stdout.
func maybeUpdateNotice(cmd *cobra.Command) {
	if !updateNoticeEligible(cmd) {
		return
	}
	// Both opt-outs are checked here, before anything is scheduled: an owner
	// who turned this off gets no cache read, no process spawn, no packet.
	if !config.UpdateCheckEnabled() {
		return
	}
	path, err := updateCachePath()
	if err != nil {
		return
	}
	cache, found := selfupdate.LoadCache(path)
	if found {
		if notice := cache.Notice(buildinfo.Get().Version, buildinfo.WireFormat); notice != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "\n%s", notice)
		}
	}
	if !found || cache.Due(time.Now()) {
		scheduleBackgroundCheck(path)
	}
	if runtime.GOOS == "windows" {
		// The very next run after a Windows upgrade is the first chance to drop
		// the binary the previous one had to leave behind.
		cleanupSupersededBinaries()
	}
}

// updateNoticeEligible decides whether this command may carry a notice.
//
// `version` is excluded because its output is what someone pastes into a bug
// report; `daemon` because it prints its own in the startup banner; `upgrade`
// and the check itself because they are already about updates.
func updateNoticeEligible(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	switch cmd.Name() {
	case "version", "help", "completion", updateCheckCmdName, "upgrade", "daemon":
		return false
	}
	return true
}

// scheduleBackgroundCheck claims the check and launches a detached child.
//
// The claim is written first and on purpose: if the child never reports back —
// killed with its shell, out of disk, a signal to the whole process group — the
// lease still moved, so a script running `tendrils status` in a loop cannot
// spawn a checker every iteration.
func scheduleBackgroundCheck(cachePath string) {
	if err := selfupdate.ClaimCheck(cachePath, time.Now()); err != nil {
		return // cannot record the claim, so do not start something unbounded
	}
	exe, ok := reExecutable()
	if !ok {
		return
	}
	child := exec.Command(exe, updateCheckCmdName)
	// No inherited stdio. A child holding the write end of `tendrils status |
	// head` would keep that pipeline open after tendrils exited.
	child.Stdin, child.Stdout, child.Stderr = nil, nil, nil
	child.Env = append(os.Environ(), updateChildEnv+"=1")
	if err := child.Start(); err != nil {
		return
	}
	_ = child.Process.Release()
}

// reExecutable returns the binary to re-run for a background check, and
// whether re-running it is safe at all.
//
// Two things must never be re-executed. A test binary: under `go test`,
// os.Executable() is the compiled test suite, and launching it with an
// unrecognised argument runs every test again — which would launch another,
// and another. And a checker child: it is already the check, so it must not
// schedule one. Both are cheap to detect and neither is worth finding out
// about in production.
func reExecutable() (string, bool) {
	if os.Getenv(updateChildEnv) != "" {
		return "", false
	}
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	base := strings.TrimSuffix(filepath.Base(exe), ".exe")
	if strings.HasSuffix(base, ".test") {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if withinTemp(filepath.Dir(exe)) {
		// `go run` and `go test` both build into a temporary directory. A
		// binary there is not an installation and has nothing to check for.
		return "", false
	}
	return exe, true
}

// updateChildEnv marks the background checker so it cannot spawn another.
const updateChildEnv = "TENDRILS_UPDATE_CHILD"

// cleanupSupersededBinaries removes the binaries a previous upgrade had to
// leave behind because Windows would not let it delete a running image. Best
// effort every time; it succeeds on the first run after the old process is
// gone. On POSIX nothing is ever left aside, so this finds nothing.
func cleanupSupersededBinaries() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	selfupdate.CleanupOld(exe, filepath.Join(dir, selfupdate.BinaryName("blossomd", runtime.GOOS)))
}

// updateSummary is the one-line form for the daemon's startup banner, or "".
func updateSummary(current string, wireFormat int) string {
	if !config.UpdateCheckEnabled() {
		return ""
	}
	path, err := updateCachePath()
	if err != nil {
		return ""
	}
	cache, found := selfupdate.LoadCache(path)
	if !found || !cache.Available(current) {
		return ""
	}
	line := fmt.Sprintf("%s available — run 'tendrils upgrade'", cache.Latest)
	if boundary, _ := selfupdate.Boundary(cache.Release(), current, wireFormat); boundary {
		line += " (COORDINATED UPGRADE — every device together)"
	}
	return line
}

// watchUpdates re-checks for a newer release while the daemon runs, and reports
// what it finds. The daemon is long-lived, so it checks inline in this
// goroutine rather than spawning the child a short-lived CLI command needs.
//
// It reports; it never upgrades. Replacing the binary under a live daemon only
// takes effect on the next start, restart is service-manager-specific, and a
// sync daemon that restarts itself mid-pass is a new failure mode for no
// benefit. `tendrils upgrade` is the owner's to run.
//
// A failed check is logged at debug and nowhere else: a daemon on a boat with
// no signal must not fill a journal with network errors about a feature nobody
// asked it to exercise.
func watchUpdates(ctx context.Context, log *slog.Logger) {
	if !config.UpdateCheckEnabled() {
		return
	}
	path, err := updateCachePath()
	if err != nil {
		return
	}
	current := buildinfo.Get().Version
	cl := selfupdate.New()

	check := func() {
		if cache, found := selfupdate.LoadCache(path); found && !cache.Due(time.Now()) {
			return
		}
		if err := selfupdate.ClaimCheck(path, time.Now()); err != nil {
			return
		}
		rel, available, err := cl.Check(ctx, current)
		_ = selfupdate.Record(path, time.Now(), rel, err)
		switch {
		case errors.Is(err, selfupdate.ErrNoReleases):
			// Nothing published to compare against. Not news, and not an error.
		case err != nil:
			log.Debug("update check failed", "err", err)
		case available:
			if boundary, why := selfupdate.Boundary(rel, current, buildinfo.WireFormat); boundary {
				log.Warn("a newer release is available and it is a coordinated upgrade",
					"latest", rel.Version, "running", current, "reason", why,
					"action", "upgrade every device together; see docs/UPGRADING.md")
				return
			}
			log.Info("a newer release is available", "latest", rel.Version, "running", current,
				"action", "run 'tendrils upgrade'")
		}
	}

	check()
	ticker := time.NewTicker(selfupdate.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}
