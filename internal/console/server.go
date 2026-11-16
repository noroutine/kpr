package console

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/otel"
	"nrtn.dev/catalyst/kpr/internal/resources"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server represents the HTTP server
type Server struct {
	Host        string
	Port        int
	OTELEnabled bool
	// TLSConfig, when set, serves the console over HTTPS. Serve
	// builds it from the KPR_TLS_BUNDLE pair; half a pair
	// refuses the boot there, never here.
	TLSConfig *tls.Config
	// Listener, when non-nil, serves on it instead of listening on
	// Host:Port. Tests inject a loopback listener on an ephemeral port;
	// production leaves it nil.
	Listener net.Listener
	// Ops serves operator actions stripped at /ops. The prefix
	// stays mounted with or without it: nil (hand-wired servers,
	// tests) answers 503 with the reason instead of 404ing from
	// the catch-all, so a dashboard link never leads nowhere.
	// The handler owns its auth — the console never sees the
	// operator token.
	Ops http.Handler
	// Token is the console password (KPR_CONSOLE_TOKEN),
	// compared constant-time against HTTP Basic user "ops".
	Token string
	// AuthEnabled gates the whole surface behind Token, except
	// /health (probes carry no credentials). False leaves the
	// console open: dev default, tokenless deployments, and
	// every test that wires no token.
	AuthEnabled bool
	// Now scripts uptime, the server clock, and the clock-card
	// cache stamp; nil reads the wall. Hand-wired servers and
	// tests leave it nil and keep today's behavior.
	Now func() time.Time
	// Resources is the live config holder: the dashboard lists
	// its bundles and the reload endpoint swaps it. Nil (tests)
	// hides the section and refuses reloads.
	Resources *resources.Holder
	// reloadSeqMu serializes the Check-then-Swap sequence: two
	// concurrent reload POSTs must not interleave, or the older
	// snapshot can land last.
	reloadSeqMu sync.Mutex
	// clockMu guards the memoized clock card: one probe per
	// TTL window, not one per dashboard render — a down time
	// server must not stall every page load up to the probe
	// bound. Keyed by the full clock config; local never
	// probes, so the cache only ever holds NTP/HTTPS readings.
	// clockSource scripts the transport in tests (a counting
	// stub); production leaves it nil and the method builds
	// its own.
	clockSource    clock.Source
	clockMu        sync.Mutex
	clockKey       string
	clockAt        time.Time
	clockCardCache keeper.ClockCard
	clockHit       bool
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
	Edge *fence.Gate
	// RegistryURL names the registry endpoint the cards talk
	// about: what the edge forwards to, what the keeper probes.
	// Empty renders no endpoint (unknown, never guessed).
	RegistryURL string
	// EdgeAddr is the edge listen address serve bound (or would
	// bind): shown beside the gateway posture when the edge is
	// open.
	EdgeAddr string
	// shutdownDone is closed when the shutdown watcher finishes. Tests
	// wait on it to observe shutdown side effects (like error logging)
	// without racing the watcher goroutine.
	shutdownDone chan struct{}
	server       *http.Server
}

// clockTTL bounds one memoized clock reading: skew is display,
// never a gate, so a minute-old number is fine and a
// per-render probe is not.
const clockTTL = time.Minute

// now reads the server clock (wall when unwired): every
// rendered age, uptime, clock, and cache stamp in this package flows
// through here, so tests script one field and pin them all.
func (s *Server) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context) error {
	// Auth without a token would compare empty against
	// empty: refuse before binding, never serve authless by
	// miswiring. Serve rejects this combination first; this
	// is the backstop for hand-wired servers.
	if s.AuthEnabled && s.Token == "" {
		return fmt.Errorf("console auth enabled without a token: refusing to serve authless")
	}
	assets, err := staticFS()
	if err != nil {
		return fmt.Errorf("console static files: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.indexHandler)
	mux.Handle("/_app/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("/favicon.ico", faviconHandler)
	mux.HandleFunc("/metrics", s.metricsHandler)
	mux.HandleFunc("/health", HealthHandler)
	mux.HandleFunc("/api/overview", s.overviewHandler)
	mux.HandleFunc("/api/activity", s.activityHandler)
	mux.HandleFunc("/api/config/reload", s.reloadHandler)
	if s.OTELEnabled {
		// Prometheus exposition for the OTel meter provider;
		// scraped by Prometheus, not linked from the console UI.
		mux.Handle("/metrics/prometheus", promhttp.Handler())
	}
	// The operator prefix is always mounted: without a handler
	// the prefix answers its own unavailability instead of
	// falling through to the catch-all, so every server —
	// wired or hand-built — answers /ops/ honestly.
	mux.Handle("/ops/", http.StripPrefix("/ops", s.opsHandler()))

	// Auth gates everything but the health probe: probes carry
	// no credentials, and everything else carries the token.
	var handler http.Handler = mux
	if s.AuthEnabled {
		handler = s.requireAuth(handler)
	}

	// Wrap with OTEL middleware if enabled. RequestTelemetry sits
	// inside HTTPMiddleware so the span context (trace/span IDs) is
	// already in the request context when the access log is written.
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

	// The socket names itself: configured host, ephemeral port,
	// injected listener — the log reads what is actually bound.
	// (The version line already opened the boot log in serve, so a
	// second one here would read twice.)
	scheme := "http"
	if s.TLSConfig != nil {
		scheme = "https"
	}
	log.Printf("Management console: %s://%s", scheme, listener.Addr())

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
