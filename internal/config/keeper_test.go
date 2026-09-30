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
	if cfg.SweeperNoDryRun || cfg.CLINoDryRun {
		t.Error("arming = true by default, want implicit dry-run (armed off)")
	}
}

// The compose stack points kpr at the in-stack registry and arms the
// sweeper via the env equivalent of --no-dry-run. If this fails, the
// stack runs against defaults instead of what it ships with.
func TestFromEnvResolvesKeeperVars(t *testing.T) {
	t.Setenv(EnvRegistryURL, "http://registry:5000")
	t.Setenv(EnvSweeperNoDryRun, "true")
	t.Setenv(EnvCLINoDryRun, "true")

	cfg := NewBuilder().FromEnv().Build()
	if cfg.RegistryURL != "http://registry:5000" {
		t.Errorf("RegistryURL = %q", cfg.RegistryURL)
	}
	if !cfg.SweeperNoDryRun || !cfg.CLINoDryRun {
		t.Error("arming = false with both vars true, want armed")
	}
}

// Anything but exactly "true" keeps the safety on: arming the sweeper
// must be deliberate, never a typo. If this fails, "1" or "yes" silently
// arms deletes.
// The two arming vars are independent concerns sharing a shape:
// compose arms the serve loop without arming one-shot commands, so
// a container gc stays a preview unless flagged. If this fails, one
// concern's arming leaks into the other — the original confusion.
func TestArmingVarsAreIndependent(t *testing.T) {
	t.Setenv(EnvSweeperNoDryRun, "true")
	if cfg := NewBuilder().FromEnv().Build(); !cfg.SweeperNoDryRun || cfg.CLINoDryRun {
		t.Errorf("sweeper armed: sweeper=%v cli=%v, want true/false", cfg.SweeperNoDryRun, cfg.CLINoDryRun)
	}
	t.Setenv(EnvSweeperNoDryRun, "")
	t.Setenv(EnvCLINoDryRun, "true")
	if cfg := NewBuilder().FromEnv().Build(); cfg.SweeperNoDryRun || !cfg.CLINoDryRun {
		t.Errorf("cli armed: sweeper=%v cli=%v, want false/true", cfg.SweeperNoDryRun, cfg.CLINoDryRun)
	}
}

func TestNoDryRunNeedsExactTrue(t *testing.T) {
	for _, env := range []string{EnvSweeperNoDryRun, EnvCLINoDryRun} {
		for _, v := range []string{"1", "yes", "TRUE", ""} {
			t.Setenv(env, v)
			cfg := NewBuilder().FromEnv().Build()
			if cfg.SweeperNoDryRun || cfg.CLINoDryRun {
				t.Errorf("%s=%q armed execution, want dry-run kept", env, v)
			}
		}
	}
}
