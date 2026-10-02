package cli

import (
	"bytes"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// runEnv executes `kpr env` against the real environment and returns
// its rendered output.
func runEnv(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	RootCmd.SetOut(&buf)
	RootCmd.SetArgs([]string{"env"})
	defer RootCmd.SetArgs(nil)
	defer RootCmd.SetOut(nil)
	Execute()
	return buf.String()
}

// A breaking pipe surfaces at the header or the first var: half an
// env table must not read as complete. If this fails, truncated env
// output passes silently.
func TestEnvCommandWriteFailuresSurface(t *testing.T) {
	envCmd.SetOut(&failAfterWriter{n: 0})
	defer envCmd.SetOut(nil)
	if err := envCmd.RunE(envCmd, nil); err == nil {
		t.Error("env header into breaking pipe succeeded, want an error")
	}
	envCmd.SetOut(&failAfterWriter{n: 1})
	defer envCmd.SetOut(nil)
	if err := envCmd.RunE(envCmd, nil); err == nil {
		t.Error("env vars into breaking pipe succeeded, want an error")
	}
}

// Every documented variable must appear on the help screen with its
// description: a var missing here is invisible to operators despite
// being read. If this fails, EnvVars grew without envValue/`kpr env`
// following it.
func TestEnvCommandListsEveryVar(t *testing.T) {
	out := runEnv(t)
	for _, v := range config.EnvVars {
		if !strings.Contains(out, v.Name) {
			t.Errorf("kpr env omits %q", v.Name)
		}
		if !strings.Contains(out, v.Description) {
			t.Errorf("kpr env omits description of %q", v.Name)
		}
	}
}

// The screen must show effective values, not just names: an explicit
// setting and an unset-var default must both render. If this fails,
// `kpr env` documents without reflecting reality.
func TestEnvCommandShowsEffectiveValues(t *testing.T) {
	t.Setenv(config.EnvAppPort, "18080")
	t.Setenv(config.EnvOTELEnabled, "true")
	t.Setenv(config.EnvRedisDB, "4")

	out := runEnv(t)
	for _, want := range []string{"KPR_APP_PORT=18080", "OTEL_ENABLED=true", "KPR_REDIS_DB=4"} {
		if !strings.Contains(out, want) {
			t.Errorf("kpr env missing %q:\n%s", want, out)
		}
	}
}

// The redis password is a secret: `kpr env` must show only whether it is
// set, never the value itself. If this fails, a screen share of `kpr env`
// leaks the credential.
func TestEnvCommandRedactsRedisPassword(t *testing.T) {
	t.Setenv(config.EnvRedisPassword, "s3cret")

	out := runEnv(t)
	if strings.Contains(out, "s3cret") {
		t.Errorf("kpr env leaks KPR_REDIS_PASSWORD value:\n%s", out)
	}
	if !strings.Contains(out, "KPR_REDIS_PASSWORD=set") {
		t.Errorf("kpr env must show KPR_REDIS_PASSWORD presence:\n%s", out)
	}
}
