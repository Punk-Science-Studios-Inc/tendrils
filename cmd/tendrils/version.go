package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/buildinfo"
)

// newVersionCmd prints the full build identity. Cobra's --version flag (set up
// in newRootCmd) covers the one-line case; this is the form worth pasting into a
// bug report, and the one to compare across devices when a relay appears to be
// dropping a device's publishes.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprint(cmd.OutOrStdout(), buildinfo.Get().Detail("tendrils"))
		},
	}
}
