package cli

import (
	"testing"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
)

// The method var becomes exactly one transport: unknown values can
// only arrive from hand-built configs (FromEnv falls soft), and
// they must land on the harmless default, not crash.
func TestClockSourceMapsMethods(t *testing.T) {
	for _, tc := range []struct {
		method clock.Method
		want   clock.Source
	}{
		{clock.MethodLocal, clock.Local{}},
		{clock.MethodHTTPS, clock.HTTPS{}},
		{clock.MethodNTP, clock.NTP{}},
		{"sundial", clock.Local{}},
	} {
		cfg := config.NewBuilder().Build()
		cfg.TimeMethod = tc.method
		if got := clockSource(cfg); got != tc.want {
			t.Errorf("method %q -> %T, want %T", tc.method, got, tc.want)
		}
	}
}
