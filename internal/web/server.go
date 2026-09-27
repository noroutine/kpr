package web

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/otel"
)

// Server represents the HTTP server
type Server struct {
	Host        string
	Port        int
	OTELEnabled bool
	server      *http.Server
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", IndexHandler)
	mux.HandleFunc("/metrics", MetricsHandler)
	mux.HandleFunc("/health", HealthHandler)

	// Wrap with OTEL middleware if enabled
	var handler http.Handler = mux
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

	// Listen on the specified address
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	log.Printf("Starting %s", config.VersionString())
	log.Printf("Management console: http://localhost:%d", s.Port)

	// Handle graceful shutdown
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Management console shutdown error: %v", err)
		}
	}()

	err = s.server.Serve(listener)
	if err != nil && err != http.ErrServerClosed {
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
