package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/rootid"
)

func newEnrollCmd() *cobra.Command {
	var keyArg, rootArg string
	var relays, blossom, exclude []string

	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Enroll this device into your sync set",
		Long: "Enroll this device with your key and point it at a local folder. No " +
			"password, no account. If a key is already stored, --key may be omitted.\n\n" +
			"Each device may place its sync root wherever it likes; files sync by " +
			"relative path, not by absolute location.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := resolveKey(keyArg)
			if err != nil {
				return err
			}

			if rootArg == "" {
				return fmt.Errorf("a sync folder is required: pass --root <folder>")
			}
			root, err := prepareRoot(rootArg)
			if err != nil {
				return err
			}
			rootID, err := establishEnrolledRoot(root)
			if err != nil {
				return err
			}

			if err := config.SaveKey(id); err != nil {
				return err
			}

			cfg, _, err := config.Load()
			if err != nil {
				return err
			}
			cfg.SyncRoot = root
			cfg.Root = rootID
			if len(relays) > 0 {
				cfg.Relays = relays
			}
			if len(blossom) > 0 {
				cfg.BlossomServers = blossom
			}
			if len(exclude) > 0 {
				cfg.Exclude = exclude
			}
			if err := config.Save(cfg); err != nil {
				return err
			}

			npub, _ := id.Npub()
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Device enrolled.")
			fmt.Fprintln(out, "  Identity: ", npub)
			fmt.Fprintln(out, "  Sync root:", root)
			if len(cfg.Relays) == 0 {
				fmt.Fprintln(out, "  Relays:    (will be discovered from your key)")
			} else {
				fmt.Fprintln(out, "  Relays:   ", cfg.Relays)
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "Start syncing with:  tendrils daemon")
			return nil
		},
	}

	cmd.Flags().StringVar(&keyArg, "key", "", "Nostr secret key (nsec or hex); omit to reuse a stored key")
	cmd.Flags().StringVar(&rootArg, "root", "", "local folder to sync (the sync root)")
	cmd.Flags().StringSliceVar(&relays, "relay", nil, "relay URL override (repeatable); default is discovery")
	cmd.Flags().StringSliceVar(&blossom, "blossom", nil, "Blossom server URL override (repeatable)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "path pattern this device should not sync (repeatable), e.g. music/ or *.iso")
	return cmd
}

// resolveKey returns the identity from the --key flag, or the stored key if the
// flag is empty. Enrollment needs the secret key; there is no other credential.
func resolveKey(keyArg string) (*keys.Identity, error) {
	if keyArg != "" {
		return keys.Parse(keyArg)
	}
	id, found, err := config.LoadKey()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no key given and none stored: pass --key <nsec> or run 'tendrils keygen'")
	}
	return id, nil
}

// establishEnrolledRoot marks root as this device's sync folder. A device that
// has synced before must not have its history pointed at an unmarked folder —
// an empty mountpoint, a fresh directory — because the next pass would read
// every file it remembers as deleted. That case is left to an explicit adopt.
func establishEnrolledRoot(root string) (rootid.Identity, error) {
	if _, hasMarker, err := rootid.HasMarker(root); err != nil {
		return rootid.Identity{}, err
	} else if !hasMarker {
		idx, err := openIndexForRoot()
		if err != nil {
			return rootid.Identity{}, err
		}
		base, err := idx.All()
		idx.Close()
		if err != nil {
			return rootid.Identity{}, err
		}
		live := 0
		for _, e := range base {
			if e.Live() {
				live++
			}
		}
		if live > 0 {
			return rootid.Identity{}, fmt.Errorf("this device has synced %d files before, and %s is not a marked sync folder: if it is the same tree (for example, a drive enrolled by an older build), run 'tendrils adopt'; to start this device over, remove %s first", live, root, mustIndexPath())
		}
	}
	return rootid.Establish(root)
}

func mustIndexPath() string {
	p, err := config.IndexPath()
	if err != nil {
		return "the index"
	}
	return p
}

// prepareRoot validates and creates the sync root, returning its absolute path.
func prepareRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve folder: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("create sync root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}
