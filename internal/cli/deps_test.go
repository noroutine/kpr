package cli

import (
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// The shared wiring fails the same way every command did: a dead
// redis refuses naming redis, before anything touches flags or
// backends. If this fails, one command's refusal diverged from the
// rest.
func TestOpenDepsRefusesDeadRedis(t *testing.T) {
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	_, err := openDeps()
	if err == nil {
		t.Fatal("openDeps on dead redis succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "redis") {
		t.Errorf("refusal %q does not name redis", err)
	}
}
