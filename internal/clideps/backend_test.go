package clideps

import (
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// clearStoreEnv unsets every backend signal: derivation reads
// explicitness, and a leaked CI variable would select a backend the
// case never asked for.
func clearStoreEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvStore, "")
	t.Setenv(config.EnvStoreDir, "")
	t.Setenv(config.EnvRedisAddr, "")
}

// The backend derives from explicit signals, never resolved values
// (KPR_REDIS_ADDR carries a default that must not count):
// KPR_STORE is authoritative and must agree with backend-specific
// variables, KPR_STORE_DIR alone selects file, KPR_REDIS_ADDR alone
// selects redis, silence selects file. If this fails, mixed
// signals boot a guessed backend or refuse a coherent one.
func TestResolveStoreBackend(t *testing.T) {
	for _, tc := range []struct {
		name            string
		env             map[string]string
		wantBackend     string
		wantDir         string
		wantErrContains string
	}{
		{"silence selects file", nil, "file", "kpr", ""},
		{"explicit redis addr", map[string]string{config.EnvRedisAddr: "r:6379"}, "redis", "", ""},
		{"explicit store redis", map[string]string{config.EnvStore: "redis"}, "redis", "", ""},
		{"explicit store file defaults dir", map[string]string{config.EnvStore: "file"}, "file", "kpr", ""},
		{"store dir alone selects file", map[string]string{config.EnvStoreDir: "/x/kpr"}, "file", "/x/kpr", ""},
		{"unknown store refuses", map[string]string{config.EnvStore: "sqlite"}, "", "", "unknown"},
		{"file plus redis addr conflicts", map[string]string{config.EnvStore: "file", config.EnvRedisAddr: "r:6379"}, "", "", "conflicts"},
		{"redis plus store dir conflicts", map[string]string{config.EnvStore: "redis", config.EnvStoreDir: "/x"}, "", "", "conflicts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearStoreEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			backend, dir, err := ResolveStoreBackend()
			if tc.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("resolve = (%q, %q, %v), want error containing %q", backend, dir, err, tc.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if backend != tc.wantBackend || dir != tc.wantDir {
				t.Errorf("resolve = (%q, %q), want (%q, %q)", backend, dir, tc.wantBackend, tc.wantDir)
			}
		})
	}
}
