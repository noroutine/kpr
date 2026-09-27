package config

import (
	"strconv"
	"strings"
	"testing"
	"time"
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
	if cfg.OTELEnabled {
		t.Error("OTELEnabled = true, want false (tracing is opt-in)")
	}
	if cfg.ManagementPortWarning != nil || cfg.AppPortWarning != nil {
		t.Error("clean environment must not produce port warnings")
	}
	if cfg.HTTPReadTimeout != 10*time.Second || cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("timeouts = %v/%v, want 10s/5s", cfg.HTTPReadTimeout, cfg.ShutdownTimeout)
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
	if !cfg.OTELEnabled || cfg.OTLPEndpoint != "tempo:4318" {
		t.Errorf("otel = enabled:%v endpoint:%q", cfg.OTELEnabled, cfg.OTLPEndpoint)
	}
	if cfg.OTELServiceName != "kpr-prod" || cfg.OTELServiceVersion != "v9.9.9" || cfg.OTELEnvironment != "production" {
		t.Errorf("otel identity = %q/%q/%q", cfg.OTELServiceName, cfg.OTELServiceVersion, cfg.OTELEnvironment)
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
	if cfg.RedisAddr != "r:1" || !cfg.OTELEnabled || cfg.OTLPEndpoint != "e:1" {
		t.Errorf("backend/otel = %q/%v/%q", cfg.RedisAddr, cfg.OTELEnabled, cfg.OTLPEndpoint)
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
		EnvRedisAddr, EnvOTELEnabled, EnvOTELEndpoint, EnvOTELServiceName,
		EnvOTELServiceVersion, EnvOTELEnvironment,
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
