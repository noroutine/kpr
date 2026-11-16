package resources

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Load reads every *.yaml and *.yml file in dir (sorted —
// deterministic errors, deterministic snapshots) into a
// validated, materialized Snapshot. Empty dir is valid and
// compiles to nothing: the unconfigured tree behaves exactly
// as today. Any error rejects the whole load.
func Load(dir string) (*Snapshot, error) {
	snap := &Snapshot{
		Bundles:   map[string]TLSBundle{},
		Certs:     map[string]tls.Certificate{},
		Pools:     map[string]*x509.CertPool{},
		Floors:    map[string]uint16{},
		ChainGaps: map[string][]string{},
		caSets:    map[string][]*x509.Certificate{},
	}
	if dir == "" {
		return snap, nil
	}
	if err := snap.loadDir(dir); err != nil {
		return nil, err
	}
	if err := snap.compile(); err != nil {
		return nil, err
	}
	return snap, nil
}

// compile validates every bundle and materializes its TLS:
// a bad path, broken PEM, or unknown floor word fails the
// load, never a live listener. Bundles compile in sorted
// name order: with two broken bundles the first name wins
// every run, never whichever the map yields first.
func (s *Snapshot) compile() error {
	for _, e := range SortedEntries(s.Bundles) {
		if err := s.materialize(e.Name, e.Value.Spec); err != nil {
			return err
		}
	}
	s.checkChains()
	return nil
}

// loadDir reads resource files recursively: every .yaml/.yml
// under dir, in any nesting. Links resolve and must stay inside
// dir — an escape refuses the whole load, as does a broken link.
// Each resolved file loads once no matter how many names point
// at it. Symlinked directories never descend (find -P behavior):
// link the files, not the folders. Dotfiles and dot-directories
// skip silently (editor state, not config), as do non-regular
// files a read could hang on.
func (s *Snapshot) loadDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("resources: stat dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("resources: %s is not a directory", dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resources: resolve dir %s: %w", dir, err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("resources: resolve dir %s: %w", dir, err)
	}
	// Walk the resolved absolute dir, not the raw spelling:
	// root above is absolute while a relative dir leaves every
	// walked file relative, and no relative path ever carries
	// the absolute prefix — the first file misreads as an
	// escape. Resolving the top dir also loads through a
	// symlinked folder name instead of serving it empty and
	// silent; nested links still never descend. Absolute in,
	// absolute out, the containment check compares like with
	// like.
	seen := map[string]bool{}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("resources: walk %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		return s.loadPath(root, seen, path, d)
	})
}

// loadPath vets one walk entry and loads it when it is a
// contained regular YAML file: dot-entries skip (SkipDir for
// folders), plain directories skip, anything else triages by
// extension and resolves against the folder root.
func (s *Snapshot) loadPath(root string, seen map[string]bool, path string, d fs.DirEntry) error {
	if strings.HasPrefix(filepath.Base(path), ".") {
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if d.IsDir() {
		return nil
	}
	name := filepath.Base(path)
	if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
		return refuseLookalike(path, name)
	}
	return s.loadContained(root, seen, path)
}

// refuseLookalike refuses config-shaped non-YAML files and
// silently skips the rest: a .conf next to the resources is a
// misfire the loader must name, not ignore.
func refuseLookalike(path, name string) error {
	for _, ext := range []string{".conf", ".cfg", ".json", ".toml"} {
		if strings.HasSuffix(name, ext) {
			return fmt.Errorf("resources: %s looks like config but is not a resource file (want .yaml/.yml)", path)
		}
	}
	return nil
}

// loadContained resolves one YAML candidate against the folder
// root and reads it in: escapes refuse, resolved directories
// and non-regular files skip silently, seen inodes load once.
func (s *Snapshot) loadContained(root string, seen map[string]bool, path string) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resources: resolve %s: %w", path, err)
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return fmt.Errorf("resources: %s escapes the resource folder", path)
	}
	if seen[real] {
		return nil
	}
	seen[real] = true
	info, err := os.Stat(real)
	if err != nil {
		return fmt.Errorf("resources: stat %s: %w", path, err)
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("resources: read %s: %w", path, err)
	}
	return s.addFile(path, raw)
}

// indexedHeader is a document's header plus its position in the
// raw stream: the strict pass re-reads the file from the start,
// so it must skip by stream index, not by header count.
type indexedHeader struct {
	index  int
	header header
}

// addFile decodes one multi-doc file in two passes: headers
// for kind dispatch (empty docs skipped), then each document
// re-decoded strictly into its kind's typed struct, so unknown
// fields refuse with the file and document named.
func (s *Snapshot) addFile(name string, raw []byte) error {
	var docs []indexedHeader
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for index := 0; ; index++ {
		var h header
		if err := dec.Decode(&h); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("resources: %s: %w", name, err)
		}
		if h.APIVersion == "" && h.Kind == "" && h.Metadata.Name == "" {
			continue
		}
		docs = append(docs, indexedHeader{index: index, header: h})
	}
	for _, d := range docs {
		if err := s.addStrict(name, d.index, raw, d.header); err != nil {
			return err
		}
	}
	return nil
}
