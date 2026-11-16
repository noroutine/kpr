package console

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"math"
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
	"nrtn.dev/catalyst/kpr/internal/resources"
)

var (
	// Metrics
	startTime = time.Now()
)

// planRow is one plan line the SPA renders verbatim: the push
// stamp preformatted, "unknown" when the store never said.
type planRow struct {
	Repo   string `json:"repo"`
	Tag    string `json:"tag"`
	Reason string `json:"reason"`
	Pushed string `json:"pushed"`
}

// planRows shapes the plan for the overview: the push stamp
// preformatted, "unknown" when the store never said. The tail
// is never nil: the SPA reads `.length` off it, and JSON
// renders a nil slice as null, not [].
func planRows(plan []keeper.PlanEntry) []planRow {
	rows := []planRow{}
	for _, p := range plan {
		pushed := p.PushedAt.UTC().Format(time.RFC3339)
		if p.PushedAt.IsZero() {
			pushed = "unknown"
		}
		rows = append(rows, planRow{Repo: p.Repo, Tag: p.Tag, Reason: p.Reason, Pushed: pushed})
	}
	return rows
}

// activityLines shapes the capped ring tail for the overview. It
// mirrors the CLI status view through the shared formatter
// (unpadded): same last-10 tail, capped and said out loud.
func activityLines(entries []keeper.ActivityEntry, now time.Time) []string {
	lines := []string{}
	for _, a := range entries {
		if len(lines) >= keeper.ActivityTail {
			break
		}
		lines = append(lines, keeper.FormatOutcome(now, a, false))
	}
	return lines
}

// overviewData is the dashboard's live model: the SPA at /
// fetches it and renders every card. The keeper cards ride
// verbatim (their JSON tags are the contract); the plan tail,
// the bundle cards, and the last reload refusal ride beside
// them.
type overviewData struct {
	Registry keeper.RegistryCard `json:"registry"`
	Store    keeper.StoreCard    `json:"store"`
	Sentinel keeper.ProofCard    `json:"sentinel"`
	Edge     keeper.EdgeCard     `json:"edge"`
	Clock    keeper.ClockCard    `json:"clock"`
	// ClockNow is the server clock at render (what "now" means
	// to kpr).
	ClockNow string `json:"clockNow"`
	// GoVersion and GoOSArch name the toolchain and platform
	// this binary was built with (from the runtime config).
	GoVersion string `json:"goVersion"`
	GoOSArch  string `json:"goOSArch"`
	// ManagementAddr, AppAddr, and ReceiverAddr are the
	// bracketed host:port display forms — "::" renders as
	// "[::]:9300", never ":::9300".
	ManagementAddr string `json:"managementAddr"`
	AppAddr        string `json:"appAddr"`
	ReceiverAddr   string `json:"receiverAddr"`
	// Obs holds the configured observability launchpad
	// entries — the same card `status observability` prints.
	Obs keeper.ObsCard `json:"obs"`
	// StoreOK and RegistryOK are the banner verdicts; Tracked
	// and Due the headline counters behind them.
	StoreOK    bool `json:"storeOk"`
	RegistryOK bool `json:"registryOk"`
	Tracked    int  `json:"tracked"`
	Due        int  `json:"due"`
	// Plan is the due tail; Activity the friendly ring tail;
	// ActivityTotal the uncapped ring size, so the cap reads
	// honest.
	Plan          []planRow `json:"plan"`
	Activity      []string  `json:"activity"`
	ActivityTotal int       `json:"activityTotal"`
	// AppStatusURL is the absolute /api/status link: the
	// endpoint lives on the app server (another port), so a
	// relative href would 404 against the console. Built off
	// the request host — the only address known dialable from
	// this browser.
	AppStatusURL string       `json:"appStatusUrl"`
	Bundles      []BundleCard `json:"bundles"`
	ReloadErr    string       `json:"reloadError"`
	Source       string       `json:"source"`
}

// BundleCard is one TLS bundle's certificate card: the judged
// position first (machine-readable), then the leaf identity
// every renderer shares — the dashboard and the status command
// read the same model, so the two never disagree about what a
// row means. "ok" is thirty or more days out, "degraded" under
// thirty, "expired" past NotAfter, "parked" disabled by config,
// "unproven" when no leaf materialized and no trust configured,
// "incomplete" when filed CA material names an unresolvable
// issuer.
type BundleCard struct {
	Standing string `json:"standing"`
	Name     string `json:"name"`
	// Subject is the leaf's common name (first DNS name when
	// the CN is empty); Issuer its issuer CN. Both empty while
	// unproven — never guessed.
	Subject string `json:"subject"`
	Issuer  string `json:"issuer"`
	// DaysLeft counts whole days to expiry, floored: a leaf dead
	// eleven hours reads -1 ("expired yesterday"), never 0.
	// Zero for trust-only and unproven cards, where it means
	// not applicable, never unknown.
	DaysLeft int    `json:"days_left"`
	Expires  string `json:"expires"`
	// Lifetime voices the remaining lifetime both renderers
	// share: a count while valid, the overdue span past expiry,
	// "trust only" for CA-only bundles, "unknown" while
	// unproven, "incomplete chain" while gapped.
	Lifetime string `json:"lifetime"`
	// Identity voices what the bundle is both renderers share:
	// leaf and issuer with expiry for identity bundles, the
	// trust source for CA-only ones, "unreadable leaf" while
	// unproven — never an empty string operators must
	// interpret.
	Identity  string `json:"identity"`
	MinTLS    string `json:"minTLS"`
	TrustOnly bool   `json:"trust_only"`
}

// bundleExpireDays is the healthy horizon: fewer days left
// degrades the card while the credential still works.
const bundleExpireDays = 30

// StandBundle judges one bundle against now: parked before
// expiry (a parked bundle is a decision, not a countdown),
// trust-only next (a CA file with no leaf is configured, not
// missing — it reads ok when every issuer resolves), gaps
// next (a named issuer no filed CA carries reads incomplete
// however healthy the leaf), unparseable or missing leaf last
// (missing evidence never alarms — an unreadable leaf reads
// unproven, never a guessed lifetime). Gaps arrive per bundle
// from the snapshot's chain check; callers pass them through
// verbatim.
func StandBundle(name string, b resources.TLSBundle, cert *tls.Certificate, floor uint16,
	now time.Time, gaps []string) BundleCard {
	card := BundleCard{Name: name, MinTLS: resources.MinTLSName(floor)}
	if !resources.Enabled(b.Spec.Enabled) {
		card.Standing = "parked"
		return card
	}
	if b.Spec.CertFile == "" && b.Spec.KeyFile == "" {
		card.TrustOnly = true
		if len(gaps) > 0 {
			card.Standing = "incomplete"
			card.Lifetime = "incomplete chain"
			card.Identity = trustIdentity(b.Spec.CAFile) + " · missing issuer " +
				strings.Join(gaps, ", ")
			return card
		}
		card.Standing = "ok"
		card.Lifetime = "trust only"
		card.Identity = trustIdentity(b.Spec.CAFile)
		return card
	}
	leaf := bundleLeaf(cert)
	if leaf == nil {
		card.Standing = "unproven"
		card.Lifetime = "unknown"
		card.Identity = "unreadable leaf"
		return card
	}
	card.Subject = leaf.Subject.CommonName
	if card.Subject == "" && len(leaf.DNSNames) > 0 {
		card.Subject = leaf.DNSNames[0]
	}
	card.Issuer = leaf.Issuer.CommonName
	card.DaysLeft = int(math.Floor(leaf.NotAfter.Sub(now).Hours() / 24))
	card.Expires = leaf.NotAfter.Format("2006-01-02")
	if len(gaps) > 0 {
		card.Standing = "incomplete"
		card.Lifetime = "incomplete chain"
		card.Identity = card.Subject + " · issued by " + issuerOrUnknown(card.Issuer) +
			" · expires " + card.Expires + " · missing issuer " + strings.Join(gaps, ", ")
		return card
	}
	card.Lifetime = lifetimeText(card.DaysLeft)
	card.Identity = card.Subject + " · issued by " + issuerOrUnknown(card.Issuer) +
		" · expires " + card.Expires
	switch {
	case card.DaysLeft < 0:
		card.Standing = "expired"
	case card.DaysLeft < bundleExpireDays:
		card.Standing = "degraded"
	default:
		card.Standing = "ok"
	}
	return card
}

// bundleLeaf returns the bundle's leaf certificate: the parsed
// Leaf when the loader kept it, else the first chain element.
// Nil when neither parses.
func bundleLeaf(cert *tls.Certificate) *x509.Certificate {
	if cert == nil {
		return nil
	}
	if cert.Leaf != nil {
		return cert.Leaf
	}
	if len(cert.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// lifetimeText voices the remaining lifetime: a count while
// valid, the overdue span past expiry.
func lifetimeText(days int) string {
	switch d := days; {
	case d == -1:
		return "expired yesterday"
	case d < 0:
		return fmt.Sprintf("expired %d days ago", -d)
	case d == 0:
		return "expires today"
	case d == 1:
		return "1 day left"
	default:
		return fmt.Sprintf("%d days left", d)
	}
}

func issuerOrUnknown(issuer string) string {
	if issuer == "" {
		return "unknown"
	}
	return issuer
}

func trustIdentity(caFile string) string {
	if caFile == "" {
		return "system roots"
	}
	return "CA " + caFile
}

type metricsData struct {
	Uptime        string            `json:"uptime"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Requests      uint64            `json:"requests"`
	Version       string            `json:"version"`
	Commit        string            `json:"commit"`
	BuildTime     string            `json:"build_time"`
	Environment   map[string]string `json:"environment"`
}

// IndexHandler serves the built SPA shell (see assets.go).
// Tests and hand-wired servers use it directly; Start wires
// the method form below.
func IndexHandler(w http.ResponseWriter, r *http.Request) {
	(&Server{}).indexHandler(w, r)
}

// indexHandler serves the built SPA shell.
func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	indexHandler(w, r)
}

// overviewHandler serves the dashboard's live model as JSON:
// the keeper cards, the plan and activity tails, the bundle
// cards, the last reload refusal, and where the snapshot came
// from. The SPA polls it.
func (s *Server) overviewHandler(w http.ResponseWriter, r *http.Request) {
	cfg := config.Current()
	rt := config.CurrentRuntime()

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
	now := s.now().UTC()

	clockCard := s.clockCard(ctx, cfg)
	bundles, reloadErr := s.bundleCards()
	source := "unconfigured"
	if cfg.ConfigDir != "" {
		source = "KPR_CONFIG_DIR=" + cfg.ConfigDir
	}
	out := overviewData{
		Registry:       keeper.NewRegistryCard(s.Registry, st.RegistryOK, cfg.RegistryURL),
		Store:          keeper.FetchStoreCard(ctx, s.Store, st.StoreOK, cfg.RedisAddr, cfg.RedisDB, sentinel.Proven()),
		Sentinel:       sentinel,
		Edge:           keeper.FetchEdgeCard(ctx, gate, s.Store, s.EdgeAddr, s.RegistryURL),
		Clock:          clockCard,
		ClockNow:       now.Format("2006-01-02 15:04:05 UTC"),
		GoVersion:      rt.GoVersion,
		GoOSArch:       rt.GOOS + "/" + rt.GOARCH,
		ManagementAddr: bracketHost(cfg.ManagementHost) + ":" + strconv.Itoa(cfg.ManagementPort),
		AppAddr:        bracketHost(cfg.AppHost) + ":" + strconv.Itoa(cfg.AppPort),
		ReceiverAddr:   bracketHost(cfg.ReceiverHost) + ":" + strconv.Itoa(cfg.ReceiverPort),
		Obs:            obsCard(cfg),
		StoreOK:        st.StoreOK,
		RegistryOK:     st.RegistryOK,
		Tracked:        st.Tracked,
		Due:            st.Due,
		Plan:           planRows(st.Plan),
		Activity:       activityLines(st.Activity, now),
		ActivityTotal:  len(st.Activity),
		AppStatusURL:   appStatusURL(r.Host, cfg.AppPort),
		Bundles:        bundles,
		ReloadErr:      reloadErr,
		Source:         source,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("Error encoding overview JSON: %v", err)
	}
}

// clockCard voices the time section through the shared keeper
// card: local trusts the machine clock without probing,
// anything else probes through the clock port with the probe
// bound. NTP/HTTPS readings memoize for clockTTL so a down
// time source stalls one render per minute, not every render.
func (s *Server) clockCard(ctx context.Context, cfg *config.Config) keeper.ClockCard {
	src := s.clockSource
	if src == nil {
		src = clock.NewSource(cfg.TimeMethod)
	}
	if cfg.TimeMethod == clock.MethodLocal {
		return keeper.FetchClockCard(ctx, src, cfg.TimeMethod, cfg.TimeServer, clock.Tolerance)
	}
	key := string(cfg.TimeMethod) + "|" + cfg.TimeServer
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	now := s.now()
	if s.clockHit && s.clockKey == key && now.Sub(s.clockAt) < clockTTL {
		return s.clockCardCache
	}
	card := keeper.FetchClockCard(ctx, src, cfg.TimeMethod, cfg.TimeServer, clock.Tolerance)
	s.clockHit, s.clockKey, s.clockAt, s.clockCardCache = true, key, now, card
	return card
}

// bundleCards judges every loaded bundle against now, sorted
// by name: maps range randomly, dashboards must not. Nil
// holder hides the section (hand-wired servers, tests) and
// reports no reload error — nothing failed because nothing
// was configured to reload.
func (s *Server) bundleCards() ([]BundleCard, string) {
	if s.Resources == nil {
		return []BundleCard{}, ""
	}
	snap := s.Resources.Get()
	// Never nil: the SPA reads `.length` off the array, and
	// JSON renders a nil slice as null, not [].
	out := []BundleCard{}
	for _, e := range resources.SortedEntries(snap.Bundles) {
		var cert *tls.Certificate
		if c, ok := snap.Certs[e.Name]; ok {
			cert = &c
		}
		out = append(out, StandBundle(e.Name, e.Value, cert, snap.Floors[e.Name], s.now(),
			snap.ChainGaps[e.Name]))
	}
	var reloadErr string
	if err := s.Resources.LoadErr(); err != nil {
		reloadErr = err.Error()
	}
	return out, reloadErr
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
// host and swaps the port. Empty stays empty — the overview then
// renders no link, never a malformed one.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(hostport, "[]")
}

// appStatusURL is the absolute /api/status href off the request
// host (the only address known dialable from this browser) with
// the app port swapped in. Empty without a host — the overview
// then carries no link, never a malformed one.
func appStatusURL(hostport string, appPort int) string {
	host := hostOnly(hostport)
	if host == "" {
		return ""
	}
	return "http://" + bracketHost(host) + ":" + strconv.Itoa(appPort) + "/api/status"
}

// MetricsHandler returns JSON metrics on a default-wired server.
// Start wires the method form below.
func MetricsHandler(w http.ResponseWriter, r *http.Request) {
	(&Server{}).metricsHandler(w, r)
}

// metricsHandler returns JSON metrics with this server's
// clock: uptime pins to scripted time in tests.
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	uptime := s.now().Sub(startTime)
	if uptime < 0 {
		uptime = 0
	}

	cfg := config.Current()
	// Secrets stay out of this map on purpose: KPR_REDIS_PASSWORD,
	// KPR_REGISTRY_PASSWORD, and KPR_CONSOLE_TOKEN render as
	// presence-only on `kpr env`, never here.
	env := map[string]string{
		"HOSTNAME":            hostname,
		"KPR_MANAGEMENT_HOST": cfg.ManagementHost,
		"KPR_MANAGEMENT_PORT": strconv.Itoa(cfg.ManagementPort),
		"KPR_APP_HOST":        cfg.AppHost,
		"KPR_APP_PORT":        strconv.Itoa(cfg.AppPort),
		"KPR_RECEIVER_HOST":   cfg.ReceiverHost,
		"KPR_RECEIVER_PORT":   strconv.Itoa(cfg.ReceiverPort),
		"KPR_REDIS_ADDR":      cfg.RedisAddr,
		"KPR_CONFIG_DIR":      cfg.ConfigDir,
		"KPR_TIME_METHOD":     string(cfg.TimeMethod),
		"KPR_TIME_SERVER":     cfg.TimeServer,
		// UI base URLs back the console launchpad: empty (the
		// default) hides the link, set shows it.
		"KPR_QUICKWIT_URL":   cfg.QuickwitURL,
		"KPR_JAEGER_URL":     cfg.JaegerURL,
		"KPR_GRAFANA_URL":    cfg.GrafanaURL,
		"KPR_PROMETHEUS_URL": cfg.PrometheusURL,
	}

	metrics := metricsData{
		Uptime:        human.Dur(uptime.Round(time.Second)),
		UptimeSeconds: int64(uptime.Seconds()),
		Requests:      app.GetAPIRequestCount(),
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

// reloadResult is the reload endpoint's JSON body: the applied
// bundle count on success, the refusal on failure. Listeners
// read TLS at boot — a reload swaps what the dashboard and
// status show, then a restart applies it to serving sockets.
type reloadResult struct {
	Applied int    `json:"applied,omitempty"`
	Error   string `json:"error,omitempty"`
	Note    string `json:"note,omitempty"`
}

// reloadHandler swaps a freshly validated snapshot in: Check
// loads without touching the live one, Swap lands it whole.
// Serialized, so two concurrent POSTs cannot interleave and
// land the older snapshot last.
func (s *Server) reloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Resources == nil {
		http.Error(w, "no config holder on this server", http.StatusServiceUnavailable)
		return
	}
	s.reloadSeqMu.Lock()
	defer s.reloadSeqMu.Unlock()
	next, err := s.Resources.Check()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(reloadResult{Error: err.Error()})
		return
	}
	s.Resources.Swap(next)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reloadResult{
		Applied: len(next.Bundles),
		Note:    "serving listeners read TLS at boot; restart to apply",
	})
}

// requireAuth gates the mux behind HTTP Basic user "ops" and
// the console token, compared constant-time. /health stays
// open: probes carry no credentials. Every other path —
// dashboard, metrics, API, reload, ops — needs the token.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ops" ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.Token)) != 1 ||
			s.Token == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="kpr console"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// opsHandler serves operator actions: the wired handler, or a
// 503 naming the gap when none is wired. The handler owns its
// auth — the console never sees the operator token.
func (s *Server) opsHandler() http.Handler {
	if s.Ops != nil {
		return s.Ops
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "operator actions not configured on this server", http.StatusServiceUnavailable)
	})
}
