package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
)

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Show environment variables and their effective values",
	Long: `Print every KPR_*/OTEL_* environment variable kpr reads, with
its description and the value currently in effect (explicit setting
or default). The serve flags override these per invocation; see
docs/CONFIG.md for the layering rules.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		out := cmd.OutOrStdout()
		if _, err := fmt.Fprintln(out, "kpr environment (effective values):"); err != nil {
			return err
		}
		for _, v := range config.EnvVars {
			if _, err := fmt.Fprintf(out, "\n  %s=%s\n    %s\n", v.Name, envValue(cfg, v.Name), v.Description); err != nil {
				return err
			}
		}
		return nil
	},
}

// envValue renders one variable's resolved value for display. It must
// cover every config.EnvVars entry — an unknown name renders empty,
// which TestEnvCommandListsEveryVar would catch only by name; keep the
// cases in sync with internal/config when adding variables.
func envValue(cfg *config.Config, name string) string {
	switch name {
	case config.EnvManagementHost:
		return cfg.ManagementHost
	case config.EnvManagementPort:
		return strconv.Itoa(cfg.ManagementPort)
	case config.EnvAppHost:
		return cfg.AppHost
	case config.EnvAppPort:
		return strconv.Itoa(cfg.AppPort)
	case config.EnvRedisAddr:
		return cfg.RedisAddr
	case config.EnvRegistryURL:
		return cfg.RegistryURL
	case config.EnvNoDryRun:
		return strconv.FormatBool(cfg.NoDryRun)
	case config.EnvOTELEnabled:
		return strconv.FormatBool(cfg.OTELEnabled)
	case config.EnvOTELEndpoint:
		return cfg.OTLPEndpoint
	case config.EnvOTELServiceName:
		return cfg.OTELServiceName
	case config.EnvOTELServiceVersion:
		return cfg.OTELServiceVersion
	case config.EnvOTELEnvironment:
		return cfg.OTELEnvironment
	case config.EnvQuickwitURL:
		return cfg.QuickwitURL
	case config.EnvJaegerURL:
		return cfg.JaegerURL
	case config.EnvGrafanaURL:
		return cfg.GrafanaURL
	case config.EnvPrometheusURL:
		return cfg.PrometheusURL
	default:
		return ""
	}
}

func init() {
	RootCmd.AddCommand(envCmd)
}
