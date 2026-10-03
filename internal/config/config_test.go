package config

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
)

// An operator starting kpr with a clean environment must get the documented
// defaults: dual-stack bind addresses, the well-known ports, a local redis,
// and tracing off. If this fails, a bare `kpr serve` binds somewhere
// surprising or phones home unexpectedly.
// Must stay in sync with docs/CONFIG.md#adding-a-new-env-var.
func TestNewBuilderDefaults(t *testing.T) {
	cfg := NewBuilder().Build()
	if cfg.ManagementHost != "::" || cfg.AppHost != "::" {
		t.Errorf("hosts = %q/%q, want ::/::", cfg.ManagementHost, cfg.AppHost)
	}
	if cfg.ManagementPort != 9300 || cfg.AppPort != 8080 {
		t.Errorf("ports = %d/%d, want 9300/8080", cfg.ManagementPort, cfg.AppPort)
	}
	if cfg.RedisAddr != "localhost:6379" {
		t.Errorf("RedisAddr = %q, want localhost:6379", cfg.RedisAddr)
	}
	if cfg.RedisPassword != "" {
		t.Errorf("RedisPassword = %q, want empty (no auth by default)", cfg.RedisPassword)
	}
	if cfg.RedisDB != 0 {
		t.Errorf("RedisDB = %d, want 0 (redis convention; deployments override)", cfg.RedisDB)
	}
	if cfg.RedisDBWarning != nil {
		t.Error("clean environment must not produce a redis DB warning")
	}
	if cfg.OTELEnabled {
		t.Error("OTELEnabled = true, want false (tracing is opt-in)")
	}
	if cfg.ManagementPortWarning != nil || cfg.AppPortWarning != nil {
		t.Error("clean environment must not produce port warnings")
	}
	if cfg.HTTPReadTimeout != 10*time.Second || cfg.HTTPWriteTimeout != 10*time.Second ||
		cfg.HTTPIdleTimeout != 60*time.Second || cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("timeouts = %v/%v/%v/%v, want 10s/10s/60s/5s",
			cfg.HTTPReadTimeout, cfg.HTTPWriteTimeout, cfg.HTTPIdleTimeout, cfg.ShutdownTimeout)
	}
}

// A compose stack sets every KPR_* var at once; the resolved Config must
// reflect all of them, including the in-stack redis hostname. If this
// fails, `docker compose up` silently runs against defaults instead of the
// stack it ships with.
func TestFromEnvResolvesEveryVar(t *testing.T) {
	t.Setenv(EnvManagementHost, "127.0.0.1")
	t.Setenv(EnvManagementPort, "19300")
	t.Setenv(EnvAppHost, "127.0.0.1")
	t.Setenv(EnvAppPort, "18080")
	t.Setenv(EnvRedisAddr, "redis:6379")
	t.Setenv(EnvRedisPassword, "s3cret")
	t.Setenv(EnvRedisDB, "4")
	t.Setenv(EnvRegistryUser, "robot")
	t.Setenv(EnvRegistryPassword, "hunter2")
	t.Setenv(EnvRegistryConfig, "/etc/registry/config.yml")
	t.Setenv(EnvOTELEnabled, "true")
	t.Setenv(EnvOTELEndpoint, "https://tempo:4318")
	t.Setenv(EnvOTELServiceName, "kpr-prod")
	t.Setenv(EnvOTELServiceVersion, "v9.9.9")
	t.Setenv(EnvOTELEnvironment, "production")

	cfg := NewBuilder().FromEnv().Build()
	if cfg.ManagementHost != "127.0.0.1" || cfg.ManagementPort != 19300 {
		t.Errorf("management = %s:%d", cfg.ManagementHost, cfg.ManagementPort)
	}
	if cfg.AppHost != "127.0.0.1" || cfg.AppPort != 18080 {
		t.Errorf("app = %s:%d", cfg.AppHost, cfg.AppPort)
	}
	if cfg.RedisAddr != "redis:6379" {
		t.Errorf("RedisAddr = %q", cfg.RedisAddr)
	}
	if cfg.RedisPassword != "s3cret" {
		t.Errorf("RedisPassword = %q, want s3cret", cfg.RedisPassword)
	}
	if cfg.RedisDB != 4 || cfg.RedisDBWarning != nil {
		t.Errorf("RedisDB = %d/%v, want 4 with no warning", cfg.RedisDB, cfg.RedisDBWarning)
	}
	if cfg.RegistryUser != "robot" || cfg.RegistryPassword != "hunter2" {
		t.Errorf("registry creds = %q/***, want robot/hunter2", cfg.RegistryUser)
	}
	if cfg.RegistryConfig != "/etc/registry/config.yml" {
		t.Errorf("RegistryConfig = %q, want the env value", cfg.RegistryConfig)
	}
	if !cfg.OTELEnabled || cfg.OTLPEndpoint != "tempo:4318" {
		t.Errorf("otel = enabled:%v endpoint:%q", cfg.OTELEnabled, cfg.OTLPEndpoint)
	}
	if cfg.OTELServiceName != "kpr-prod" || cfg.OTELServiceVersion != "v9.9.9" || cfg.OTELEnvironment != "production" {
		t.Errorf("otel identity = %q/%q/%q", cfg.OTELServiceName, cfg.OTELServiceVersion, cfg.OTELEnvironment)
	}
}

// Unset registry config falls back to the stock distribution path.
// If this fails, commands read shared-store paths from nowhere.
func TestFromEnvDefaultsRegistryConfig(t *testing.T) {
	t.Setenv(EnvRegistryConfig, "")
	cfg := NewBuilder().FromEnv().Build()
	if cfg.RegistryConfig != DefaultRegistryConfig {
		t.Errorf("RegistryConfig = %q, want default %q", cfg.RegistryConfig, DefaultRegistryConfig)
	}
}

// Time source resolves to local-by-default, https or ntp on request,
// and falls soft to the default (with a warning) on garbage — Build
// never fails outright. If this fails, mints check against the
// wrong transport or a typo silently selects nothing.
func TestFromEnvResolvesTimeSource(t *testing.T) {
	t.Setenv(EnvTimeMethod, "ntp")
	t.Setenv(EnvTimeServer, "time.example.com")

	cfg := NewBuilder().FromEnv().Build()
	if cfg.TimeMethod != clock.MethodNTP || cfg.TimeServer != "time.example.com" {
		t.Errorf("time = %q/%q", cfg.TimeMethod, cfg.TimeServer)
	}
	if cfg.TimeMethodWarning != nil {
		t.Errorf("warning = %v, want nil for a valid method", cfg.TimeMethodWarning)
	}
}

func TestFromEnvDefaultsTimeSource(t *testing.T) {
	cfg := NewBuilder().FromEnv().Build()
	if cfg.TimeMethod != clock.MethodLocal || cfg.TimeServer != clock.DFNServer {
		t.Errorf("time = %q/%q, want local DFN", cfg.TimeMethod, cfg.TimeServer)
	}
}

func TestFromEnvResolvesHTTPS(t *testing.T) {
	t.Setenv(EnvTimeMethod, "https")

	cfg := NewBuilder().FromEnv().Build()
	if cfg.TimeMethod != clock.MethodHTTPS {
		t.Errorf("method = %q, want https", cfg.TimeMethod)
	}
	if cfg.TimeMethodWarning != nil {
		t.Errorf("warning = %v, want nil for a valid method", cfg.TimeMethodWarning)
	}
}

func TestFromEnvRejectsBadTimeMethod(t *testing.T) {
	t.Setenv(EnvTimeMethod, "sundial")

	cfg := NewBuilder().FromEnv().Build()
	if cfg.TimeMethod != clock.DefaultMethod {
		t.Errorf("method = %q, want the default", cfg.TimeMethod)
	}
	if cfg.TimeMethodWarning == nil {
		t.Error("garbage method warned nothing")
	}
}

// UI base URLs resolve from the environment and default to empty
// (no console link). If this fails, the launchpad links somewhere
// stale or appears when nothing backs it.
func TestFromEnvResolvesUILinks(t *testing.T) {
	t.Setenv(EnvQuickwitURL, "http://localhost:7280")
	t.Setenv(EnvGrafanaURL, "http://localhost:3000")

	cfg := NewBuilder().FromEnv().Build()
	if cfg.QuickwitURL != "http://localhost:7280" || cfg.GrafanaURL != "http://localhost:3000" {
		t.Errorf("links = %q/%q", cfg.QuickwitURL, cfg.GrafanaURL)
	}
	if cfg.JaegerURL != "" || cfg.PrometheusURL != "" {
		t.Errorf("unset links = %q/%q, want empty", cfg.JaegerURL, cfg.PrometheusURL)
	}
}

// A typo'd port must not take the server down: kpr still comes up on the
// known-good default, and the warning tells the operator exactly which
// value was rejected. If this fails, one bad env var is a startup outage
// instead of a log line.
// An explicitly blank port is the same as unset: the default applies with
// no warning. If this fails, clearing a variable (the documented way to
// get the default back) warns spuriously or, worse, binds port zero.
func TestFromEnvBlankPortMeansDefault(t *testing.T) {
	t.Setenv(EnvManagementPort, "")
	t.Setenv(EnvAppPort, "   ")
	cfg := NewBuilder().FromEnv().Build()
	if cfg.ManagementPort != DefaultManagementPort || cfg.AppPort != DefaultAppPort {
		t.Errorf("ports = %d/%d, want defaults", cfg.ManagementPort, cfg.AppPort)
	}
	if cfg.ManagementPortWarning != nil || cfg.AppPortWarning != nil {
		t.Error("blank ports must not warn")
	}
}

// The boundary ports 1 and 65535 are valid and must be accepted without
// warning: an off-by-one in the range check would silently rebind the
// server to its default while the operator believes their explicit port
// is in effect. If this fails, the valid range is narrower than
// documented.
func TestFromEnvAcceptsBoundaryPorts(t *testing.T) {
	for _, raw := range []string{"1", "65535"} {
		t.Setenv(EnvManagementPort, raw)
		t.Setenv(EnvAppPort, raw)
		cfg := NewBuilder().FromEnv().Build()
		if cfg.ManagementPortWarning != nil || cfg.AppPortWarning != nil {
			t.Errorf("raw %q: unexpected warning", raw)
		}
		want, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ManagementPort != want || cfg.AppPort != want {
			t.Errorf("raw %q: ports = %d/%d", raw, cfg.ManagementPort, cfg.AppPort)
		}
	}
}

func TestFromEnvBadPortFallsBackWithWarning(t *testing.T) {
	for _, raw := range []string{"notaport", "0", "-1", "65536", "9300junk"} {
		t.Setenv(EnvManagementPort, raw)
		t.Setenv(EnvAppPort, raw)
		cfg := NewBuilder().FromEnv().Build()
		if cfg.ManagementPort != DefaultManagementPort {
			t.Errorf("raw %q: management port = %d, want default %d", raw, cfg.ManagementPort, DefaultManagementPort)
		}
		if cfg.AppPort != DefaultAppPort {
			t.Errorf("raw %q: app port = %d, want default %d", raw, cfg.AppPort, DefaultAppPort)
		}
		if cfg.ManagementPortWarning == nil || cfg.AppPortWarning == nil {
			t.Errorf("raw %q: no warning recorded", raw)
		} else if !strings.Contains(cfg.ManagementPortWarning.Error(), raw) {
			t.Errorf("raw %q: warning %q does not name the value", raw, cfg.ManagementPortWarning)
		}
	}
}

// Every With* setter must write its own field and nothing else: a setter
// wired to the wrong field silently retunes an unrelated knob. If this
// fails, a test scoping one override is actually scoping another.
func TestBuilderEveryWithSetterAppliesItsOwnField(t *testing.T) {
	cfg := NewBuilder().
		WithManagementHost("h1").
		WithManagementPort(1).
		WithAppHost("h2").
		WithAppPort(2).
		WithRedisAddr("r:1").
		WithRedisPassword("pw").
		WithRedisDB(4).
		WithRegistryURL("http://reg:5000").
		WithRegistryUser("u").
		WithRegistryPassword("p").
		WithRegistryConfig("/r.yml").
		WithCLINoDryRun(true).
		WithOTELEnabled(true).
		WithOTLPEndpoint("e:1").
		WithOTELServiceName("s").
		WithOTELServiceVersion("v").
		WithOTELEnvironment("env").
		WithHTTPReadTimeout(time.Second).
		WithHTTPWriteTimeout(2 * time.Second).
		WithHTTPIdleTimeout(3 * time.Second).
		WithShutdownTimeout(4 * time.Second).
		Build()
	if cfg.ManagementHost != "h1" || cfg.ManagementPort != 1 {
		t.Errorf("management = %s:%d", cfg.ManagementHost, cfg.ManagementPort)
	}
	if cfg.AppHost != "h2" || cfg.AppPort != 2 {
		t.Errorf("app = %s:%d", cfg.AppHost, cfg.AppPort)
	}
	if cfg.RedisAddr != "r:1" || cfg.RedisPassword != "pw" || cfg.RedisDB != 4 || !cfg.OTELEnabled || cfg.OTLPEndpoint != "e:1" {
		t.Errorf("backend/otel = %q/%q/%d/%v/%q", cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, cfg.OTELEnabled, cfg.OTLPEndpoint)
	}
	if cfg.RegistryURL != "http://reg:5000" || cfg.RegistryUser != "u" || cfg.RegistryPassword != "p" || !cfg.CLINoDryRun {
		t.Errorf("keeper = %q/%q/%q/%v, want reg/creds/armed", cfg.RegistryURL, cfg.RegistryUser, cfg.RegistryPassword, cfg.CLINoDryRun)
	}
	if cfg.RegistryConfig != "/r.yml" {
		t.Errorf("RegistryConfig = %q, want /r.yml", cfg.RegistryConfig)
	}
	if cfg.OTELServiceName != "s" || cfg.OTELServiceVersion != "v" || cfg.OTELEnvironment != "env" {
		t.Error("otel identity not applied")
	}
	if cfg.HTTPReadTimeout != time.Second || cfg.HTTPWriteTimeout != 2*time.Second ||
		cfg.HTTPIdleTimeout != 3*time.Second || cfg.ShutdownTimeout != 4*time.Second {
		t.Error("timeouts not applied")
	}
}

// A test scoping a Config override must not leak it into the next test:
// SetCurrent's restore func has to put the previous value back. If this
// fails, test order decides production behavior under test.
func TestSetCurrentRestoresPrevious(t *testing.T) {
	before := Current()
	restore := SetCurrent(NewBuilder().WithAppPort(1).Build())
	if Current().AppPort != 1 {
		t.Fatal("override not installed")
	}
	restore()
	if Current() != before {
		t.Error("restore did not put the previous config back")
	}
}

// Every Env* const must appear in EnvVars exactly once: the const block is
// what FromEnv reads, EnvVars is what help/docs render, and the two
// drifting apart is how a variable goes live but undocumented. If this
// fails, update EnvVars (and docs/CONFIG.md) alongside the new const.
func TestEnvVarsDocumentsEveryEnvConst(t *testing.T) {
	consts := []string{
		EnvManagementHost, EnvManagementPort, EnvAppHost, EnvAppPort,
		EnvRedisAddr, EnvRedisPassword, EnvRedisDB, EnvStore, EnvStoreDir,
		EnvRegistryURL, EnvRegistryUser, EnvRegistryPassword,
		EnvRegistryConfig,
		EnvEdgeAddr, EnvEdge, EnvCLINoDryRun,
		EnvOTELEnabled, EnvOTELEndpoint, EnvOTELServiceName,
		EnvOTELServiceVersion, EnvOTELEnvironment,
		EnvQuickwitURL, EnvJaegerURL, EnvGrafanaURL, EnvPrometheusURL,
		EnvTimeMethod, EnvTimeServer,
	}
	seen := map[string]int{}
	for _, v := range EnvVars {
		seen[v.Name]++
		if v.Description == "" {
			t.Errorf("%s has no description", v.Name)
		}
	}
	for _, c := range consts {
		if seen[c] != 1 {
			t.Errorf("%s appears %d times in EnvVars, want exactly once", c, seen[c])
		}
	}
	if len(EnvVars) != len(consts) {
		t.Errorf("EnvVars has %d entries for %d consts — remove the stale one", len(EnvVars), len(consts))
	}
}

// A garbage DB number must fail soft to DB 0 with a warning, never to a
// half-parsed client: kpr shares its redis with other tenants (DBs 0-2
// taken elsewhere), so a mistyped DB selecting someone else's keyspace
// is worse than refusing. If this fails, KPR_REDIS_DB=typo silently
// reads the wrong database.
func TestFromEnvInvalidRedisDBFallsBackWithWarning(t *testing.T) {
	for _, raw := range []string{"nope", "-1", "4.5", ""} {
		t.Run("db="+raw, func(t *testing.T) {
			t.Setenv(EnvRedisDB, raw)
			cfg := NewBuilder().FromEnv().Build()
			if raw == "" {
				if cfg.RedisDB != 0 || cfg.RedisDBWarning != nil {
					t.Errorf("unset DB = %d/%v, want 0 with no warning", cfg.RedisDB, cfg.RedisDBWarning)
				}
				return
			}
			if cfg.RedisDB != 0 {
				t.Errorf("DB %q resolved to %d, want fallback 0", raw, cfg.RedisDB)
			}
			if cfg.RedisDBWarning == nil {
				t.Errorf("DB %q produced no warning, want one", raw)
			}
		})
	}
}

// DB 0 is a valid selection, not a missing one: it must resolve
// clean, never warn. If this fails, the boundary between "unset"
// and "explicitly zero" collapsed.
func TestFromEnvZeroRedisDBSelectsZero(t *testing.T) {
	t.Setenv(EnvRedisDB, "0")
	cfg := NewBuilder().FromEnv().Build()
	if cfg.RedisDB != 0 || cfg.RedisDBWarning != nil {
		t.Errorf("DB 0 = %d/%v, want 0 with no warning", cfg.RedisDB, cfg.RedisDBWarning)
	}
}

// The edge rides inside serve, enabled by default: only an explicit
// opt-out closes it. Proof failure still refuses it (no proof, no
// edge) — the switch selects intent, the proof selects safety. If
// this fails, serve either drops its fence silently or refuses to
// run naked when asked.
func TestFromEnvEdgeDefaultsOn(t *testing.T) {
	cfg := NewBuilder().FromEnv().Build()
	if !cfg.EdgeEnabled {
		t.Error("EdgeEnabled = false with KPR_EDGE unset, want default-on")
	}
}

func TestFromEnvEdgeDisables(t *testing.T) {
	for _, raw := range []string{"false", "0", "no"} {
		t.Setenv(EnvEdge, raw)
		if cfg := NewBuilder().FromEnv().Build(); cfg.EdgeEnabled {
			t.Errorf("EdgeEnabled = true with KPR_EDGE=%q, want false", raw)
		}
	}
}

// The builder chain sets every field it names: one pass over the
// rarely-touched setters (edge placement, observability sinks)
// pins the contract. If this fails, a setter writes the wrong
// field (or nothing).
func TestBuilderSetsRareFields(t *testing.T) {
	cfg := NewBuilder().
		WithEdgeAddr("127.0.0.1:9000").
		WithEdgeEnabled(false).
		WithQuickwitURL("http://qw:7280").
		WithJaegerURL("http://jg:4318").
		WithGrafanaURL("http://gf:3000").
		WithPrometheusURL("http://pr:9090").
		Build()
	if cfg.EdgeAddr != "127.0.0.1:9000" || cfg.EdgeEnabled {
		t.Errorf("edge = (%q, %v), want the pinned addr and false", cfg.EdgeAddr, cfg.EdgeEnabled)
	}
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"quickwit", cfg.QuickwitURL, "http://qw:7280"},
		{"jaeger", cfg.JaegerURL, "http://jg:4318"},
		{"grafana", cfg.GrafanaURL, "http://gf:3000"},
		{"prometheus", cfg.PrometheusURL, "http://pr:9090"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// The DB warning reads as a sentence naming raw and default: it
// prints as the startup line, not as a hard error. If this fails,
// the fallback boots silent about what it ignored.
func TestDBWarningNamesRawAndDefault(t *testing.T) {
	err := (&DBWarning{Raw: "bogus", Default: 0}).Error()
	for _, want := range []string{`"bogus"`, "using default"} {
		if !strings.Contains(err, want) {
			t.Errorf("DBWarning = %q, lacks %q", err, want)
		}
	}
}
