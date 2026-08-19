// Command tendrils is the headless sync daemon and CLI for Tendrils. A CLI plus
// logs is the most legible interface possible, which is the whole point: a tool
// you can open the hood on, understand, and repair.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/buildinfo"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
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
	}
	root.AddCommand(
		newKeygenCmd(),
		newEnrollCmd(),
		newStatusCmd(),
		newDaemonCmd(),
		newGCCmd(),
		newRepairCmd(),
		newRetryCmd(),
		newVersionCmd(),
	)
	return root
}
