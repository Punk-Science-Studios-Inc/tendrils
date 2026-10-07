package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/engine"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/rootfs"
	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/tree"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report what Tendrils is doing and has done",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			id, hasKey, err := config.LoadKey()
			if err != nil {
				return err
			}
			if !hasKey {
				fmt.Fprintln(out, "Not enrolled. Run 'tendrils keygen' then 'tendrils enroll'.")
				return nil
			}

			// Ask a running daemon first — it holds the index lock, so it is the
			// only one that can read the base, and the only one that knows live
			// state (a pass in flight, the last pass's error).
			snap, running, err := queryDaemon()
			if err != nil {
				return err
			}
			if !running {
				// No daemon: read the index directly. Safe — the lock is free.
				snap, err = localSnapshot(id)
				if err != nil {
					return err
				}
			}
			printStatus(out, snap, running)
			return nil
		},
	}
}

// localSnapshot reconstructs status from local state when no daemon is running.
func localSnapshot(id *keys.Identity) (statusSnapshot, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return statusSnapshot{}, err
	}
	npub, _ := id.Npub()
	snap := statusSnapshot{
		Identity: npub,
		SyncRoot: cfg.SyncRoot,
		Relays:   cfg.Relays,
		Storage:  cfg.BlossomServers,
	}

	idxPath, err := config.IndexPath()
	if err != nil {
		return snap, err
	}
	store, err := index.Open(idxPath)
	if err != nil {
		return snap, err
	}
	defer store.Close()

	last, stats, err := computeStatus(store, cfg.SyncRoot, cfg.Root, cfg.Exclude)
	if err != nil {
		return snap, err
	}
	snap.LastReconcile = last
	snap.Pending = stats.Pending
	snap.Conflicts = stats.Conflicts
	snap.Deferred = stats.Deferred
	snap.Unavailable = stats.Unavailable
	snap.Blocked = stats.Blocked
	snap.Paused = stats.Paused
	return snap, nil
}

// computeStatus derives, from an open index and the sync root, the last recorded
// reconcile and the same outstanding-work counts a daemon pass would report: how
// many local files differ from the last-synced base (pending upload/delete), how
// many conflict copies await the owner, and how many paths are waiting out a
// retry backoff. Its reads are safe to run concurrently with the engine's writes
// on the same index handle.
//
// It applies the same ignore rules the engine does (shared .tendrilsignore plus
// this node's "exclude" patterns), so a path this node has opted out of and
// deleted does not read as a forever-pending local deletion. A root that fails
// verification is reported as paused rather than scanned: an empty mountpoint
// would otherwise read as every file pending deletion.
func computeStatus(store *index.Store, root string, rootID rootid.Identity, exclude []string) (last time.Time, stats engine.Stats, err error) {
	last, err = store.LastReconcile()
	if err != nil {
		return
	}
	base, err := store.All()
	if err != nil {
		return
	}
	retries, err := store.Retries()
	if err != nil {
		return
	}
	now := time.Now()
	for _, r := range retries {
		if r.NextAttempt.After(now) {
			stats.Deferred++
		}
	}
	if root == "" {
		return
	}
	fsys, verr := rootfs.OpenVerified(root, rootID)
	if verr != nil {
		stats.Paused = verr.Error()
		return
	}
	defer fsys.Close()
	ign, ierr := engine.IgnoreMatcher(fsys, exclude)
	if ierr != nil {
		stats.Paused = ierr.Error()
		return
	}
	mounts, err := store.Mounts()
	if err != nil {
		return
	}
	scanned, err := scan.Walk(fsys, scan.Options{Base: base, Ignore: ign, Mounts: mounts})
	if err != nil {
		return
	}
	// Paths blocked by what only the relay knows (a name another device published
	// that this one cannot represent) come from the last daemon pass.
	blocked, err := store.Blocked()
	if err != nil {
		return
	}
	for p, reason := range scanned.Blocked {
		blocked[p] = reason
	}
	for p := range blocked {
		if !ign.Match(p) && !ign.PruneDir(p) {
			stats.Blocked++
		}
	}
	local, cov := scanned.Entries, scanned.Coverage
	known := make([]string, 0, len(local)+len(base))
	for path := range local {
		known = append(known, path)
	}
	for path, b := range base {
		if _, ok := local[path]; !ok && b.Live() {
			known = append(known, path)
		}
	}
	stats.Unavailable = engine.CountUnavailable(cov, ign, known)

	for path, e := range local {
		if !cov.Observed(path) || isBlocked(blocked, path) {
			continue
		}
		if ign.Match(path) {
			continue // invisible to this node; neither pending nor a conflict
		}
		if scan.IsConflictCopy(path) {
			stats.Conflicts++
			continue
		}
		if b := base[path]; !tree.SameContent(b, e) {
			stats.Pending++ // new or edited since last sync
		}
	}
	for path, b := range base {
		if !b.Live() || ign.Match(path) || !cov.Observed(path) || isBlocked(blocked, path) {
			continue
		}
		if _, stillHere := local[path]; !stillHere {
			stats.Pending++ // deleted locally since last sync
		}
	}
	return
}

// isBlocked reports whether path, or a directory above it, is blocked.
func isBlocked(blocked map[string]string, path string) bool {
	for p := path; ; {
		if _, ok := blocked[p]; ok {
			return true
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return false
		}
		p = p[:i]
	}
}

func printStatus(out io.Writer, snap statusSnapshot, daemonRunning bool) {
	fmt.Fprintln(out, "Identity: ", snap.Identity)
	fmt.Fprintln(out, "Sync root:", orNone(snap.SyncRoot))
	fmt.Fprintln(out, "Relays:   ", orDiscovery(snap.Relays))
	fmt.Fprintln(out, "Storage:  ", orDiscovery(snap.Storage))
	fmt.Fprintln(out, "Daemon:   ", daemonState(daemonRunning, snap.Syncing))
	if snap.Paused != "" {
		fmt.Fprintln(out, "Paused:   ", snap.Paused)
	}
	if snap.Syncing {
		fmt.Fprintln(out, "Progress: ", progressLine(snap))
	}
	fmt.Fprintln(out, "Last reconcile:", formatTime(snap.LastReconcile))
	fmt.Fprintln(out, "Pending changes:", snap.Pending)
	fmt.Fprintln(out, "Conflicts:     ", snap.Conflicts)
	if snap.Unavailable > 0 {
		fmt.Fprintf(out, "Unavailable:     %d (could not be read; held back, never treated as deleted)\n", snap.Unavailable)
	}
	if snap.Blocked > 0 {
		fmt.Fprintf(out, "Blocked:         %d (cannot be represented on this device; left untouched, still synced elsewhere)\n", snap.Blocked)
	}
	if snap.Deferred > 0 {
		fmt.Fprintf(out, "Stuck:           %d (repeatedly failed, waiting out a retry backoff)\n", snap.Deferred)
	}
	if snap.LastError != "" {
		fmt.Fprintln(out, "Last pass error:", snap.LastError)
	}
	if !daemonRunning {
		fmt.Fprintln(out, "Reachability:   not checked (daemon not running)")
	}
}

// progressLine renders the current pass position. Before any action has begun
// (Total still 0) the daemon is scanning and fetching, not yet acting on a file.
func progressLine(snap statusSnapshot) string {
	switch {
	case snap.Total == 0:
		return "preparing (scanning and fetching)"
	case snap.Current == "":
		return fmt.Sprintf("%d/%d done", snap.Done, snap.Total)
	default:
		return fmt.Sprintf("%d/%d — %s %s", snap.Done, snap.Total, snap.Operation, snap.Current)
	}
}

func daemonState(running, syncing bool) string {
	switch {
	case !running:
		return "not running"
	case syncing:
		return "running (reconciling now)"
	default:
		return "running (idle)"
	}
}

func orNone(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}

func orDiscovery(ss []string) string {
	if len(ss) == 0 {
		return "(discovered from key)"
	}
	return fmt.Sprint(ss)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}
