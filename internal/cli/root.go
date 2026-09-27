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
)

// RootCmd is the root command for kpr
var RootCmd = &cobra.Command{
	Use:   "kpr",
	Short: "kpr - lightweight companion for an OCI distribution registry",
	Long: `kpr (keeper) is a lightweight companion sidecar for an OCI
distribution registry: ephemeral images and lightweight retention
cleanups.

Configuration comes from KPR_* environment variables (see
docs/CONFIG.md); every flag below overrides its matching variable.
Run "kpr env" to list every variable with its effective value.`,
	Version: config.Version,
}

func init() {
	// Flag defaults seed from the environment once, via config — the
	// default value is written down in internal/config, not here.
	defaults := config.NewBuilder().FromEnv().Build()

	// Management console flags (:: for dual-stack IPv4+IPv6)
	RootCmd.PersistentFlags().StringVar(&managementHost, "management-host", defaults.ManagementHost, "Management console host")
	RootCmd.PersistentFlags().IntVar(&managementPort, "management-port", defaults.ManagementPort, "Management console port")

	// Application server flags
	RootCmd.PersistentFlags().StringVar(&appHost, "app-host", defaults.AppHost, "Application server host")
	RootCmd.PersistentFlags().IntVar(&appPort, "app-port", defaults.AppPort, "Application server port")
}

// Execute runs the root command
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
