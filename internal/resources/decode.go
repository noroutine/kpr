package resources

import (
	"bytes"
	"fmt"

	"go.yaml.in/yaml/v3"
)

type header struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   Meta   `yaml:"metadata"`
}

// typedDoc is the one strictly-decoded document: the same
// header plus the kind's spec. One struct per kind keeps
// KnownFields refusal precise instead of approximate.
type bundleDoc struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   Meta          `yaml:"metadata"`
	Spec       TLSBundleSpec `yaml:"spec"`
}

// addStrict consumes the first n documents loosely, then
// decodes document n+1 strictly into its kind's struct.
func (s *Snapshot) addStrict(file string, n int, raw []byte, h header) error {
	where := fmt.Sprintf("%s doc %d", file, n+1)
	if h.APIVersion != APIVersion {
		return fmt.Errorf("resources: %s: apiVersion %q, want %q", where, h.APIVersion, APIVersion)
	}
	if h.Metadata.Name == "" {
		return fmt.Errorf("resources: %s: metadata.name is required", where)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var skip any
	// No error return: the header pass already decoded
	// these same bytes with the same parser, and a loose
	// decode fails only where that header decode failed
	// first — unreachable on error by construction.
	for i := 0; i < n; i++ {
		_ = dec.Decode(&skip)
	}
	dec.KnownFields(true)
	return s.addDoc(dec, where, h.Kind)
}

// addDoc decodes one strict document of a known kind and files
// it, refusing duplicates: the same name twice is an edit
// collision, not a merge.
func (s *Snapshot) addDoc(dec *yaml.Decoder, where, kind string) error {
	switch kind {
	case KindTLSBundle:
		return s.addBundle(dec, where)
	default:
		return fmt.Errorf("resources: %s: unknown kind %q", where, kind)
	}
}

// addBundle decodes one bundle document and stores it, refusing
// a name already taken.
func (s *Snapshot) addBundle(dec *yaml.Decoder, where string) error {
	var d bundleDoc
	if err := strictDecode(dec, where, &d); err != nil {
		return err
	}
	if _, dup := s.Bundles[d.Metadata.Name]; dup {
		return fmt.Errorf("resources: %s: duplicate TLSBundle %q", where, d.Metadata.Name)
	}
	s.Bundles[d.Metadata.Name] = TLSBundle{Name: d.Metadata.Name, Spec: d.Spec}
	return nil
}

func strictDecode(dec *yaml.Decoder, where string, d any) error {
	if err := dec.Decode(d); err != nil {
		return fmt.Errorf("resources: %s: %w", where, err)
	}
	return nil
}
