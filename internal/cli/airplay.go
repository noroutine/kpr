package cli

import (
	"context"
	"log"
	"net/url"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/registry"
)

// isLoopbackRegistry reports whether rawURL points at this machine.
// AirPlay Receiver only binds locally, so the squat warning is only
// meaningful for loopback peers.
func isLoopbackRegistry(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// warnIfAirPlaySquats logs a pointed warning when the configured
// registry peer answers like macOS AirPlay Receiver (which squats
// localhost:5000) instead of a registry. Anything else — a real
// registry, an unreachable peer, a remote host — stays silent: boot
// degrades elsewhere for those.
func warnIfAirPlaySquats(cfg *config.Config) {
	if !isLoopbackRegistry(cfg.RegistryURL) {
		return
	}
	if registry.DetectAirPlay(context.Background(), cfg.RegistryURL) {
		log.Printf("Warning: registry at %s answers like macOS AirPlay Receiver (Server: AirTunes) — AirPlay is squatting localhost:5000; turn it off in System Settings → General → AirDrop & Handoff, then restart the stack", cfg.RegistryURL)
	}
}
