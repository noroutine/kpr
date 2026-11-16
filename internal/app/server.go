package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/otel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Server represents the application HTTP server
type Server struct {
	Host        string
	Port        int
	OTELEnabled bool
	// TLSConfig, when set, serves the app over HTTPS. Serve
	// builds it from the KPR_TLS_BUNDLE pair; half a pair
	// refuses the boot there, never here.
	TLSConfig *tls.Config
	// Store backs /api/status. Nil renders it unbacked (but
	// still 200); production always wires the keeper store.
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

// Start starts the application HTTP server on port 8080
func (s *Server) Start(ctx context.Context) error {
	assets := staticFS()
	mux := http.NewServeMux()

	// Built SPA shell at root, hashed assets under /_app/.
	mux.HandleFunc("/", indexHandler)
	mux.Handle("/_app/", http.FileServer(http.FS(assets)))

	// Tab icon browsers request unprompted — same mark as the console.
	mux.HandleFunc("/favicon.ico", faviconHandler)

	// Machine-readable expected state (ok/degraded + store); the
	// landing page fetches it at runtime.
	mux.HandleFunc("/api/status", StatusHandler(s.Store))

	// /events lives on the receiver's own port (see
	// internal/receiver): this server answers landing page and
	// status only.

	// Count every served request for the console's request metric,
	// then the OTEL middleware if enabled. RequestTelemetry sits
	// inside HTTPMiddleware so the span context (trace/span IDs) is
	// already in the request context when the access log is written.
	handler := CountRequests(mux)
	handler = otel.RequestTelemetry(handler, s.OTELEnabled)
	handler = otel.HTTPMiddleware(handler, "application", s.OTELEnabled)

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
	if s.TLSConfig != nil {
		s.server.TLSConfig = s.TLSConfig
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

	log.Printf("Starting app server")
	// The socket names itself: configured host, ephemeral port,
	// injected listener — the log reads what is actually bound.
	scheme := "http"
	if s.TLSConfig != nil {
		scheme = "https"
	}
	log.Printf("Application: %s://%s", scheme, listener.Addr())

	// Handle graceful shutdown
	s.shutdownDone = make(chan struct{})
	go func() {
		defer close(s.shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Application server shutdown error: %v", err)
		}
	}()

	serve := s.server.Serve
	if s.TLSConfig != nil {
		serve = func(l net.Listener) error { return s.server.ServeTLS(l, "", "") }
	}
	if err := serve(listener); err != nil && err != http.ErrServerClosed {
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
