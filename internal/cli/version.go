package cli

import (
	"io"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
)

func init() {
	RootCmd.AddCommand(newVersionCmd())
}

// newVersionCmd prints what --version prints: the identity line
// plus the license banner, so the flag and the command never
// drift apart.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and license banner",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			config.PrintVersion(out)
			_, err := io.WriteString(out, config.LicenseBanner()+"\n")
			return err
		},
	}
}
