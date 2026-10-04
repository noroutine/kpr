package console

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/otel"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server represents the HTTP server
type Server struct {
	Host        string
	Port        int
	OTELEnabled bool
	// Store backs the keeper sections (banner, counters, plan,
	// activity). Nil renders them degraded (red/empty), never 500.
	Store store.Store
	// Registry is probed for the banner. Nil renders unreachable.
	// Reachability lives behind the keeper use cases
	// (keeper.Prober): production passes *registry.Client, tests a
	// stub.
	Registry keeper.Prober
	// Sentinel is read for the same-store proof card: the live
	// generation the registry serves, or unproven. Same production
	// client as Registry (it already speaks sentinel.API), separate
	// port so the card degrades on its own. Nil renders
	// unconfigured.
	Sentinel sentinel.API
	// Edge is the serve-embedded gateway fence. Nil renders the
	// gateway section closed (disabled or unproven), never 500.
	Edge *edge.Gate
	// RegistryURL names the registry endpoint the cards talk
	// about: what the edge forwards to, what the keeper probes.
	// Empty renders no endpoint (unknown, never guessed).
	RegistryURL string
	// EdgeAddr is the edge listen address serve bound (or would
	// bind): shown beside the gateway posture when the edge is
	// open.
	EdgeAddr string
	// Listener, when non-nil, serves on it instead of listening on
	// Host:Port. Tests inject a loopback listener on an ephemeral port;
	// production leaves it nil.
	Listener net.Listener
	// shutdownDone is closed when the shutdown watcher finishes. Tests
	// wait on it to observe shutdown side effects (like error logging)
	// without racing the watcher goroutine.
	shutdownDone chan struct{}
	server       *http.Server
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.indexHandler)
	mux.HandleFunc("/metrics", MetricsHandler)
	mux.HandleFunc("/health", HealthHandler)
	mux.HandleFunc("/api/activity", s.activityHandler)
	if s.OTELEnabled {
		// Prometheus exposition for the OTel meter provider;
		// scraped by Prometheus, not linked from the console UI.
		mux.Handle("/metrics/prometheus", promhttp.Handler())
	}

	// Wrap with OTEL middleware if enabled. RequestTelemetry sits
	// inside HTTPMiddleware so the span context (trace/span IDs) is
	// already in the request context when the access log is written.
	var handler http.Handler = mux
	handler = otel.RequestTelemetry(handler, s.OTELEnabled)
	handler = otel.HTTPMiddleware(handler, "management-console", s.OTELEnabled)

	// Use net.JoinHostPort to properly handle IPv6 addresses with brackets
	addr := net.JoinHostPort(s.Host, fmt.Sprintf("%d", s.Port))

	// Timeouts come from the resolved config (see internal/config),
	// not literals here.
	cfg := config.Current()

	// Configure server for both IPv4 and IPv6
	s.server = &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  cfg.HTTPReadTimeout,
		WriteTimeout: cfg.HTTPWriteTimeout,
		IdleTimeout:  cfg.HTTPIdleTimeout,
	}

	// Listen on the specified address, unless a listener was injected.
	listener := s.Listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to listen on %s: %w", addr, err)
		}
	}

	log.Printf("Starting %s", config.VersionString())
	log.Printf("Management console: http://localhost:%d", s.Port)

	// Handle graceful shutdown
	s.shutdownDone = make(chan struct{})
	go func() {
		defer close(s.shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Management console shutdown error: %v", err)
		}
	}()

	if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}
