package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/engine"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/rootfs"
	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/syncpath"
)

func newAdoptCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "adopt",
		Short: "Verify the configured sync folder and record it as this device's root",
		Long: "Records the identity of the configured sync folder so every later pass can " +
			"tell it apart from an empty mountpoint or a different drive. Needed once for " +
			"an enrollment made by an older build, and again after deliberately moving the " +
			"folder to another filesystem.\n\n" +
			"Adoption refuses a folder that holds none of the files this device last " +
			"synced: that is what an unmounted drive looks like. It never deletes anything. " +
			"Stop the daemon first; adoption needs the index.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, found, err := config.Load()
			if err != nil {
				return err
			}
			if !found || cfg.SyncRoot == "" {
				return fmt.Errorf("no sync root set: run 'tendrils enroll --root <folder>'")
			}
			root := cfg.SyncRoot
			info, err := os.Stat(root)
			if err != nil {
				return fmt.Errorf("sync root %s: %w (is the drive holding it mounted?)", root, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("sync root %s is not a directory", root)
			}

			idx, err := openIndexForRoot()
			if err != nil {
				return err
			}
			defer idx.Close()

			present, live, err := surveyRoot(root, idx, cfg.Exclude)
			if err != nil {
				return err
			}
			if live > 0 && present == 0 && !force {
				return fmt.Errorf("%s holds none of the %d files this device last synced — an unmounted drive's empty mountpoint looks exactly like this; mount it and retry, or pass --force if the folder really is meant to start empty", root, live)
			}
			markerID, hasMarker, err := rootid.HasMarker(root)
			if err != nil {
				return err
			}
			if hasMarker && !cfg.Root.IsZero() && markerID != cfg.Root.ID && !force {
				return fmt.Errorf("%s carries another enrollment's marker (%s, expected %s); pass --force to adopt it anyway", root, markerID, cfg.Root.ID)
			}

			id, err := rootid.Establish(root)
			if err != nil {
				return err
			}
			if err := idx.ResetMounts(); err != nil {
				return err
			}
			cfg.Root = id
			if err := config.Save(cfg); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Sync root adopted.")
			fmt.Fprintln(out, "  Folder:    ", root)
			fmt.Fprintln(out, "  Root ID:   ", id.ID)
			fmt.Fprintln(out, "  Filesystem:", orNone(id.FS))
			fmt.Fprintf(out, "  Found %d of the %d files last synced here.\n", present, live)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "adopt even if the folder looks empty or belongs to another enrollment")
	return cmd
}

// openIndexForRoot opens the index for a root-identity change, explaining the
// one failure an owner is likely to hit: a running daemon holding the lock.
func openIndexForRoot() (*index.Store, error) {
	path, err := config.IndexPath()
	if err != nil {
		return nil, err
	}
	idx, err := index.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w (stop the daemon first)", err)
	}
	return idx, nil
}

// surveyRoot counts how many of the files this device last synced the folder
// still holds, so adoption can refuse a folder that is evidently not the tree.
// Paths this node ignores are not counted either way.
func surveyRoot(root string, idx *index.Store, exclude []string) (present, live int, err error) {
	base, err := idx.All()
	if err != nil {
		return 0, 0, err
	}
	fsys, err := rootfs.Open(root)
	if err != nil {
		return 0, 0, err
	}
	defer fsys.Close()
	ign, err := engine.IgnoreMatcher(fsys, exclude)
	if err != nil {
		return 0, 0, err
	}
	for path, e := range base {
		if !e.Live() || ign.Match(path) {
			continue
		}
		live++
		info, err := fsys.Lstat(path)
		switch {
		case err == nil && info.Mode().IsRegular():
			present++
		case errors.Is(err, syncpath.ErrInvalid), errors.Is(err, syncpath.ErrBlocked), errors.Is(err, rootfs.ErrNotConfined):
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return 0, 0, err
		}
	}
	return present, live, nil
}
