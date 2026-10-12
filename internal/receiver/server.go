package receiver

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/otel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Server represents the registry receiver HTTP server: /events and
// nothing else. The app server keeps the landing page and
// /api/status; the console keeps the dashboard. Registry coupling
// lives here alone, so topologies that must not expose /events
// alongside user- or management-facing ports simply do not publish
// this one.
type Server struct {
	Host        string
	Port        int
	OTELEnabled bool
	// Store backs the notification receiver. Nil disables /events
	// (503); production always wires the keeper store.
	Store store.Store
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

// Start starts the receiver HTTP server on the configured port.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// Distribution notification receiver (-> tracked rows). The only
	// route: anything else 404s, so a miswired registry or a curious
	// browser learns nothing here.
	mux.HandleFunc("/events", EventsHandler(s.Store))

	// No request counter here: the console's Requests metric counts
	// what comes via the app port (landing page, /api/status).
	// Receiver traffic gets its own counters when it needs them —
	// see docs/RECEIVER.md.
	var handler http.Handler = mux
	handler = otel.RequestTelemetry(handler, s.OTELEnabled)
	handler = otel.HTTPMiddleware(handler, "receiver", s.OTELEnabled)

	// Use net.JoinHostPort to properly handle IPv6 addresses
	addr := net.JoinHostPort(s.Host, fmt.Sprintf("%d", s.Port))

	// Timeouts come from the resolved config (see internal/config),
	// not literals here.
	cfg := config.Current()

	// Configure server
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

	log.Printf("Starting receiver server")
	// The socket names itself: configured host, ephemeral port,
	// injected listener — the log reads what is actually bound.
	log.Printf("Receiver: http://%s/events", listener.Addr())

	// Handle graceful shutdown
	s.shutdownDone = make(chan struct{})
	go func() {
		defer close(s.shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Receiver server shutdown error: %v", err)
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
