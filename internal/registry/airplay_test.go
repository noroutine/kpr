package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// macOS AirPlay Receiver squats localhost:5000 answering 403 with
// Server: AirTunes/... — the same socket our dev registry wants.
// DetectAirPlay sniffs that header (on the registry's /v2/ base route)
// so serve and make/just can name the squatter instead of failing
// cryptically. If this fails, the probe misreads the only fingerprint
// AirPlay leaves.
func TestDetectAirPlay(t *testing.T) {
	var gotPath string
	airplay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Server", "AirTunes/623.2.3")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer airplay.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx/1.25.0")
		w.WriteHeader(http.StatusOK)
	}))
	defer plain.Close()

	ctx := context.Background()
	if !DetectAirPlay(ctx, airplay.URL) {
		t.Errorf("DetectAirPlay(airplay) = false, want true")
	}
	if gotPath != "/v2/" {
		t.Errorf("probe path = %q, want /v2/", gotPath)
	}
	if !DetectAirPlay(ctx, airplay.URL+"/") {
		t.Errorf("DetectAirPlay(airplay with trailing slash) = false, want true")
	}
	if DetectAirPlay(ctx, plain.URL) {
		t.Errorf("DetectAirPlay(plain registry) = true, want false")
	}
	if DetectAirPlay(ctx, "http://127.0.0.1:1") {
		t.Errorf("DetectAirPlay(closed port) = true, want false")
	}
	if DetectAirPlay(ctx, "://bogus") {
		t.Errorf("DetectAirPlay(malformed URL) = true, want false")
	}
}
