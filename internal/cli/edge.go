package cli

import (
	"log"
	"net/http"

	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

// serveConfigPath is the registry config file the edge
// RelativeURLs proof reads (serve --config).
var serveConfigPath string

// buildEdge proves the edge addressing and wraps the transparent
// proxy in the HOLD/DENY gate without binding: proof failure
// returns the error for a loud skip (no proof, no edge), the
// listener stays a thin tail in serve. The gate reads the lock
// marker per mutating request and the HOLD lease off the shared
// file store — no shared file store means marker-only fencing
// (DENY works over any backend; HOLD needs the lease file).
func buildEdge(st edge.GateStore, backendURL, configPath, holdDir string, report gc.Reporter) (*edge.Gate, http.Handler, error) {
	h, err := openEdge(backendURL, configPath, log.Printf)
	if err != nil {
		return nil, nil, err
	}
	gate := &edge.Gate{Store: st, Dir: holdDir, Report: report}
	return gate, gate.Wrap(h), nil
}

// openEdge proves RelativeURLs over the registry config and builds
// the transparent proxy handler: the refusal paths stay testable,
// the listener stays a thin tail.
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
	serveCmd.Flags().StringVar(&serveConfigPath, "config", "/etc/distribution/config.yml", "Registry config file the edge RelativeURLs proof reads")
}
