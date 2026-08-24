package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/engine"
	"ca.punkscience.tendrils/internal/gc"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/relay"
	"ca.punkscience.tendrils/internal/tree"
)

func newGCCmd() *cobra.Command {
	var (
		apply           bool
		grace           time.Duration
		trustReferences bool
		server          string
		workers         int
		maxInflightMB   int64
	)

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim Blossom blobs no current file references",
		Long: "Reclaim storage held by blobs nothing points at any more — old versions of\n" +
			"edited files, deleted files' contents, and duplicates left by earlier builds.\n\n" +
			"Reports only by default. Pass --apply to delete. The keep-set comes from the\n" +
			"relay, so run this where the relay is reachable; a fetch that looks incomplete\n" +
			"aborts the sweep rather than risk deleting live blobs.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			id, hasKey, err := config.LoadKey()
			if err != nil {
				return err
			}
			if !hasKey {
				return fmt.Errorf("not enrolled: run 'tendrils keygen' then 'tendrils enroll'")
			}
			cfg, _, err := config.Load()
			if err != nil {
				return err
			}
			if len(cfg.Relays) == 0 {
				return fmt.Errorf("no relays configured: the keep-set comes from the relay, so gc cannot run without one")
			}
			if server == "" {
				if len(cfg.BlossomServers) == 0 {
					return fmt.Errorf("no Blossom server configured: pass --server")
				}
				server = cfg.BlossomServers[0]
			}

			symKey, err := id.SymmetricKey()
			if err != nil {
				return err
			}

			// The daemon holds the index lock, so this is a read the daemon must not
			// be doing concurrently. Fail clearly rather than block.
			idxPath, err := config.IndexPath()
			if err != nil {
				return err
			}
			if _, running, _ := queryDaemon(); running {
				return fmt.Errorf("the daemon is running and holds the index; stop it first (systemctl --user stop tendrils-daemon) so gc can read the base")
			}
			// Read the base and let go of the index immediately. A sweep of a large
			// store takes hours, and holding the lock for all of it would keep the
			// daemon from syncing the whole time — for data it only needs at the
			// start. Blobs the daemon uploads while the sweep runs are protected by
			// the grace period, which is what that period is for.
			base, err := readBase(idxPath)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			relays := relay.New(cfg.Relays)
			defer relays.Close()

			fmt.Fprintln(out, "Fetching live file events from the relay…")
			live, nEvents, nPaths, err := liveBlobsFromRelay(ctx, relays, id.PublicHex())
			if err != nil {
				return fmt.Errorf("fetch live events: %w (refusing to sweep on an incomplete view)", err)
			}
			fmt.Fprintf(out, "Relay: %d events folded to %d current paths\n", nEvents, nPaths)

			// The relay's own answer, kept separate: it is what *must* exist for
			// every file to be pullable. The base union below only widens what we
			// refuse to delete.
			required := make(map[string]struct{}, len(live))
			for h := range live {
				required[h] = struct{}{}
			}

			// Union the device's own base in. It covers anything this device
			// published whose event the relay did not return.
			fromBase := 0
			for _, e := range base {
				if !e.Live() {
					continue
				}
				for _, h := range blobAddresses(e) {
					if _, already := live[h]; !already {
						live[h] = struct{}{}
						fromBase++
					}
				}
			}
			fmt.Fprintf(out, "Keep-set: %d blobs (%d only in this device's index)\n", len(live), fromBase)

			blobs := blob.New(server, id)
			opts := gc.Options{
				Apply:            apply,
				Grace:            grace,
				TrustReferences:  trustReferences,
				SymKey:           symKey,
				Workers:          workers,
				MaxInFlightBytes: maxInflightMB << 20,
				Required:         required,
			}
			if trustReferences {
				fmt.Fprintln(out, "Ownership checks DISABLED (--trust-references): every unreferenced blob")
				fmt.Fprintln(out, "is treated as ours. Only correct if this server holds no other identity's blobs.")
			} else {
				fmt.Fprintln(out, "Proving ownership by decrypting each candidate; this reads them, so it is slow.")
			}
			if apply {
				fmt.Fprintf(out, "Sweeping %s — DELETING.\n\n", server)
			} else {
				fmt.Fprintf(out, "Sweeping %s — dry run, nothing will be deleted.\n\n", server)
			}

			plan, err := gc.Sweep(ctx, blobs, id.PublicHex(), live, countLive(base), opts)
			if err != nil {
				return err
			}
			printPlan(out, plan, apply)
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "actually delete (default is a dry run)")
	cmd.Flags().DurationVar(&grace, "grace", gc.DefaultGrace, "spare blobs written more recently than this")
	cmd.Flags().BoolVar(&trustReferences, "trust-references", false,
		"skip per-blob ownership proof; only for a store holding no other identity's blobs")
	cmd.Flags().StringVar(&server, "server", "", "Blossom server to sweep (default: the first configured)")
	cmd.Flags().IntVar(&workers, "workers", 0,
		"concurrent ownership checks (0 = default)")
	cmd.Flags().Int64Var(&maxInflightMB, "max-inflight-mb", 0,
		"ceiling on blob bytes held in memory at once; raise it on a large host (0 = default 256)")
	return cmd
}

// liveBlobsFromRelay builds the keep-set from what the relay currently says is
// true, and reports how many events it folded and how many live paths resulted.
//
// The fold is the important part. Relays keep superseded replaceable events, so a
// raw fetch returns several versions of the same path — on the author's store,
// 11,269 blob addresses across 4,805 files. Treating all of them as live would
// spare every old version forever, which is exactly the garbage this command
// exists to reclaim. engine.FoldRemote applies the same winner-per-path rule the
// sync engine does, so "live" here means what a syncing device would actually pull.
func liveBlobsFromRelay(ctx context.Context, relays *relay.Client, pubkey string) (live map[string]struct{}, events, paths int, err error) {
	evts, complete, err := relays.Fetch(ctx, pubkey)
	if err != nil {
		return nil, 0, 0, err
	}
	// Deleting a blob is the one thing syncing again cannot undo, so a keep-set
	// built from an answer the relay did not finish giving is not usable at any
	// price. Refuse rather than guess: every event we failed to read is a file
	// whose blobs would look like orphans.
	if !complete {
		return nil, 0, 0, fmt.Errorf("the relay did not return its whole event set, so the keep-set would be missing files this sweep would then delete. " +
			"If this persists, the relay is holding more events at one timestamp than it will return in one query (NIP-01 pagination cannot reach past that) — " +
			"prune the superseded events from the relay and try again")
	}
	current, skipped := engine.FoldRemote(evts)
	if len(skipped) > 0 {
		// Not fatal, but worth saying: an event we cannot read is not a licence to
		// delete the blob it might describe.
		fmt.Fprintf(os.Stderr, "warning: %d events could not be parsed and were ignored\n", len(skipped))
	}
	entries := make([]*tree.Entry, 0, len(current))
	for _, e := range current {
		entries = append(entries, e)
	}
	return gc.LiveBlobs(entries), len(evts), len(current), nil
}

// readBase loads the index base and closes the index again, so the caller does
// not hold the lock for the duration of a long sweep.
func readBase(idxPath string) (map[string]*tree.Entry, error) {
	store, err := index.Open(idxPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.All()
}

func countLive(base map[string]*tree.Entry) int {
	n := 0
	for _, e := range base {
		if e.Live() {
			n += len(blobAddresses(e))
		}
	}
	return n
}

// blobAddresses is every address one live entry puts on the server: its single
// blob, or each of its chunks. A caller that reads only BlobHash misses a
// chunked file's contents entirely, and in a keep-set that means deleting them.
func blobAddresses(e *tree.Entry) []string {
	if e.Chunked() {
		hs := make([]string, 0, len(e.Chunks))
		for _, c := range e.Chunks {
			if c.BlobHash != "" {
				hs = append(hs, c.BlobHash)
			}
		}
		return hs
	}
	if e.BlobHash == "" {
		return nil
	}
	return []string{e.BlobHash}
}

func printPlan(out io.Writer, p gc.Plan, applied bool) {
	fmt.Fprintf(out, "Blobs on server:  %7d  %10s\n", p.TotalBlobs, humanBytes(p.TotalBytes))
	fmt.Fprintf(out, "  referenced:     %7d  %10s\n", p.Kept, humanBytes(p.KeptBytes))
	fmt.Fprintf(out, "  unreferenced:   %7d  %10s\n", p.Orphans, humanBytes(p.OrphanBytes))
	if p.Invalid > 0 {
		fmt.Fprintf(out, "  invalid:        %7d  %10s  (too short to be a sealed blob; removed even if referenced)\n",
			p.Invalid, humanBytes(p.InvalidBytes))
	}
	if p.TooRecent > 0 {
		fmt.Fprintf(out, "  too recent:     %7d  %10s  (inside the grace period, spared)\n",
			p.TooRecent, humanBytes(p.TooRecentBytes))
	}
	if p.NotOurs > 0 {
		fmt.Fprintf(out, "  not ours:       %7d  %10s  (did not decrypt under this key, left alone)\n",
			p.NotOurs, humanBytes(p.NotOursBytes))
	}
	if p.Missing > 0 {
		fmt.Fprintf(out, "\nMISSING:          %7d  blobs a current file references but the store does not hold.\n", p.Missing)
		fmt.Fprintln(out, "  Those files cannot be pulled by a device that does not already have them.")
		fmt.Fprintln(out, "  A sweep cannot cause this; it is an upload that never completed.")
		for _, h := range p.MissingBlobs {
			fmt.Fprintln(out, "  -", h)
		}
	}
	fmt.Fprintln(out)
	if applied {
		fmt.Fprintf(out, "Deleted:          %7d  %10s reclaimed\n", p.Deleted, humanBytes(p.DeletedBytes))
	} else {
		fmt.Fprintf(out, "Would delete:     %7d  %10s reclaimable\n",
			p.Orphans+p.Invalid, humanBytes(p.OrphanBytes+p.InvalidBytes))
		fmt.Fprintln(out, "\nRe-run with --apply to delete.")
	}
	if p.Failed > 0 {
		fmt.Fprintf(out, "Failed:           %7d\n", p.Failed)
		for _, e := range p.FirstErrors {
			fmt.Fprintln(out, "  -", e)
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	val := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		val /= unit
		if val < unit {
			return fmt.Sprintf("%.1f %s", val, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", val)
}
