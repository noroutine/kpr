package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// AirPlay only binds locally, so the squat warning only makes sense for
// loopback registry peers — a remote host answering AirTunes is not our
// problem to diagnose. If this fails, the gate lets a remote probe
// through (slow boot, wrong advice) or skips a local one (silent
// squat).
func TestIsLoopbackRegistry(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://localhost:5000":      true,
		"http://127.0.0.1:5000":      true,
		"http://[::1]:5000":          true,
		"http://registry.local:5000": false,
		"http://10.0.0.5:5000":       false,
		"http://example.com:5000":    false,
		"":                           false,
		"://bogus":                   false,
	} {
		if got := isLoopbackRegistry(raw); got != want {
			t.Errorf("isLoopbackRegistry(%q) = %v, want %v", raw, got, want)
		}
	}
}

// The serve boot must name the Apple squatter: registry peer answering
// AirTunes → a warning with the off-switch location; a real registry
// or an unreachable peer → silence. If this fails, boot either stays
// mute on the most common macOS failure or cries wolf on healthy
// stacks.
func TestWarnIfAirPlaySquats(t *testing.T) {
	airplay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "AirTunes/623.2.3")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer airplay.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer plain.Close()

	withRegistryURL := func(t *testing.T, url string) *config.Config {
		t.Helper()
		t.Setenv("KPR_REGISTRY_URL", url)
		return config.NewBuilder().FromEnv().Build()
	}

	t.Run("airplay warns", func(t *testing.T) {
		logs := captureLog(t)
		warnIfAirPlaySquats(withRegistryURL(t, airplay.URL))
		out := logs.String()
		if !strings.Contains(out, "AirPlay") {
			t.Errorf("no AirPlay warning for squatted peer, logs:\n%s", out)
		}
		if !strings.Contains(out, "AirDrop") {
			t.Errorf("warning names no off-switch, logs:\n%s", out)
		}
	})
	t.Run("plain registry silent", func(t *testing.T) {
		logs := captureLog(t)
		warnIfAirPlaySquats(withRegistryURL(t, plain.URL))
		if out := logs.String(); strings.Contains(out, "AirPlay") {
			t.Errorf("healthy peer warned about AirPlay, logs:\n%s", out)
		}
	})
	t.Run("unreachable silent", func(t *testing.T) {
		logs := captureLog(t)
		warnIfAirPlaySquats(withRegistryURL(t, "http://127.0.0.1:1"))
		if out := logs.String(); strings.Contains(out, "AirPlay") {
			t.Errorf("unreachable peer warned about AirPlay, logs:\n%s", out)
		}
	})
	t.Run("remote skipped", func(t *testing.T) {
		logs := captureLog(t)
		warnIfAirPlaySquats(withRegistryURL(t, "http://registry.example.invalid:5000"))
		if out := logs.String(); strings.Contains(out, "AirPlay") {
			t.Errorf("remote peer warned about AirPlay, logs:\n%s", out)
		}
	})
}
