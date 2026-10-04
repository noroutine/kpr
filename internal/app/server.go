package app

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

// Server represents the application HTTP server
type Server struct {
	Host        string
	Port        int
	OTELEnabled bool
	// Store backs the notification receiver. Nil disables /events
	// (503); production always wires the redis store.
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
	mux := http.NewServeMux()

	// Serve static files
	staticFS, err := GetStaticFS()
	if err != nil {
		return fmt.Errorf("failed to load static files: %w", err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(staticFS)))

	// Serve index.html at root, templated with the app base path so
	// the stylesheet href survives a stripped subpath prefix. Routes
	// stay put — /events never moves for a UI reason. The template
	// parses once at init (a broken embed fails the boot, not each
	// request); the only per-request failure left is the execute,
	// which a static page with one string key never produces.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := indexTmpl.Execute(w, map[string]string{"BasePath": config.Current().AppBasePath}); err != nil {
				log.Printf("Error executing index.html: %v", err)
			}
		} else {
			http.NotFound(w, r)
		}
	})

	// Machine-readable expected state (ok/degraded + store); the
	// landing page carries no state.
	mux.HandleFunc("/api/status", StatusHandler(s.Store))

	// Distribution notification receiver (-> tracked rows).
	mux.HandleFunc("/events", EventsHandler(s.Store))

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
	log.Printf("Application: http://localhost:%d", s.Port)

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
