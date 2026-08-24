package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/crypt"
	"ca.punkscience.tendrils/internal/engine"
	"ca.punkscience.tendrils/internal/relay"
	"ca.punkscience.tendrils/internal/tree"
)

// newRepairCmd restores blobs that published events reference but the store no
// longer holds.
//
// Sync cannot fix this on its own, and that is the point of the command. The
// reconciler decides from plaintext hashes: when a file's local content matches
// what the relay says, the answer is "nothing to do" — it never asks whether the
// blob backing that event still exists. So a file whose blob was lost (a failed
// upload, or a corrupt blob correctly reclaimed by gc) sits there looking
// converged on every device while being unpullable by any device that does not
// already have it. Nothing reports it and nothing repairs it.
//
// The repair itself is exact rather than a re-publish. Sealing is deterministic,
// so the bytes the event names can be reconstructed from the local file and
// uploaded to precisely that address. No new event, no mtime change, no conflict
// risk — the tree is untouched and the hole is simply filled.
func newRepairCmd() *cobra.Command {
	var (
		apply  bool
		server string
	)

	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Re-upload blobs that published files reference but the store is missing",
		Long: "Find files whose published event points at a blob the Blossom server no longer\n" +
			"has, and restore that blob from the local copy.\n\n" +
			"Sync cannot detect this by itself: it compares file contents, not blob\n" +
			"availability, so an affected file looks perfectly converged. Reports only by\n" +
			"default; pass --apply to upload.",
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
			if cfg.SyncRoot == "" {
				return fmt.Errorf("no sync root set: run 'tendrils enroll --root <folder>'")
			}
			if len(cfg.Relays) == 0 {
				return fmt.Errorf("no relays configured: repair needs the relay to know what should exist")
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

			ctx := cmd.Context()
			relays := relay.New(cfg.Relays)
			defer relays.Close()

			fmt.Fprintln(out, "Fetching current file events from the relay…")
			evts, complete, err := relays.Fetch(ctx, id.PublicHex())
			if err != nil {
				return fmt.Errorf("fetch events: %w", err)
			}
			if !complete {
				// Repair only ever uploads, so a partial view costs coverage, not
				// safety: it repairs what it could see and says so.
				fmt.Fprintln(out, "Warning: the relay did not return its whole event set; some files may be missed.")
			}
			current, _ := engine.FoldRemote(evts)

			blobs := blob.New(server, id)
			fmt.Fprintln(out, "Listing the blob store…")
			stored, err := blobs.List(ctx, id.PublicHex())
			if err != nil {
				return fmt.Errorf("list blobs: %w", err)
			}
			present := make(map[string]struct{}, len(stored))
			for _, b := range stored {
				// A blob too short to be a sealed blob is not really there, whatever
				// the listing says: it is the corruption repair exists to undo.
				if b.Size >= 12+16 {
					present[b.SHA256] = struct{}{}
				}
			}

			var holes []*tree.Entry
			for _, e := range current {
				if !e.Live() || e.BlobHash == "" {
					continue
				}
				if _, ok := present[e.BlobHash]; !ok {
					holes = append(holes, e)
				}
			}
			if len(holes) == 0 {
				fmt.Fprintf(out, "\nAll %d published files have their blob. Nothing to repair.\n", len(current))
				return nil
			}
			fmt.Fprintf(out, "\n%d published file(s) reference a blob the store does not have:\n\n", len(holes))

			var repaired, unrepairable int
			for _, e := range holes {
				status, err := repairOne(ctx, cfg.SyncRoot, e, symKey, blobs, apply)
				if err != nil {
					unrepairable++
					fmt.Fprintf(out, "  ✗ %s\n      %v\n", e.Path, err)
					continue
				}
				repaired++
				fmt.Fprintf(out, "  %s %s\n", status, e.Path)
			}

			fmt.Fprintln(out)
			if apply {
				fmt.Fprintf(out, "Restored %d blob(s); %d could not be repaired from this device.\n", repaired, unrepairable)
			} else {
				fmt.Fprintf(out, "%d blob(s) can be restored from this device; %d cannot.\n", repaired, unrepairable)
				fmt.Fprintln(out, "Re-run with --apply to upload them.")
			}
			if unrepairable > 0 {
				fmt.Fprintln(out, "\nThe ones that cannot be repaired need a device still holding that exact")
				fmt.Fprintln(out, "version of the file. Run repair there.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "actually upload (default is a dry run)")
	cmd.Flags().StringVar(&server, "server", "", "Blossom server to repair (default: the first configured)")
	return cmd
}

// repairOne restores the blob for one entry from the local file, refusing unless
// the local copy reproduces exactly the bytes the event names.
func repairOne(ctx context.Context, root string, e *tree.Entry, symKey [32]byte, blobs *blob.Client, apply bool) (string, error) {
	abs := filepath.Join(root, filepath.FromSlash(e.Path))
	plaintext, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("not on this device")
		}
		return "", err
	}
	// The local file must be the version the event describes. A newer local copy
	// would seal to a different address and would not fill this hole.
	if got := hashHex(plaintext); got != e.Sha256 {
		return "", fmt.Errorf("local copy is a different version (%s, event names %s)", short12(got), short12(e.Sha256))
	}
	sealed, err := crypt.Seal(symKey, plaintext)
	if err != nil {
		return "", err
	}
	// Deterministic sealing means this should land on exactly the missing address.
	// If it does not, the blob was written by a build or key this repair cannot
	// reproduce, and uploading it would leave the event still dangling.
	if got := hashHex(sealed); got != e.BlobHash {
		return "", fmt.Errorf("re-sealing yields %s, not the referenced %s", short12(got), short12(e.BlobHash))
	}
	if !apply {
		return "would restore", nil
	}
	if _, err := blobs.Upload(ctx, sealed); err != nil {
		return "", err
	}
	return "restored", nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func short12(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
