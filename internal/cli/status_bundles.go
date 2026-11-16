package cli

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/console"
	"nrtn.dev/catalyst/kpr/internal/resources"
)

// bundleStatus is the bundle model one `status bundles` prints:
// the judged cards plus the config source they loaded from. The
// dashboard reads the same cards through console.StandBundle, so
// the two never disagree about what a row means.
type bundleStatus struct {
	Bundles []console.BundleCard `json:"bundles"`
	Source  string               `json:"source"`
}

// Lines renders one line per bundle, standing first: the shape
// operators grep. A trust-only bundle names its trust source;
// an identity bundle voices the lifetime with its identity.
func (b bundleStatus) Lines() []string {
	var out []string
	for _, c := range b.Bundles {
		line := c.Standing + " bundle " + c.Name + " " + c.Lifetime
		if c.Identity != "" {
			line += " (" + c.Identity + ")"
		}
		out = append(out, line)
	}
	out = append(out, "source "+b.Source)
	return out
}

var statusBundlesCmd = &cobra.Command{
	Use:   "bundles",
	Short: "Show TLS bundle standings",
	Long: `One line per TLS bundle from the config dir, standing
first. A config dir that will not load refuses instead of
printing guesses.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// Env-resolved directly, never through OpenDeps: bundles
		// need no backend, and a down store must not refuse a
		// config read.
		cfg := config.NewBuilder().FromEnv().Build()
		status, err := collectBundles(cfg)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			return json.NewEncoder(out).Encode(status)
		}
		for _, line := range status.Lines() {
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
		return nil
	},
}

func init() {
	statusBundlesCmd.Flags().Bool("json", false, "Print the bundle cards as JSON for scripts")
	statusCmd.AddCommand(statusBundlesCmd)
}

// collectBundles reads the cards `status bundles` prints: the
// config dir through the resources loader, the serving bundle
// resolved like serve resolves it. A dir that will not load is
// a refusal — cards that need it never invent numbers. Pure
// except for the wall clock, so the matrix is testable without
// binding.
func collectBundles(cfg *config.Config) (bundleStatus, error) {
	status := bundleStatus{Source: "unconfigured"}
	if cfg.ConfigDir == "" && cfg.TLSBundle == "" {
		return status, nil
	}
	snap, err := resources.Load(cfg.ConfigDir)
	if err != nil {
		return bundleStatus{}, err
	}
	status.Source = "KPR_CONFIG_DIR=" + cfg.ConfigDir
	// The serving bundle resolves like serve resolves it: an
	// unknown, parked, or identity-less name refuses instead of
	// printing an https card it cannot back.
	if cfg.TLSBundle != "" {
		if _, _, err := bundleTLSConfig(snap, cfg.TLSBundle); err != nil {
			return bundleStatus{}, err
		}
	}
	now := time.Now()
	for _, e := range resources.SortedEntries(snap.Bundles) {
		var cert *tls.Certificate
		if c, ok := snap.Certs[e.Name]; ok {
			cert = &c
		}
		status.Bundles = append(status.Bundles, console.StandBundle(e.Name, e.Value, cert,
			snap.Floors[e.Name], now, snap.ChainGaps[e.Name]))
	}
	return status, nil
}
