package cli

import (
	"log"
	"net/http"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/event"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

// buildEdge proves the edge addressing and wraps the transparent
// proxy in the HOLD/DENY gate without binding: proof failure
// returns the error for a loud skip (no proof, no edge), the
// listener stays a thin tail in serve. The gate reads the lock
// marker per mutating request and the HOLD lease through the
// store's advertised capability — no capability means
// marker-only fencing (DENY works over any backend).
func buildEdge(st fence.GateStore, backendURL, configPath string, lease fence.Lease, report event.Reporter) (*fence.Gate, http.Handler, error) {
	h, err := openEdge(backendURL, configPath, log.Printf)
	if err != nil {
		return nil, nil, err
	}
	gate := &fence.Gate{Store: st, Lease: lease, Report: report}
	return gate, gate.Wrap(h), nil
}

// assembleEdge is serve's edge wiring, factored for test: the HOLD
// lease follows the store's advertised capability (its dir, its
// redis conn, or nothing), never a backend string — and proof
// refusal comes back as nils for the loud skip. If this fails,
// HOLD leases land in the wrong medium, or serve boots an
// unfenced edge thinking it proved one.
func assembleEdge(cfg *config.Config, backend string, st fence.GateStore, configPath string, report event.Reporter) (*fence.Gate, http.Handler, error) {
	return buildEdge(st, cfg.RegistryURL, configPath, fence.LeaseForStore(st), report)
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
