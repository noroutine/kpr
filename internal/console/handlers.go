package console

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"nrtn.dev/catalyst/kpr/internal/app"
	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/helpers/human"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

var (
	// Metrics
	startTime = time.Now()
)

type pageData struct {
	Hostname string
	// BasePath prefixes console hrefs (KPR_CONSOLE_BASE_PATH,
	// normalized): empty renders the historic root-relative hrefs,
	// "/kpr" renders "/kpr/api/..." behind a stripped prefix.
	BasePath  string
	Version   string
	Commit    string
	BuildTime string
	Uptime    string
	Requests  uint64
	GoVersion string
	// GoOSArch names the platform this binary was built for
	// (GOOS/GOARCH from the runtime config).
	GoOSArch       string
	ManagementHost string
	ManagementPort string
	// ManagementAddr is the bracketed host:port display form —
	// "::" renders as "[::]:9300", never ":::9300".
	ManagementAddr string
	AppHost        string
	AppPort        string
	// AppAddr is the bracketed host:port display form, as above.
	AppAddr string
	// AppStatusURL is the absolute /api/status link: the endpoint
	// lives on the app server (another port), so a relative href
	// would 404 against the console. Built off the request host —
	// the only address known dialable from this browser.
	AppStatusURL string
	// Obs holds the configured observability launchpad entries —
	// the same card `status observability` prints. A link appears
	// only when its URL is configured; an empty card hides the
	// whole section.
	Obs keeper.ObsCard
	// Registry voices the probed distribution registry with its
	// endpoint — the same card `status registry` prints.
	Registry keeper.RegistryCard
	// Status is what kpr tracks (banner, counters) — the same
	// model `status` reads. Plan and Activity are its
	// template-shaped views; ActivityTotal is the uncapped ring
	// size, so the cap reads honest.
	Status        keeper.Status
	Plan          []keeperPlanRow
	Activity      []keeperActivityRow
	ActivityTotal int
	// Edge is the shared fence posture card: open, deny, or
	// hold with the endpoints beside it. What IS, while the
	// activity ring shows what CHANGED. The section and the
	// card are named edge; the card's own label says gateway —
	// the role the edge plays in front of the registry.
	Edge keeper.EdgeCard
	// Clock is the shared time-transport card: method, server,
	// live skew, bound. ClockNow is the server clock at render
	// (what "now" means to kpr) — stamped here, never shared:
	// the skew reading is taken during the same render, so a
	// separate checked stamp would always read "just now" —
	// noise, not information. (The proof itself carries no
	// obtained-at.)
	Clock    keeper.ClockCard
	ClockNow string
	// Store names the state backend and where it lives; Sentinel
	// carries the live same-store proof generation, if any.
	Store    keeper.StoreCard
	Sentinel keeper.ProofCard
}

// bulbClass paints a card's left border from its standing: deny
// red, hold and degraded amber, ok teal. Anything else gets no
// class (the default border) rather than a wrong color — the
// words carry those states. The template calls it; the CLI
// voices the same rule in words beside its own cards.
func bulbClass(standing keeper.Standing) string {
	switch standing {
	case keeper.StandingDenied:
		return "bulb-deny"
	case keeper.StandingHold, keeper.StandingDegraded:
		return "bulb-warn"
	case keeper.StandingOK:
		return "bulb-ok"
	default:
		return ""
	}
}

// bracketHost renders a bind address for display: a bare IPv6 host
// (containing ':') gets brackets so the console shows "[::]:9300",
// never ":::9300". Already-bracketed and plain hostnames pass through.
func bracketHost(host string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]"
	}
	return host
}

// hostOnly strips the port off a request host ("h:9300" → "h",
// "[::1]:9300" → "::1"): the app-status link keeps the browser's
// host and swaps the port. Empty stays empty — the template then
// renders no link, never a malformed one.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(hostport, "[]")
}

// appStatusURL is the absolute /api/status href off the request
// host (the only address known dialable from this browser) with
// the app port swapped in. Empty without a host — the template
// then renders text, never a malformed link.
func appStatusURL(hostport string, appPort int) string {
	host := hostOnly(hostport)
	if host == "" {
		return ""
	}
	return "http://" + bracketHost(host) + ":" + strconv.Itoa(appPort) + "/api/status"
}

// obsCard resolves the configured UI base URLs into the shared
// launchpad card: the same entries `status observability`
// prints, decided in keeper, never recomputed here.
func obsCard(cfg *config.Config) keeper.ObsCard {
	return keeper.FetchObsCard(keeper.ObsURLs{
		Quickwit: cfg.QuickwitURL, Jaeger: cfg.JaegerURL,
		Grafana: cfg.GrafanaURL, Prometheus: cfg.PrometheusURL,
	})
}

type metricsData struct {
	Uptime        string            `json:"uptime"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Requests      uint64            `json:"requests"`
	GoVersion     string            `json:"go_version"`
	Version       string            `json:"version"`
	Commit        string            `json:"commit"`
	BuildTime     string            `json:"build_time"`
	Environment   map[string]string `json:"environment"`
}

// IndexHandler serves the main management console dashboard.
// It renders with no backends (degraded banner); Server.indexHandler
// renders with this server's keeper wiring.
func IndexHandler(w http.ResponseWriter, r *http.Request) {
	(&Server{}).indexHandler(w, r)
}

func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	hostname, _ := os.Hostname()
	uptime := time.Since(startTime).Round(time.Second)

	// Effective configuration, resolved once at startup (see
	// internal/cli and internal/config) — never re-read from the
	// environment here.
	cfg := config.Current()

	rt := config.CurrentRuntime()

	// The page assembles straight from the shared keeper
	// models — the same implementation `status` reads — with
	// only template shaping in between, degrading to red/empty
	// when a backend is absent or down.
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	st := keeper.FetchStatus(ctx, s.Store, s.Registry)
	sentinel := s.sentinelSnapshot(ctx)
	// Edge posture reads the shared card: would-deny, lease,
	// and standing decided in keeper, never recomputed here.
	var gate keeper.GatePosture
	if s.Edge != nil {
		gate = s.Edge
	}
	now := time.Now().UTC()

	data := pageData{
		Hostname:       hostname,
		BasePath:       cfg.ConsoleBasePath,
		Version:        config.Version,
		Commit:         config.Commit,
		BuildTime:      config.BuildTime,
		Uptime:         uptime.String(),
		Requests:       app.GetAPIRequestCount(),
		GoVersion:      rt.GoVersion,
		GoOSArch:       rt.GOOS + "/" + rt.GOARCH,
		ManagementHost: cfg.ManagementHost,
		ManagementPort: strconv.Itoa(cfg.ManagementPort),
		ManagementAddr: bracketHost(cfg.ManagementHost) + ":" + strconv.Itoa(cfg.ManagementPort),
		AppHost:        cfg.AppHost,
		AppPort:        strconv.Itoa(cfg.AppPort),
		AppAddr:        bracketHost(cfg.AppHost) + ":" + strconv.Itoa(cfg.AppPort),
		AppStatusURL:   appStatusURL(r.Host, cfg.AppPort),
		Obs:            obsCard(cfg),
		Registry:       keeper.NewRegistryCard(s.Registry, st.RegistryOK, cfg.RegistryURL),
		Status:         st,
		Plan:           planRows(st.Plan),
		Activity:       activityRows(st.Activity, now),
		ActivityTotal:  len(st.Activity),
		Edge:           keeper.FetchEdgeCard(ctx, gate, s.Store, s.EdgeAddr, s.RegistryURL),
		Clock:          keeper.FetchClockCard(ctx, clock.NewSource(cfg.TimeMethod), cfg.TimeMethod, cfg.TimeServer, clock.Tolerance),
		ClockNow:       now.Format("2006-01-02 15:04:05 UTC"),
		Store: keeper.FetchStoreCard(ctx, s.Store, st.StoreOK,
			cfg.RedisAddr, cfg.RedisDB, sentinel.Proven()),
		Sentinel: sentinel,
	}

	renderIndex(w, data)
}

// MetricsHandler returns JSON metrics
func MetricsHandler(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	uptime := time.Since(startTime)

	cfg := config.Current()
	env := map[string]string{
		"HOSTNAME":            hostname,
		"KPR_MANAGEMENT_HOST": cfg.ManagementHost,
		"KPR_MANAGEMENT_PORT": strconv.Itoa(cfg.ManagementPort),
		"KPR_APP_HOST":        cfg.AppHost,
		"KPR_APP_PORT":        strconv.Itoa(cfg.AppPort),
		"KPR_REDIS_ADDR":      cfg.RedisAddr,
	}

	metrics := metricsData{
		Uptime:        human.Dur(uptime.Round(time.Second)),
		UptimeSeconds: int64(uptime.Seconds()),
		Requests:      app.GetAPIRequestCount(),
		GoVersion:     config.CurrentRuntime().GoVersion,
		Version:       config.Version,
		Commit:        config.Commit,
		BuildTime:     config.BuildTime,
		Environment:   env,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(metrics); err != nil {
		log.Printf("Error encoding JSON: %v", err)
	}
}

// HealthHandler returns 200 OK if service is healthy
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("OK\n")); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}
