// Command tendrils is the headless sync daemon and CLI for Tendrils. A CLI plus
// logs is the most legible interface possible, which is the whole point: a tool
// you can open the hood on, understand, and repair.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/buildinfo"
)

// exitCode is an error that carries a process exit status and nothing else.
// `upgrade --check` needs to exit 1 to mean "an upgrade is available" — a
// status a script can branch on — without printing an error, because nothing
// went wrong.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "tendrils",
		Short:         "Deliberate folder sync over Nostr + Blossom",
		Long:          "Tendrils keeps your personal folders identical across your devices by editing locally and syncing deliberately over Nostr + Blossom.",
		Version:       buildinfo.Get().String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		// After the command has printed what was asked for — and only when it
		// succeeded — say if this device is behind, and schedule the next check
		// if one is due. Cobra skips PersistentPostRun on error, which is the
		// behaviour wanted here: a failing command has enough to say already.
		PersistentPostRun: func(cmd *cobra.Command, _ []string) {
			maybeUpdateNotice(cmd)
		},
	}
	root.AddCommand(
		newKeygenCmd(),
		newEnrollCmd(),
		newAdoptCmd(),
		newStatusCmd(),
		newExcludeCmd(),
		newDaemonCmd(),
		newGCCmd(),
		newRepairCmd(),
		newRetryCmd(),
		newUpgradeCmd(),
		newUpdateCheckCmd(),
		newVersionCmd(),
	)
	return root
}
