package config

import (
	"testing"
)

// The registry kpr companions defaults to the local dev-stack address;
// dry-run stays on unless explicitly armed off. If this fails, a bare
// `kpr serve` points at the wrong registry or deletes by default.
func TestKeeperDefaults(t *testing.T) {
	cfg := NewBuilder().Build()
	if cfg.RegistryURL != "http://localhost:5000" {
		t.Errorf("RegistryURL = %q, want the dev-stack default", cfg.RegistryURL)
	}
	if cfg.NoDryRun {
		t.Error("NoDryRun = true by default, want implicit dry-run (armed off)")
	}
}

// The compose stack points kpr at the in-stack registry and arms the
// sweeper via the env equivalent of --no-dry-run. If this fails, the
// stack runs against defaults instead of what it ships with.
func TestFromEnvResolvesKeeperVars(t *testing.T) {
	t.Setenv(EnvRegistryURL, "http://registry:5000")
	t.Setenv(EnvNoDryRun, "true")

	cfg := NewBuilder().FromEnv().Build()
	if cfg.RegistryURL != "http://registry:5000" {
		t.Errorf("RegistryURL = %q", cfg.RegistryURL)
	}
	if !cfg.NoDryRun {
		t.Error("NoDryRun = false with KPR_NO_DRY_RUN=true, want armed")
	}
}

// Anything but exactly "true" keeps the safety on: arming the sweeper
// must be deliberate, never a typo. If this fails, "1" or "yes" silently
// arms deletes.
func TestNoDryRunNeedsExactTrue(t *testing.T) {
	for _, v := range []string{"1", "yes", "TRUE", ""} {
		t.Setenv(EnvNoDryRun, v)
		if cfg := NewBuilder().FromEnv().Build(); cfg.NoDryRun {
			t.Errorf("KPR_NO_DRY_RUN=%q armed the sweeper, want dry-run kept", v)
		}
	}
}
