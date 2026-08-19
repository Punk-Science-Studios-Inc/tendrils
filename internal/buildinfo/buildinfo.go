// Package buildinfo reports which build of Tendrils is running.
//
// It matters more here than in most tools. The wire format has already changed
// once in a way that is silent on the losing side — a device on a pre-2026-07-23
// build publishes created_at = mtime, and the relay drops every one of its
// events without telling it (see "Two clocks" in AGENTS.md). Asking a device
// what it is running is how that gets diagnosed, so every binary carries it and
// the daemon prints it at startup.
//
// Release builds stamp the values below with -ldflags -X. Everything else falls
// back to the module and VCS data the Go toolchain embeds automatically, so a
// binary built from a git clone still reports a usable commit and dirty flag.
package buildinfo

import (
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Stamped by the release build:
//
//	-ldflags "-X ca.punkscience.tendrils/internal/buildinfo.version=1.2.3 ..."
//
// Both binaries take the same stamp, so tendrils and blossomd from one release
// always report the same version.
var (
	version string
	commit  string
	date    string
)

// Info is one binary's identity.
type Info struct {
	Version string // semver of the release, or "dev" for an unreleased build
	Commit  string // full git SHA when known
	Date    string // RFC3339 build or commit time when known
	Dirty   bool   // built from a tree with uncommitted changes
}

// Get returns the running binary's build information.
var Get = sync.OnceValue(func() Info {
	bi, _ := debug.ReadBuildInfo()
	return resolve(version, commit, date, bi)
})

// resolve merges the ldflags stamp with the toolchain's embedded build info.
// The stamp always wins: it is the only source that knows the release tag.
func resolve(version, commit, date string, bi *debug.BuildInfo) Info {
	info := Info{Version: version, Commit: commit, Date: date}
	if bi != nil {
		if info.Version == "" && isRelease(bi.Main.Version) {
			info.Version = strings.TrimPrefix(bi.Main.Version, "v")
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = s.Value
				}
			case "vcs.modified":
				info.Dirty = s.Value == "true"
			}
		}
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	return info
}

// pseudoVersion matches the timestamp-and-revision form the toolchain
// synthesizes for a module with no tag, e.g. 0.0.0-20260728020249-a81f7ab00a90.
var pseudoVersion = regexp.MustCompile(`[0-9]{14}-[0-9a-f]{12}`)

// isRelease reports whether a module version names an actual released tag.
// Anything else — "(devel)", a synthesized pseudo-version — says only "built
// from source", which "dev" already says more honestly.
func isRelease(v string) bool {
	if v == "" || v == "(devel)" || pseudoVersion.MatchString(v) {
		return false
	}
	return strings.HasPrefix(v, "v")
}

// ShortCommit is the commit abbreviated to the usual 7 characters.
func (i Info) ShortCommit() string {
	if len(i.Commit) > 7 {
		return i.Commit[:7]
	}
	return i.Commit
}

// String is the one-line form, e.g. "1.2.3 (a81f7ab, dirty)".
func (i Info) String() string {
	var parts []string
	if c := i.ShortCommit(); c != "" {
		parts = append(parts, c)
	}
	if i.Dirty {
		parts = append(parts, "dirty")
	}
	if len(parts) == 0 {
		return i.Version
	}
	return fmt.Sprintf("%s (%s)", i.Version, strings.Join(parts, ", "))
}

// Detail is the multi-line form printed by the version command: everything
// worth pasting into a bug report.
func (i Info) Detail(program string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", program, i.Version)
	if i.Commit != "" {
		dirty := ""
		if i.Dirty {
			dirty = " (dirty)"
		}
		fmt.Fprintf(&b, "  commit: %s%s\n", i.Commit, dirty)
	}
	if i.Date != "" {
		fmt.Fprintf(&b, "  built:  %s\n", i.Date)
	}
	fmt.Fprintf(&b, "  go:     %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return b.String()
}
