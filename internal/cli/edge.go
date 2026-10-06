package cli

import (
	"log"
	"net/http"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/fencing"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

// buildEdge proves the edge addressing and wraps the transparent
// proxy in the HOLD/DENY gate without binding: proof failure
// returns the error for a loud skip (no proof, no edge), the
// listener stays a thin tail in serve. The gate reads the lock
// marker per mutating request and the HOLD lease off the shared
// file store — no shared file store means marker-only fencing
// (DENY works over any backend; HOLD needs the lease file).
func buildEdge(st fence.GateStore, backendURL, configPath, holdDir string, report fence.Reporter) (*fencing.Gate, http.Handler, error) {
	h, err := openEdge(backendURL, configPath, log.Printf)
	if err != nil {
		return nil, nil, err
	}
	gate := &fencing.Gate{Store: st, Dir: holdDir, Report: report}
	return gate, gate.Wrap(h), nil
}

// assembleEdge is serve's edge wiring, factored for test: the HOLD
// lease dir follows the backend (the file store's dir, nothing on
// redis — HOLD needs the lease file), and proof refusal comes back
// as nils for the loud skip. If this fails, HOLD leases land in the
// wrong dir, or serve boots an unfenced edge thinking it proved one.
func assembleEdge(cfg *config.Config, backend, storeDir string, st fence.GateStore, configPath string, report fence.Reporter) (*fencing.Gate, http.Handler, error) {
	holdDir := ""
	if backend == "file" {
		holdDir = storeDir
	}
	return buildEdge(st, cfg.RegistryURL, configPath, holdDir, report)
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
