package cli

import (
	"github.com/spf13/cobra"
)

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Inspect the distribution registry itself",
}

func init() {
	RootCmd.AddCommand(registryCmd)
}
