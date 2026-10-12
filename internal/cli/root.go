package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
)

var (
	managementHost string
	managementPort int
	appHost        string
	appPort        int
	receiverHost   string
	receiverPort   int
)

// RootCmd is the root command for kpr
var RootCmd = &cobra.Command{
	Use:   "kpr",
	Short: "kpr - lightweight gateway and keeper for an OCI distribution registry",
	Long: `kpr (keeper) fronts an OCI distribution registry: ephemeral
images and lightweight retention cleanups. Pushes land on kpr's
edge, which forwards them byte-identical; the registry itself
stays stock, never forked. Configuration comes from KPR_* variables;
flags override per invocation.

` + config.LicenseBanner(),
	// NOTE(mutants): the Long initializer above emits no cover
	// block of its own, so gremlins reads it NOT COVERED — the
	// effect is pinned instead (TestRootHelpCarriesBanner fails
	// loudly without the banner), whatever the label says.
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

	// Registry receiver flags (registry coupling only — see
	// docs/RECEIVER.md)
	RootCmd.PersistentFlags().StringVar(&receiverHost, "receiver-host", defaults.ReceiverHost, "Registry receiver host")
	RootCmd.PersistentFlags().IntVar(&receiverPort, "receiver-port", defaults.ReceiverPort, "Registry receiver port")

	// Subpackaged commands register here; each owns its flags,
	// help, and RunE behind its exported Cmd.
	RootCmd.AddCommand(Cmd)
}

// Execute runs the root command
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
