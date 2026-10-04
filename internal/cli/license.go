package cli

import (
	"io"
	"log"

	"github.com/spf13/cobra"
	kpr "nrtn.dev/catalyst/kpr"
	"nrtn.dev/catalyst/kpr/internal/config"
)

// logLicenseBanner logs the GPL short notice; serve calls it first at
// startup so docker logs carry the warranty/refusal grant.
func logLicenseBanner() {
	log.Print(config.LicenseBanner())
}

func init() {
	RootCmd.AddCommand(newLicenseCmd())
}

// newLicenseCmd prints the embedded GPL text: the license travels
// with the binary, so containers and air-gapped registries carry it
// even where the repo checkout isn't around.
func newLicenseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "license",
		Short: "Print the GPLv3 license",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), kpr.Text())
			return err
		},
	}
}
