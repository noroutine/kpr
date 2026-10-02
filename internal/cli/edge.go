package cli

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

var edgeAddrFlag string
var edgeConfigPath string

var edgeCmd = &cobra.Command{
	Use:   "edge",
	Short: "Serve the registry edge proxy",
	Long: `Listen on the edge address and forward the registry API to
KPR_REGISTRY_URL, byte-identical, with upstream Location headers
guarded (relative passes, absolute rewrites loudly). Opens only
on a RelativeURLs proof over the registry config: no proof, no
edge. Slice 1 forwards everything, zero policy — DELETEs pass
straight through, the sweeper stays the sole delete owner.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		addr := edgeAddrFlag
		if addr == "" {
			addr = d.cfg.EdgeAddr
		}
		h, err := openEdge(d.cfg.RegistryURL, edgeConfigPath, log.Printf)
		if err != nil {
			return err
		}
		// Slice 2: the gate reads the marker per mutating request
		// and the HOLD lease off the shared file store. No shared
		// file store means marker-only fencing (DENY works over
		// any backend; HOLD needs the lease file).
		holdDir := ""
		if backend, dir, berr := resolveStoreBackend(); berr == nil && backend == "file" {
			holdDir = dir
		}
		gate := &edge.Gate{
			Store: d.store,
			Dir:   holdDir,
			Report: func(e gc.Event) {
				log.Printf("edge fence: %s %s", e.Stage, e.Message)
			},
		}
		srv := &http.Server{Addr: addr, Handler: gate.Wrap(h)}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		fmt.Printf("edge on %s -> %s\n", addr, d.cfg.RegistryURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	},
}

// openEdge proves the edge addressing and builds the handler
// without binding: the refusal paths stay testable, the
// listener stays a thin tail. Slice 2 wraps this handler with
// HOLD/DENY fencing.
func openEdge(backend, configPath string, logf func(string, ...any)) (http.Handler, error) {
	proven, err := proof.ProveRelativeURLs(configPath)
	if err != nil {
		return nil, err
	}
	p, err := edge.New(backend)
	if err != nil {
		return nil, err
	}
	p.Logf = logf
	h, err := p.Handler(proven)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func init() {
	edgeCmd.Flags().StringVar(&edgeAddrFlag, "edge-addr", "", "Edge listen address (default KPR_EDGE_ADDR, :5000)")
	edgeCmd.Flags().StringVar(&edgeConfigPath, "config", "/etc/distribution/config.yml", "Registry config file the RelativeURLs proof reads")
	RootCmd.AddCommand(edgeCmd)
}
