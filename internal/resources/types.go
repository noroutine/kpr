// Package resources compiles the config dir: k8s-style YAML
// TLSBundle files into the snapshot serve and the console read.
// Loading validates everything first — unknown kinds, bad
// values, unreadable cert files — and a single error rejects
// the whole load, so a broken edit keeps serving the old
// snapshot instead of half-applying. New kinds arrive as new
// typed specs; the loader refuses what it was never taught,
// never guesses.
package resources

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"slices"
	"strings"

	"nrtn.dev/catalyst/kpr/internal/tlscfg"
)

// APIVersion is the only resource group this loader reads: a
// file naming another version refuses instead of guessing a
// schema it was never written for.
const APIVersion = "kpr/v1alpha1"

// KindTLSBundle is the only kind this loader compiles. Unknown
// kinds refuse the load — a typo'd kind must never silently
// drop a bundle.
const KindTLSBundle = "TLSBundle"

// Meta names a resource. For TLSBundle the name is the bundle
// other config (serve wiring, the console) points at.
type Meta struct {
	Name string `yaml:"name"`
}

// TLSBundleSpec pins credential files: CertFile holds the leaf
// or the full chain (certbot's fullchain.pem), ChainFile
// appends a separate chain for split layouts, KeyFile the key,
// CAFile the trust pool for outbound verification (empty means
// system roots). MinTLS floors the negotiated version for every
// listener using the bundle ("1.2", "1.3", empty means Go's
// default): a floor is the one TLS tuning that never weakens
// security, unlike cipher lists (which Go ignores for 1.3
// anyway).
type TLSBundleSpec struct {
	CertFile  string `yaml:"certFile" json:"certFile"`
	KeyFile   string `yaml:"keyFile" json:"keyFile"`
	ChainFile string `yaml:"chainFile" json:"chainFile"`
	CAFile    string `yaml:"caFile" json:"caFile"`
	MinTLS    string `yaml:"minTLS" json:"minTLS"`
	// Enabled false parks the bundle: serve refuses listeners
	// pointing at it, and the console cards it parked instead
	// of failing the load. Absent means on, like every kind.
	Enabled *bool `yaml:"enabled" json:"enabled"`
}

// The minTLS words: the only versions Go still negotiates.
// Anything older refuses the load instead of downgrading
// every listener behind the bundle.
const (
	MinTLS12 = "1.2"
	MinTLS13 = "1.3"
)

// MinTLSVersion resolves a bundle's floor word to the tls
// version: empty is zero (Go's default), unknown words refuse
// — a typo must fail the load, never silently pin wrong.
func MinTLSVersion(word string) (uint16, error) {
	switch word {
	case "":
		return 0, nil
	case MinTLS12:
		return tls.VersionTLS12, nil
	case MinTLS13:
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("minTLS %q, want 1.2 or 1.3", word)
	}
}

// MinTLSName voices a resolved floor for logs and cards: the
// word back, empty for the Go default — unpinned bundles
// voice nothing, so the default never prints as a promise.
func MinTLSName(v uint16) string {
	switch v {
	case tls.VersionTLS12:
		return MinTLS12
	case tls.VersionTLS13:
		return MinTLS13
	default:
		return ""
	}
}

// TLSBundle is the decoded resource: name plus typed spec.
type TLSBundle struct {
	Name string
	Spec TLSBundleSpec
}

// Snapshot is the compiled config: the declared bundles plus
// their materialized TLS (parsed certs, built pools, resolved
// floors — file typos fail the load, never a live listener).
// Readers share it without locking; reload swaps the pointer
// wholesale.
type Snapshot struct {
	Bundles map[string]TLSBundle
	Certs   map[string]tls.Certificate
	Pools   map[string]*x509.CertPool
	// Floors holds each bundle's resolved minTLS: zero means
	// Go's default, never "no floor".
	Floors map[string]uint16
	// ChainGaps names, per bundle, the certificate issuers its
	// filed CA material cannot resolve — an intermediate whose
	// issuer sits in neither its own file nor any other
	// bundle's. Presence-only: a bundle is either listed (its
	// trust is incomplete) or absent (every issuer resolves).
	// Gaps mark cards, never the load: a broken edit refuses
	// the whole folder, but a filed chain with a hole serves
	// plaintext nowhere and HTTPS with a warning nowhere —
	// it cards incomplete instead.
	ChainGaps map[string][]string
	// caSets holds every bundle's parsed CA roster for the
	// chain check; unexported — readers ask ChainGaps.
	caSets map[string][]*x509.Certificate
}

// Enabled reads an Enabled switch the way every kind means it:
// absent is on, only an explicit false parks the resource.
// Every consumer uses this, never a bare deref.
func Enabled(b *bool) bool { return b == nil || *b }

// Named pairs a resource name with its spec for sorted
// rendering: Go maps range in random order, so the console
// template and any list output iterate these, never the maps.
type Named[T any] struct {
	Name  string
	Value T
}

// SortedEntries lists a name-keyed map by ascending name.
func SortedEntries[T any](m map[string]T) []Named[T] {
	out := make([]Named[T], 0, len(m))
	for name, v := range m {
		out = append(out, Named[T]{Name: name, Value: v})
	}
	slices.SortFunc(out, func(a, b Named[T]) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// materialize parses every bundle's files so typos fail the
// load, not a live listener. Parked bundles materialize too:
// flipping one back on is a pure reload, and a parked bundle
// with a bad path is still a broken edit.
func (s *Snapshot) materialize(name string, spec TLSBundleSpec) error {
	floor, err := MinTLSVersion(spec.MinTLS)
	if err != nil {
		return fmt.Errorf("resources: TLSBundle %q: %w", name, err)
	}
	s.Floors[name] = floor
	cert, ok, err := tlscfg.LoadServer(spec.CertFile, spec.KeyFile, spec.ChainFile)
	if err != nil {
		return fmt.Errorf("resources: TLSBundle %q: %w", name, err)
	}
	if ok {
		s.Certs[name] = cert
	}
	if spec.CAFile == "" {
		return nil
	}
	certs, err := tlscfg.LoadCertificates(spec.CAFile)
	if err != nil {
		return fmt.Errorf("resources: TLSBundle %q: %w", name, err)
	}
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	s.Pools[name] = pool
	s.caSets[name] = certs
	return nil
}

// checkChains marks bundles whose filed CA material cannot
// resolve an intermediate's issuer — first against its own
// file, then against every other bundle's. Self-signed roots
// resolve against themselves, so only true holes land here.
// The union is presence-only by distinguished name: an issuer
// that exists anywhere counts as present, even across files.
func (s *Snapshot) checkChains() {
	union := map[string]bool{}
	for _, certs := range s.caSets {
		for _, c := range certs {
			union[c.Subject.String()] = true
		}
	}
	for name, certs := range s.caSets {
		var missing []string
		filed := map[string]bool{}
		for _, c := range certs {
			issuer := c.Issuer.String()
			if union[issuer] || filed[issuer] {
				continue
			}
			filed[issuer] = true
			missing = append(missing, issuer)
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			s.ChainGaps[name] = missing
		}
	}
}
