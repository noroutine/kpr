package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/web"
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
	Short: "kpr - Go application blueprint",
	Long: `kpr is a Go application blueprint with embedded management console and application server.
It provides a structure for building Go applications with web UI and API endpoints.`,
	Version: web.Version,
}

func init() {
	// Management console flags (:: for dual-stack IPv4+IPv6)
	RootCmd.PersistentFlags().StringVar(&managementHost, "management-host", getEnv("KPR_MANAGEMENT_HOST", "::"), "Management console host")
	RootCmd.PersistentFlags().IntVar(&managementPort, "management-port", getEnvInt("KPR_MANAGEMENT_PORT", 9300), "Management console port")

	// Application server flags
	RootCmd.PersistentFlags().StringVar(&appHost, "app-host", getEnv("KPR_APP_HOST", "::"), "Application server host")
	RootCmd.PersistentFlags().IntVar(&appPort, "app-port", getEnvInt("KPR_APP_PORT", 8080), "Application server port")
}

// Execute runs the root command
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var intValue int
		if _, err := fmt.Sscanf(value, "%d", &intValue); err == nil {
			return intValue
		}
	}
	return defaultValue
}
