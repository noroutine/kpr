package web

import (
	"encoding/json"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"nrtn.dev/catalyst/kpr/internal/app"
	"nrtn.dev/catalyst/kpr/internal/config"
)

var (
	// Metrics
	startTime = time.Now()
)

type pageData struct {
	Hostname  string
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
	// Links holds the configured observability launchpad entries. A
	// link appears only when its URL is configured — empty means the
	// backend is absent and the template hides the whole section.
	Links []consoleLink
	// Keeper is what kpr tracks (banner, counters, plan, activity).
	Keeper keeperData
	// Store names the state backend and where it lives; Sentinel
	// carries the live same-store proof generation, if any.
	Store    storeData
	Sentinel sentinelData
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

// consoleLink is one observability UI entry on the console.
type consoleLink struct {
	Name  string
	URL   string
	Blurb string
}

// observabilityLinks resolves the configured UI base URLs into
// launchpad entries, skipping every backend without a URL.
func observabilityLinks(cfg *config.Config) []consoleLink {
	var links []consoleLink
	add := func(url, name, blurb string) {
		if url != "" {
			links = append(links, consoleLink{Name: name, URL: url, Blurb: blurb})
		}
	}
	add(cfg.QuickwitURL, "Quickwit", "Searchable logs, kept across dev runs")
	add(cfg.JaegerURL, "Jaeger", "Distributed traces, stored in Quickwit")
	add(cfg.GrafanaURL, "Grafana", "Metrics dashboards for this service")
	add(cfg.PrometheusURL, "Prometheus", "Raw metrics and scrape targets")
	return links
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

	keeper := s.keeperSnapshot(r.Context())
	sentinel := s.sentinelSnapshot(r.Context())

	data := pageData{
		Hostname:       hostname,
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
		Links:          observabilityLinks(cfg),
		Keeper:         keeper,
		Store:          storeSnapshot(r.Context(), s.Store, keeper.RedisOK, cfg, sentinel.Proven),
		Sentinel:       sentinel,
	}

	tmpl, err := template.New("index").Parse(indexTemplate)
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Error executing template: %v", err)
	}
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
		Uptime:        uptime.Round(time.Second).String(),
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
