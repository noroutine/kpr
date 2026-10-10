package proof

import (
	"errors"
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// ErrRelativeURLsOff refuses a registry config without
// relativeurls: upstream Locations would name the backend and
// walk edge clients around the proxy — a fence bypass.
var ErrRelativeURLsOff = errors.New("registry config lacks http.relativeurls: upstream Locations would name the backend — set `relativeurls: true` (and leave `host` empty)")

// ErrHostOverridesURLs refuses a registry config with http.host
// set: host silently overrides relativeurls (absolute URLs
// always), so the knob would be dead config the proof must not
// vouch for.
var ErrHostOverridesURLs = errors.New("registry config sets http.host: host silently overrides relativeurls — leave `host` empty so Locations stay relative")

// RelativeURLs is proven edge addressing: the mounted registry
// config emits relative Location headers and sets no host to
// override them. Sealed like every evidence: the only
// inhabitant comes from ProveRelativeURLs, and the zero value
// is nil.
type RelativeURLs interface {
	sealed()
}

type relativeURLs struct{}

func (relativeURLs) sealed() {}

// ProveRelativeURLs reads the mounted registry config — the
// same file gc resolves store paths from — and produces only when
// http.relativeurls is true and http.host is empty. Unreadable
// or unparseable propagates: unknown is not relative.
func ProveRelativeURLs(configPath string) (RelativeURLs, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("prove relativeurls: read %s: %w", configPath, err)
	}
	var cfg struct {
		HTTP struct {
			RelativeURLs bool   `yaml:"relativeurls"`
			Host         string `yaml:"host"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("prove relativeurls: parse %s: %w", configPath, err)
	}
	if cfg.HTTP.Host != "" {
		return nil, ErrHostOverridesURLs
	}
	if !cfg.HTTP.RelativeURLs {
		return nil, ErrRelativeURLsOff
	}
	return relativeURLs{}, nil
}
