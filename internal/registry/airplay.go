package registry

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// airPlayProbeTimeout bounds the AirPlay squat preflight: a black-holed
// peer must not stall serve boot or an up recipe.
const airPlayProbeTimeout = 2 * time.Second

// DetectAirPlay reports whether baseURL answers like macOS AirPlay
// Receiver — HTTP Server header containing AirTunes on the registry's
// /v2/ base route — instead of a registry. AirPlay squats
// localhost:5000 with a 403, so the check keys on the header, not the
// status. Unreachable or malformed peers report false: only a positive
// fingerprint warns.
func DetectAirPlay(ctx context.Context, baseURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, airPlayProbeTimeout)
	defer cancel()
	endpoint := strings.TrimSuffix(baseURL, "/") + "/v2/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // GET-only loopback preflight, no body parsed
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return strings.Contains(strings.ToLower(resp.Header.Get("Server")), "airtunes")
}
