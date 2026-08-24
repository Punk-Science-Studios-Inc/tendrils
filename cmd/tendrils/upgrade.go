package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/buildinfo"
	"ca.punkscience.tendrils/internal/selfupdate"
)

// upgradeOptions are the flags, kept separate so runUpgrade is callable from a
// test with a client pointed at an httptest.Server.
type upgradeOptions struct {
	check   bool
	version string
	force   bool
	yes     bool
}

func newUpgradeCmd() *cobra.Command {
	var o upgradeOptions

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Install the latest release, verified against the release checksums",
		Long: `Download the latest Tendrils release, verify it against the release's
checksums.txt, confirm the new binary runs and reports the expected version, and
only then move it into place. A failed upgrade leaves the working binary exactly
where it was.

blossomd is replaced too, but only if it already sits beside tendrils.

The daemon is not restarted: replacing a binary under a live daemon takes effect
on the next start, and a sync daemon that restarts itself mid-pass is a new
failure mode for no benefit. The restart command is printed instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd.Context(), cmd, selfupdate.New(), o)
		},
	}
	cmd.Flags().BoolVar(&o.check, "check", false, "report what is available and exit 1 if an upgrade exists; install nothing")
	cmd.Flags().StringVar(&o.version, "version", "", "install this exact release (e.g. v0.3.0), including a downgrade")
	cmd.Flags().BoolVar(&o.force, "force", false, "cross a declared wire-format boundary, or reinstall the running version")
	cmd.Flags().BoolVar(&o.yes, "yes", false, "assume yes for confirmations; never for a wire-format boundary, which needs --force")
	return cmd
}

func runUpgrade(ctx context.Context, cmd *cobra.Command, cl *selfupdate.Client, o upgradeOptions) error {
	out := cmd.OutOrStdout()
	current := buildinfo.Get().Version

	rel, available, err := resolveTarget(ctx, cl, current, o.version)
	if err != nil {
		if errors.Is(err, selfupdate.ErrNoReleases) {
			// The state the repository is in before its first release, and the
			// state a private repository presents to an anonymous client. Said
			// plainly, because "you are up to date" would be a lie and an error
			// would make a healthy new install look broken.
			fmt.Fprintf(out, "No releases have been published for %s yet, so there is nothing to upgrade to.\n", cl.Slug)
			fmt.Fprintf(out, "This device is running %s.\n", buildinfo.Get())
			recordCheck(rel, err)
			return nil
		}
		return err
	}
	recordCheck(rel, nil)

	fmt.Fprintf(out, "Current: %s   Latest: %s\n", current, rel.Version)
	if o.check {
		return reportCheck(out, rel, current, available)
	}

	switch {
	case o.version != "":
		if err := confirmPinned(cmd, rel, current, o); err != nil {
			return err
		}
	case !available:
		if !o.force {
			fmt.Fprintln(out, "Already on the latest release. Nothing to do.")
			return nil
		}
		fmt.Fprintln(out, "Reinstalling the current release because --force was given.")
	}

	if err := confirmBoundary(cmd, rel, current, o); err != nil {
		return err
	}
	return install(ctx, cmd, cl, rel, o)
}

// resolveTarget picks the release to install: an exact tag when pinned, the
// newest applicable one otherwise.
func resolveTarget(ctx context.Context, cl *selfupdate.Client, current, pinned string) (selfupdate.Release, bool, error) {
	if pinned == "" {
		return cl.Check(ctx, current)
	}
	rel, err := cl.ReleaseByTag(ctx, pinned)
	if err != nil {
		return selfupdate.Release{}, false, err
	}
	available := true
	if selfupdate.IsRelease(current) {
		if cmp, err := selfupdate.CompareVersions(rel.Version, current); err == nil {
			available = cmp > 0
		}
	}
	return rel, available, nil
}

// reportCheck implements --check: report, install nothing, and exit non-zero
// when an upgrade exists so a script or a monitoring job can act on it.
func reportCheck(out io.Writer, rel selfupdate.Release, current string, available bool) error {
	if !available {
		fmt.Fprintln(out, "You are on the latest release.")
		return nil
	}
	if boundary, why := selfupdate.Boundary(rel, current, buildinfo.WireFormat); boundary {
		fmt.Fprintf(out, "COORDINATED UPGRADE: %s.\nUpgrade every device together; see docs/UPGRADING.md.\n", why)
	}
	if rel.Notes != "" {
		fmt.Fprintln(out, rel.Notes)
	}
	fmt.Fprintln(out, "Run 'tendrils upgrade' to install it.")
	return exitCode(1)
}

// recordCheck folds a check performed by this command into the same cache the
// background check writes, so an explicit `upgrade` both silences a stale
// notice and defers the next background check.
func recordCheck(rel selfupdate.Release, err error) {
	path, perr := updateCachePath()
	if perr != nil {
		return
	}
	_ = selfupdate.Record(path, time.Now(), rel, err)
}

// confirmPinned handles --version: a downgrade or a reinstall is a deliberate
// act, so say what it is and get an answer unless one was given in advance.
func confirmPinned(cmd *cobra.Command, rel selfupdate.Release, current string, o upgradeOptions) error {
	if !selfupdate.IsRelease(current) {
		return nil
	}
	cmp, err := selfupdate.CompareVersions(rel.Version, current)
	if err != nil || cmp > 0 {
		return nil
	}
	what := fmt.Sprintf("%s is a downgrade from the running %s", rel.Version, current)
	if cmp == 0 {
		what = fmt.Sprintf("%s is the version already running", rel.Version)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s.\n", what)
	if o.yes || o.force {
		return nil
	}
	return confirm(cmd, "Continue?", "y")
}

// confirmBoundary is the coordinated-upgrade gate.
//
// A release that changes the wire format is one every device must cross
// together — upgrading a single device across it manufactures exactly the
// split-brain the boundary warns about, and the losing side is silent about it.
// So this gate is not satisfied by --yes. --yes means "do not ask me the
// routine questions"; crossing a wire-format boundary is not routine, and
// --force is the flag that says the rest of the fleet is being handled.
func confirmBoundary(cmd *cobra.Command, rel selfupdate.Release, current string, o upgradeOptions) error {
	boundary, why := selfupdate.Boundary(rel, current, buildinfo.WireFormat)
	if !boundary {
		return nil
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\nCOORDINATED UPGRADE: %s.\n", why)
	if rel.Notes != "" {
		fmt.Fprintln(out, rel.Notes)
	}
	fmt.Fprintln(out, "Devices on either side of a wire-format change can disagree without either")
	fmt.Fprintln(out, "of them being able to tell. Upgrade every device together — see docs/UPGRADING.md.")
	if o.force {
		fmt.Fprintln(out, "Proceeding because --force was given.")
		return nil
	}
	if !interactive(cmd) {
		return fmt.Errorf("refusing to cross a wire-format boundary without confirmation: re-run with --force once every other device is being upgraded too (--yes is deliberately not enough)")
	}
	return confirm(cmd, "Upgrade this device across the boundary anyway?", "yes")
}

// confirm asks a question on stdout and reads an answer. want is the exact
// answer required ("y" for routine questions, "yes" where the cost of a
// mistaken keystroke is a split fleet).
func confirm(cmd *cobra.Command, question, want string) error {
	if !interactive(cmd) {
		return fmt.Errorf("%s needs an answer and there is no terminal to ask on; pass --yes", question)
	}
	// The prompt goes to stderr so it is still visible when stdout is
	// redirected — a prompt nobody can see is a hang.
	fmt.Fprintf(cmd.ErrOrStderr(), "%s Type '%s' to continue: ", question, want)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		// Stdin looked like a terminal but had nothing to give — /dev/null is a
		// character device, so a service unit or a cron job lands here. Say what
		// to pass rather than reporting a cancellation nobody performed.
		return fmt.Errorf("no answer was given and stdin is empty: pass --yes to answer the routine questions in advance, or --force to cross a wire-format boundary")
	}
	if !strings.EqualFold(strings.TrimSpace(line), want) {
		return fmt.Errorf("upgrade cancelled")
	}
	return nil
}

// interactive reports whether there is a terminal to ask a question on.
func interactive(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// install downloads, verifies and swaps the binaries.
func install(ctx context.Context, cmd *cobra.Command, cl *selfupdate.Client, rel selfupdate.Release, o upgradeOptions) error {
	out := cmd.OutOrStdout()

	reps, err := installedBinaries(o.force)
	if err != nil {
		return err
	}
	dir := filepath.Dir(reps[len(reps)-1].Path)

	programs := make([]string, 0, len(reps))
	for _, r := range reps {
		programs = append(programs, r.Program)
	}

	fmt.Fprintf(out, "Downloading %s\n", selfupdate.ArchiveName(rel.Version, runtime.GOOS, runtime.GOARCH))
	dl, err := cl.Download(ctx, rel, runtime.GOOS, runtime.GOARCH, dir, programs)
	if err != nil {
		return err
	}
	defer dl.Cleanup()
	fmt.Fprintf(out, "Checksum verified (%s)\n", dl.SHA256[:16])

	staged := make([]selfupdate.Replacement, 0, len(reps))
	for _, r := range reps {
		path, ok := dl.Binaries[r.Program]
		if !ok {
			if r.Program == "tendrils" {
				return fmt.Errorf("the %s archive does not contain %s", rel.Version, selfupdate.BinaryName(r.Program, runtime.GOOS))
			}
			// blossomd is optional in the archive; a host that has one keeps the
			// one it has rather than the upgrade failing over it.
			fmt.Fprintf(out, "Note: the archive has no %s; leaving the installed one alone.\n", r.Program)
			continue
		}
		r.New = path
		staged = append(staged, r)
	}

	replaced, err := selfupdate.Applier{}.Apply(ctx, rel.Version, staged)
	for _, p := range replaced {
		fmt.Fprintf(out, "Replaced %s\n", p)
	}
	if err != nil {
		if len(replaced) == 0 {
			fmt.Fprintln(out, "Nothing was replaced; the installed binaries are untouched.")
		}
		return err
	}

	fmt.Fprintf(out, "\nUpgraded to %s.\n", rel.Version)
	printRestartHint(out, replaced)
	// The notice must not survive the upgrade that answered it.
	recordCheck(selfupdate.Release{Tag: rel.Tag, Version: rel.Version, WireFormat: rel.WireFormat, MinCompatible: rel.MinCompatible}, nil)
	return nil
}

// installedBinaries lists what this upgrade may replace, blossomd first.
//
// The ordering is from docs/UPGRADING.md: on a host running both, the Blossom
// server moves before the sync client. An updater that quietly inverted that
// would be doing the one thing the upgrade guide tells an owner not to do.
func installedBinaries(force bool) ([]selfupdate.Replacement, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot find the running binary to replace: %w", err)
	}
	// Follow symlinks: replacing the link would break every other link to the
	// same install, and replacing the target is what the owner means.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if dir := filepath.Dir(exe); !force && withinTemp(dir) {
		return nil, fmt.Errorf("this binary is running from a temporary directory (%s), which is what `go run` does — upgrade would install into it. Install a release first, or pass --force", dir)
	}
	return replacementsFor(exe), nil
}

// replacementsFor is the ordering and the beside-it rule, split out from
// locating the running binary so both can be tested.
func replacementsFor(exe string) []selfupdate.Replacement {
	var reps []selfupdate.Replacement
	blossom := filepath.Join(filepath.Dir(exe), selfupdate.BinaryName("blossomd", runtime.GOOS))
	if fi, err := os.Stat(blossom); err == nil && !fi.IsDir() {
		// Only where it already lives. Installing a blob server on a device
		// whose owner never asked for one is not an upgrade. First, because
		// docs/UPGRADING.md moves the Blossom host before the sync client.
		reps = append(reps, selfupdate.Replacement{Program: "blossomd", Path: blossom})
	}
	return append(reps, selfupdate.Replacement{Program: "tendrils", Path: exe})
}

func withinTemp(dir string) bool {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	rel, err := filepath.Rel(tmp, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// printRestartHint says what to restart, per platform. The daemon is never
// restarted here — see the command's Long text.
func printRestartHint(out io.Writer, replaced []string) {
	blossom := false
	for _, p := range replaced {
		if strings.HasPrefix(filepath.Base(p), "blossomd") {
			blossom = true
		}
	}
	_, running, _ := queryDaemon()
	if !running {
		fmt.Fprintln(out, "The new binary is live the next time you start the daemon:")
		fmt.Fprintln(out, "  tendrils daemon")
	} else {
		fmt.Fprintln(out, "Restart the daemon for it to take effect:")
		switch runtime.GOOS {
		case "windows":
			fmt.Fprintln(out, `  schtasks /End /TN "Tendrils Daemon"`)
			fmt.Fprintln(out, `  schtasks /Run /TN "Tendrils Daemon"`)
		case "darwin":
			fmt.Fprintln(out, "  restart however you run it (launchd job, terminal, or Ctrl-C and 'tendrils daemon')")
		default:
			fmt.Fprintln(out, "  systemctl --user restart tendrils-daemon.service")
		}
	}
	if blossom {
		fmt.Fprintln(out, "blossomd was replaced too — restart the blob server as well:")
		if runtime.GOOS == "windows" {
			fmt.Fprintln(out, "  restart however you run blossomd")
		} else {
			fmt.Fprintln(out, "  systemctl --user restart tendrils-blossom.service")
		}
	}
}
