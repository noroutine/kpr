package app

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/otel"
)

// Server represents the application HTTP server
type Server struct {
	Host        string
	Port        int
	OTELEnabled bool
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

	// Serve index.html at root
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			file, err := staticFS.Open("index.html")
			if err != nil {
				http.Error(w, "Not found", http.StatusNotFound)
				return
			}
			defer func() {
				if err := file.Close(); err != nil {
					log.Printf("Error closing index.html: %v", err)
				}
			}()

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeContent(w, r, "index.html", time.Time{}, file)
		} else {
			http.NotFound(w, r)
		}
	})

	// API endpoints
	mux.HandleFunc("/api/hello", HelloHandler)
	mux.HandleFunc("/api/data", DataHandler)

	// Wrap with OTEL middleware if enabled
	var handler http.Handler = mux
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
