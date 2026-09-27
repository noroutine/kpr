// Package config is the single, documented source of truth for every
// environment variable this program reads, plus the handful of tunable
// constants (server timeouts, default addresses) that have no environment
// override at all. It resolves all of them once into an immutable-after-
// construction Config value via Builder, rather than exposing each knob as
// its own independently-readable/mutable package-level function, var, or
// const — a scattered set of independently mutable knobs is exactly the
// kind of thing worth being defensive about: nothing coordinates them,
// nothing lets you reason about "the config" as one value, and it only
// gets worse as more mockable knobs are added.
//
// Production code builds one Config from the environment at startup
// (NewBuilder().FromEnv().Build()), installs it with SetCurrent, and every
// consumer throughout the app reads Current().Field instead of a
// package-level identifier. Tests build their own Config (typically
// starting from NewBuilder(), which never touches the real environment) and
// install it for the scope of one test:
//
//	t.Cleanup(config.SetCurrent(config.NewBuilder().WithRedisAddr("localhost:6380").Build()))
//
// SetCurrent swaps one atomic pointer, so a reader never observes a
// half-updated config, and a test's override/restore is one line instead of
// a bespoke save/restore per knob.
//
// Build() never fails outright: an invalid env value falls back to its
// default, recorded in a companion *Warning field (e.g.
// ManagementPortWarning) so the caller can log it once at startup.
//
// The one dimension this package deliberately does not own is argv: CLI
// flags live on the cobra commands in internal/cli (cobra parses argv and
// renders --help from the command structs, so a parallel FlagDefs registry
// here would be a second source of truth, not a single one). Env-backed
// defaults for those flags still live here — internal/cli seeds each flag's
// default from a FromEnv-built Config — so the default value is written
// down exactly once.
package config

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Environment variable names this program reads, grouped by concern —
// server bindings, state backend, observability — in the same order
// EnvVars below lists them. Keep a new const and its EnvVars entry in the
// matching group in both places, so auditing "what controls X" never needs
// more than one section of this file.
const (
	// EnvManagementHost overrides the management console bind address.
	// Defaults to DefaultManagementHost ("::", dual-stack IPv4+IPv6).
	EnvManagementHost = "KPR_MANAGEMENT_HOST"

	// EnvManagementPort overrides the management console port.
	// Defaults to DefaultManagementPort. Invalid values fall back to the
	// default (see ManagementPortWarning).
	EnvManagementPort = "KPR_MANAGEMENT_PORT"

	// EnvAppHost overrides the application server bind address.
	// Defaults to DefaultAppHost ("::", dual-stack IPv4+IPv6).
	EnvAppHost = "KPR_APP_HOST"

	// EnvAppPort overrides the application server port.
	// Defaults to DefaultAppPort. Invalid values fall back to the
	// default (see AppPortWarning).
	EnvAppPort = "KPR_APP_PORT"

	// EnvRedisAddr overrides the redis address kpr uses for TTL tracking
	// and cleanup bookkeeping. Defaults to DefaultRedisAddr.
	EnvRedisAddr = "KPR_REDIS_ADDR"

	// EnvOTELEnabled, when set to "true", enables OpenTelemetry tracing.
	EnvOTELEnabled = "OTEL_ENABLED"

	// EnvOTELEndpoint overrides the OTLP/gRPC exporter endpoint
	// (host:port; a leading http:// or https:// is stripped).
	// Defaults to DefaultOTELEndpoint.
	EnvOTELEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"

	// EnvOTELServiceName overrides the service name reported in traces.
	// Defaults to DefaultOTELServiceName.
	EnvOTELServiceName = "OTEL_SERVICE_NAME"

	// EnvOTELServiceVersion overrides the service version reported in
	// traces. Defaults to the built binary's version (see Version).
	EnvOTELServiceVersion = "OTEL_SERVICE_VERSION"

	// EnvOTELEnvironment overrides the deployment environment reported in
	// traces. Defaults to DefaultOTELEnvironment.
	EnvOTELEnvironment = "OTEL_ENVIRONMENT"

	// EnvQuickwitURL is the browser-facing Quickwit UI base URL shown
	// on the management console. Empty (the default) hides the link.
	EnvQuickwitURL = "KPR_QUICKWIT_URL"

	// EnvJaegerURL is the browser-facing Jaeger UI base URL shown on
	// the management console. Empty (the default) hides the link.
	EnvJaegerURL = "KPR_JAEGER_URL"

	// EnvGrafanaURL is the browser-facing Grafana base URL shown on
	// the management console. Empty (the default) hides the link.
	EnvGrafanaURL = "KPR_GRAFANA_URL"

	// EnvPrometheusURL is the browser-facing Prometheus base URL shown
	// on the management console. Empty (the default) hides the link.
	EnvPrometheusURL = "KPR_PROMETHEUS_URL"
)

// EnvVar is one environment variable this program reads, paired with the
// prose user-facing help prints for it.
type EnvVar struct {
	Name        string
	Description string
}

// EnvVars is every environment variable this program reads, grouped by
// concern in the same order as the Env* consts above. It is the single
// place a var's user-facing description is written down. Add a new Env*
// const and its EnvVars entry together, in the matching group — and a case
// in TestEnvVarsDocumentsEveryEnvConst, which fails if the two drift apart.
var EnvVars = []EnvVar{
	{EnvManagementHost, "Management console bind address. Defaults to \"::\" (dual-stack IPv4+IPv6)."},
	{EnvManagementPort, "Management console port. Defaults to 9300; invalid values fall back to the default."},
	{EnvAppHost, "Application server bind address. Defaults to \"::\" (dual-stack IPv4+IPv6)."},
	{EnvAppPort, "Application server port. Defaults to 8080; invalid values fall back to the default."},
	{EnvRedisAddr, "Redis address for TTL tracking and cleanup bookkeeping. Defaults to localhost:6379."},
	{EnvOTELEnabled, "Set to \"true\" to enable OpenTelemetry tracing. Disabled by default."},
	{EnvOTELEndpoint, "OTLP/gRPC exporter endpoint (host:port). Defaults to localhost:4317."},
	{EnvOTELServiceName, "Service name reported in traces. Defaults to kpr."},
	{EnvOTELServiceVersion, "Service version reported in traces. Defaults to the built binary's version."},
	{EnvOTELEnvironment, "Deployment environment reported in traces. Defaults to development."},
	{EnvQuickwitURL, "Quickwit UI base URL linked from the management console. Empty by default (no link)."},
	{EnvJaegerURL, "Jaeger UI base URL linked from the management console. Empty by default (no link)."},
	{EnvGrafanaURL, "Grafana base URL linked from the management console. Empty by default (no link)."},
	{EnvPrometheusURL, "Prometheus base URL linked from the management console. Empty by default (no link)."},
}

const (
	// Defaults for the server bindings. The cobra flags in internal/cli
	// seed from these (via a FromEnv-built Config), so change them here,
	// not there.
	DefaultManagementHost = "::"
	DefaultManagementPort = 9300
	DefaultAppHost        = "::"
	DefaultAppPort        = 8080

	// DefaultRedisAddr is the redis address used when KPR_REDIS_ADDR is
	// unset — a bare local run with no compose stack alongside it.
	DefaultRedisAddr = "localhost:6379"

	// Observability defaults.
	DefaultOTELEndpoint    = "localhost:4317"
	DefaultOTELServiceName = "kpr"
	DefaultOTELEnvironment = "development"

	// Server timeouts. No environment override: these are operator-stable
	// tuning, still Config fields (seeded here by defaultConfig) so every
	// knob is read the same way — config.Current().Field — and tests can
	// shrink them via With* setters instead of waiting out real seconds.
	HTTPReadTimeout  = 10 * time.Second
	HTTPWriteTimeout = 10 * time.Second
	HTTPIdleTimeout  = 60 * time.Second
	ShutdownTimeout  = 5 * time.Second
)

// Config is the resolved runtime configuration: what the environment asked
// for (via FromEnv) plus the tunable constants above. Immutable after
// Build; consumers read Current().Field, tests override via Builder.
type Config struct {
	// ManagementHost is EnvManagementHost's value, or
	// DefaultManagementHost if unset.
	ManagementHost string
	// ManagementPort is EnvManagementPort's parsed value, or
	// DefaultManagementPort if unset or invalid. See
	// ManagementPortWarning.
	ManagementPort int
	// ManagementPortWarning is non-nil when EnvManagementPort was set but
	// not a valid port; ManagementPort still falls back to
	// DefaultManagementPort. Callers that log this (serve startup) check
	// it once, right after building the config.
	ManagementPortWarning error

	// AppHost is EnvAppHost's value, or DefaultAppHost if unset.
	AppHost string
	// AppPort is EnvAppPort's parsed value, or DefaultAppPort if unset
	// or invalid. See AppPortWarning.
	AppPort int
	// AppPortWarning is non-nil when EnvAppPort was set but not a valid
	// port; AppPort still falls back to DefaultAppPort.
	AppPortWarning error

	// RedisAddr is EnvRedisAddr's value, or DefaultRedisAddr if unset.
	RedisAddr string

	// OTELEnabled is true when EnvOTELEnabled is exactly "true".
	OTELEnabled bool
	// OTLPEndpoint is EnvOTELEndpoint's value with any leading http:// or
	// https:// stripped (the OTLP exporter wants host:port only), or
	// DefaultOTELEndpoint if unset.
	OTLPEndpoint string
	// OTELServiceName is EnvOTELServiceName's value, or
	// DefaultOTELServiceName if unset.
	OTELServiceName string
	// OTELServiceVersion is EnvOTELServiceVersion's value, or the built
	// binary's version (see Version) if unset.
	OTELServiceVersion string
	// OTELEnvironment is EnvOTELEnvironment's value, or
	// DefaultOTELEnvironment if unset.
	OTELEnvironment string

	// QuickwitURL is EnvQuickwitURL's value, or "" if unset. Empty
	// means the management console shows no Quickwit link.
	QuickwitURL string
	// JaegerURL is EnvJaegerURL's value, or "" if unset. Empty means
	// the management console shows no Jaeger link.
	JaegerURL string
	// GrafanaURL is EnvGrafanaURL's value, or "" if unset. Empty means
	// the management console shows no Grafana link.
	GrafanaURL string
	// PrometheusURL is EnvPrometheusURL's value, or "" if unset. Empty
	// means the management console shows no Prometheus link.
	PrometheusURL string

	// HTTPReadTimeout, HTTPWriteTimeout, HTTPIdleTimeout, and
	// ShutdownTimeout have no environment override — see the matching
	// constants above. They're still Config fields (seeded from those
	// constants by defaultConfig, below) so every knob in this package is
	// read the same way: config.Current().Field.
	HTTPReadTimeout  time.Duration
	HTTPWriteTimeout time.Duration
	HTTPIdleTimeout  time.Duration
	ShutdownTimeout  time.Duration
}

// defaultConfig is every field's starting value before any environment
// resolution or test override — equivalent to "every env var unset."
func defaultConfig() Config {
	return Config{
		ManagementHost: DefaultManagementHost,
		ManagementPort: DefaultManagementPort,
		AppHost:        DefaultAppHost,
		AppPort:        DefaultAppPort,
		RedisAddr:      DefaultRedisAddr,

		OTLPEndpoint:       DefaultOTELEndpoint,
		OTELServiceName:    DefaultOTELServiceName,
		OTELServiceVersion: Version,
		OTELEnvironment:    DefaultOTELEnvironment,

		HTTPReadTimeout:  HTTPReadTimeout,
		HTTPWriteTimeout: HTTPWriteTimeout,
		HTTPIdleTimeout:  HTTPIdleTimeout,
		ShutdownTimeout:  ShutdownTimeout,
	}
}

// Builder constructs a Config. NewBuilder alone never touches the real
// environment — call FromEnv for that — so it's the natural starting point
// for a hermetic test; production calls NewBuilder().FromEnv().Build().
type Builder struct {
	cfg Config
}

// NewBuilder starts a Builder from defaultConfig, i.e. as if every
// environment variable in this package were unset.
func NewBuilder() *Builder {
	return &Builder{cfg: defaultConfig()}
}

// parsePort resolves a port env var: unset or blank means the default,
// anything outside 1-65535 (or not a number at all) means the default plus
// a warning for the caller to log — ports fail soft, never fatal, since
// the server can always come up on its known-good default.
func parsePort(raw string, def int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > 65535 {
		return def, &PortWarning{Raw: raw, Default: def}
	}
	return n, nil
}

// PortWarning records an unusable port env value that fell back to its
// default. It prints as the startup log line, not as a hard error.
type PortWarning struct {
	Raw     string
	Default int
}

func (w *PortWarning) Error() string {
	return "invalid port " + strconv.Quote(w.Raw) + ", using default"
}

// FromEnv resolves every environment-backed field from os.Getenv.
func (b *Builder) FromEnv() *Builder {
	b.cfg.ManagementHost = envOr(EnvManagementHost, DefaultManagementHost)
	b.cfg.AppHost = envOr(EnvAppHost, DefaultAppHost)
	b.cfg.RedisAddr = envOr(EnvRedisAddr, DefaultRedisAddr)

	var err error
	b.cfg.ManagementPort, err = parsePort(os.Getenv(EnvManagementPort), DefaultManagementPort)
	if err != nil {
		b.cfg.ManagementPortWarning = err
	}
	b.cfg.AppPort, err = parsePort(os.Getenv(EnvAppPort), DefaultAppPort)
	if err != nil {
		b.cfg.AppPortWarning = err
	}

	b.cfg.OTELEnabled = os.Getenv(EnvOTELEnabled) == "true"
	b.cfg.OTLPEndpoint = stripScheme(envOr(EnvOTELEndpoint, DefaultOTELEndpoint))
	b.cfg.OTELServiceName = envOr(EnvOTELServiceName, DefaultOTELServiceName)
	b.cfg.OTELServiceVersion = envOr(EnvOTELServiceVersion, Version)
	b.cfg.OTELEnvironment = envOr(EnvOTELEnvironment, DefaultOTELEnvironment)
	b.cfg.QuickwitURL = envOr(EnvQuickwitURL, "")
	b.cfg.JaegerURL = envOr(EnvJaegerURL, "")
	b.cfg.GrafanaURL = envOr(EnvGrafanaURL, "")
	b.cfg.PrometheusURL = envOr(EnvPrometheusURL, "")
	return b
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	return endpoint
}

// Build returns the built Config. It never fails: unusable values fall
// back to defaults with a companion *Warning field (see FromEnv).
func (b *Builder) Build() *Config {
	cfg := b.cfg
	return &cfg
}

func (b *Builder) WithManagementHost(v string) *Builder         { b.cfg.ManagementHost = v; return b }
func (b *Builder) WithManagementPort(v int) *Builder            { b.cfg.ManagementPort = v; return b }
func (b *Builder) WithAppHost(v string) *Builder                { b.cfg.AppHost = v; return b }
func (b *Builder) WithAppPort(v int) *Builder                   { b.cfg.AppPort = v; return b }
func (b *Builder) WithRedisAddr(v string) *Builder              { b.cfg.RedisAddr = v; return b }
func (b *Builder) WithOTELEnabled(v bool) *Builder              { b.cfg.OTELEnabled = v; return b }
func (b *Builder) WithOTLPEndpoint(v string) *Builder           { b.cfg.OTLPEndpoint = stripScheme(v); return b }
func (b *Builder) WithOTELServiceName(v string) *Builder        { b.cfg.OTELServiceName = v; return b }
func (b *Builder) WithOTELServiceVersion(v string) *Builder     { b.cfg.OTELServiceVersion = v; return b }
func (b *Builder) WithOTELEnvironment(v string) *Builder        { b.cfg.OTELEnvironment = v; return b }
func (b *Builder) WithQuickwitURL(v string) *Builder            { b.cfg.QuickwitURL = v; return b }
func (b *Builder) WithJaegerURL(v string) *Builder              { b.cfg.JaegerURL = v; return b }
func (b *Builder) WithGrafanaURL(v string) *Builder             { b.cfg.GrafanaURL = v; return b }
func (b *Builder) WithPrometheusURL(v string) *Builder          { b.cfg.PrometheusURL = v; return b }
func (b *Builder) WithHTTPReadTimeout(v time.Duration) *Builder { b.cfg.HTTPReadTimeout = v; return b }
func (b *Builder) WithHTTPWriteTimeout(v time.Duration) *Builder {
	b.cfg.HTTPWriteTimeout = v
	return b
}
func (b *Builder) WithHTTPIdleTimeout(v time.Duration) *Builder { b.cfg.HTTPIdleTimeout = v; return b }
func (b *Builder) WithShutdownTimeout(v time.Duration) *Builder { b.cfg.ShutdownTimeout = v; return b }

// current holds the active Config. Never nil: init seeds it with the
// defaults so Current() is safe to call even before anything calls
// SetCurrent.
var current atomic.Pointer[Config]

func init() {
	current.Store(NewBuilder().Build())
}

// Current returns the active Config. Every consumer reads
// Current().Field this way rather than os.Getenv or a package-level var of
// its own, so the whole process reasons about one value.
func Current() *Config {
	return current.Load()
}

// SetCurrent installs cfg as the active Config and returns a function that
// restores whatever was active before — the one-liner tests use to scope
// an override:
//
//	t.Cleanup(config.SetCurrent(config.NewBuilder().WithAppPort(1).Build()))
func SetCurrent(cfg *Config) (restore func()) {
	previous := current.Swap(cfg)
	return func() { current.Store(previous) }
}
