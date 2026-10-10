package proof

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A config with relativeurls and no host produces: the edge cannot
// bypass. If this fails, proven configs stopped opening.
func TestProveRelativeURLsProducesWhenRelative(t *testing.T) {
	path := writeRegistryConfig(t, "http:\n  addr: :5000\n  relativeurls: true\n")
	p, err := ProveRelativeURLs(path)
	if err != nil {
		t.Fatalf("relative config = %v, want the proof", err)
	}
	if p == nil {
		t.Fatal("relative config produced nil, want RelativeURLs")
	}
}

// relativeurls off refuses: absolute backend Locations would
// walk clients around the edge. If this fails, bypass configs
// started opening.
func TestProveRelativeURLsRefusesWhenAbsolute(t *testing.T) {
	path := writeRegistryConfig(t, "http:\n  addr: :5000\n")
	p, err := ProveRelativeURLs(path)
	if p != nil {
		t.Errorf("absolute config produced %v, want nothing", p)
	}
	if !errors.Is(err, ErrRelativeURLsOff) {
		t.Errorf("absolute config error = %v, want ErrRelativeURLsOff", err)
	}
}

// A set host refuses even with relativeurls on: host overrides
// the knob silently, so the proof must exclude it. If this
// fails, dead-knob configs started opening.
func TestProveRelativeURLsRefusesWhenHostSet(t *testing.T) {
	path := writeRegistryConfig(t, "http:\n  addr: :5000\n  relativeurls: true\n  host: https://registry.example.com\n")
	p, err := ProveRelativeURLs(path)
	if p != nil {
		t.Errorf("hosted config produced %v, want nothing", p)
	}
	if !errors.Is(err, ErrHostOverridesURLs) {
		t.Errorf("hosted config error = %v, want ErrHostOverridesURLs", err)
	}
}

// An unreadable config propagates: unknown is not relative. If
// this fails, missing files started producing.
func TestProveRelativeURLsPropagatesUnreadable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yml")
	p, err := ProveRelativeURLs(missing)
	if p != nil {
		t.Errorf("missing config produced %v, want nothing", p)
	}
	if err == nil {
		t.Error("missing config error = nil, want the read error")
	}
}

// Unparseable YAML propagates: guessing is not proving. If this
// fails, broken configs started producing.
func TestProveRelativeURLsPropagatesUnparseable(t *testing.T) {
	path := writeRegistryConfig(t, "http:\n  relativeurls: [true,\n")
	p, err := ProveRelativeURLs(path)
	if p != nil {
		t.Errorf("broken config produced %v, want nothing", p)
	}
	if err == nil {
		t.Error("broken config error = nil, want the parse error")
	}
}

// The zero value is nothing: without the config there are no
// relative URLs. If this fails, nil stopped meaning nothing.
func TestRelativeURLsNilIsNothing(t *testing.T) {
	var p RelativeURLs
	if p != nil {
		t.Errorf("zero RelativeURLs = %v, want nil", p)
	}
}

func writeRegistryConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	return path
}
