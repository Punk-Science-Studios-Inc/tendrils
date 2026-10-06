package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/config"
)

func newExcludeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exclude",
		Short: "Manage paths this device does not sync",
		Long: "Exclude paths this device should not keep locally, without affecting any\n" +
			"other device. An excluded path is invisible to this node: never pulled,\n" +
			"published, trashed or tombstoned, so an existing local copy can be\n" +
			"deleted to reclaim space and will not come back. Patterns use\n" +
			"gitignore-style syntax (blank lines and # comments are ignored, a\n" +
			"trailing / marks a directory, and * ? ** match path segments).\n\n" +
			"This is per-device configuration and is never published. To ignore a\n" +
			"path on every device at once, edit the synced .tendrilsignore at the\n" +
			"sync root instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listExclude(cmd)
		},
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List this device's exclude patterns",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return listExclude(cmd)
			},
		},
		&cobra.Command{
			Use:   "add <pattern>...",
			Short: "Add one or more exclude patterns",
			Args:  cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return addExclude(cmd, args)
			},
		},
		&cobra.Command{
			Use:   "remove <pattern>...",
			Short: "Remove one or more exclude patterns",
			Args:  cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return removeExclude(cmd, args)
			},
		},
	)
	return cmd
}

// loadExcludeConfig loads the local config, requiring that this device is
// enrolled — exclude patterns live in config.json, which enrollment creates.
func loadExcludeConfig() (config.Config, error) {
	cfg, found, err := config.Load()
	if err != nil {
		return cfg, err
	}
	if !found {
		return cfg, fmt.Errorf("not enrolled: run 'tendrils enroll' first")
	}
	return cfg, nil
}

func listExclude(cmd *cobra.Command) error {
	cfg, err := loadExcludeConfig()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(cfg.Exclude) == 0 {
		fmt.Fprintln(out, "This device excludes nothing; it keeps every synced path locally.")
		return nil
	}
	for _, p := range cfg.Exclude {
		fmt.Fprintln(out, p)
	}
	return nil
}

func addExclude(cmd *cobra.Command, patterns []string) error {
	cfg, err := loadExcludeConfig()
	if err != nil {
		return err
	}
	added := 0
	for _, p := range patterns {
		if p == "" || contains(cfg.Exclude, p) {
			continue
		}
		cfg.Exclude = append(cfg.Exclude, p)
		added++
	}
	if added == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No change; those patterns were already excluded.")
		return nil
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Added %d pattern(s). This device now excludes:\n", added)
	for _, p := range cfg.Exclude {
		fmt.Fprintln(cmd.OutOrStdout(), "  ", p)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "\nRestart the daemon ('systemctl --user restart tendrils.service') to apply.")
	return nil
}

func removeExclude(cmd *cobra.Command, patterns []string) error {
	cfg, err := loadExcludeConfig()
	if err != nil {
		return err
	}
	kept := cfg.Exclude[:0]
	for _, p := range cfg.Exclude {
		if contains(patterns, p) {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == len(cfg.Exclude) {
		fmt.Fprintln(cmd.OutOrStdout(), "No change; none of those patterns were excluded.")
		return nil
	}
	cfg.Exclude = kept
	if err := config.Save(cfg); err != nil {
		return err
	}
	if len(cfg.Exclude) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "This device now excludes nothing.")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "This device now excludes:")
		for _, p := range cfg.Exclude {
			fmt.Fprintln(cmd.OutOrStdout(), "  ", p)
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), "\nRestart the daemon ('systemctl --user restart tendrils.service') to apply.")
	return nil
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
