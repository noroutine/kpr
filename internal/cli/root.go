package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	gccmd "nrtn.dev/catalyst/kpr/internal/cli/gc"
	"nrtn.dev/catalyst/kpr/internal/config"
)

var (
	managementHost string
	managementPort int
	appHost        string
	appPort        int
)

// RootCmd is the root command for kpr
var RootCmd = &cobra.Command{
	Use:   "kpr",
	Short: "kpr - lightweight gateway and keeper for an OCI distribution registry",
	Long: `kpr (keeper) fronts an OCI distribution registry: ephemeral
images and lightweight retention cleanups. Pushes land on kpr's
edge, which forwards them byte-identical and fences mutating
routes; the registry itself stays stock, never forked.

Configuration comes from KPR_* environment variables (see
docs/CONFIG.md); every flag below overrides its matching variable.
Run "kpr env" to list every variable with its effective value.

` + config.LicenseBanner(),
	Version: config.Version,
	// Errors print once, from Execute below — cobra stays silent,
	// so a refusal never echoes as "Error: ..." plus the message.
	SilenceErrors: true,
	// Usage is for mistyped flags, not refusals: flag parsing fails
	// before this runs (usage still prints), while every RunE error
	// returns after it (usage suppressed, error only).
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		cmd.SilenceUsage = true
	},
}

func init() {
	// --version carries the license banner under the identity line.
	RootCmd.SetVersionTemplate(config.VersionString() + "\n" + config.LicenseBanner() + "\n")

	// Flag defaults seed from the environment once, via config — the
	// default value is written down in internal/config, not here.
	defaults := config.NewBuilder().FromEnv().Build()

	// Management console flags (:: for dual-stack IPv4+IPv6)
	RootCmd.PersistentFlags().StringVar(&managementHost, "management-host", defaults.ManagementHost, "Management console host")
	RootCmd.PersistentFlags().IntVar(&managementPort, "management-port", defaults.ManagementPort, "Management console port")

	// Application server flags
	RootCmd.PersistentFlags().StringVar(&appHost, "app-host", defaults.AppHost, "Application server host")
	RootCmd.PersistentFlags().IntVar(&appPort, "app-port", defaults.AppPort, "Application server port")

	// Subpackaged commands register here; each owns its flags,
	// help, and RunE behind its exported Cmd.
	RootCmd.AddCommand(gccmd.Cmd)
}

// Execute runs the root command
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
