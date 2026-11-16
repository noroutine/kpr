package resources

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selfSigned mints a throwaway leaf for bundle fixtures: no
// checked-in keys, ever. The CA pool test reuses the same PEM
// as its own trust anchor.
func selfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	der, err = x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	return certPEM, keyPEM
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func bundleDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		writeFile(t, filepath.Join(dir, name), []byte(body))
	}
	return dir
}

// A filed bundle must compile to materialized TLS: the parsed
// cert, the built pool, and the resolved floor the listeners
// will serve. A parked bundle compiles too — flipping it back
// on is a pure reload. If this fails, serve boots without the
// identity the dashboard promises.
func TestLoadCompilesBundleDir(t *testing.T) {
	certPEM, keyPEM := selfSigned(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tls", "web.crt"), certPEM)
	writeFile(t, filepath.Join(dir, "tls", "web.key"), keyPEM)
	writeFile(t, filepath.Join(dir, "tls", "ca.crt"), certPEM)
	writeFile(t, filepath.Join(dir, "bundles", "web.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: web
spec:
  certFile: `+filepath.Join(dir, "tls", "web.crt")+`
  keyFile: `+filepath.Join(dir, "tls", "web.key")+`
  caFile: `+filepath.Join(dir, "tls", "ca.crt")+`
  minTLS: "1.3"
---
apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: parked
spec:
  certFile: `+filepath.Join(dir, "tls", "web.crt")+`
  keyFile: `+filepath.Join(dir, "tls", "web.key")+`
  enabled: false
`))
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if _, ok := snap.Certs["web"]; !ok {
		t.Error("web bundle has no materialized cert")
	}
	if snap.Pools["web"] == nil {
		t.Error("web bundle has no trust pool")
	}
	if snap.Floors["web"] != 0x0304 {
		t.Errorf("web floor = %04x, want TLS 1.3", snap.Floors["web"])
	}
	if _, ok := snap.Certs["parked"]; !ok {
		t.Error("parked bundle must materialize too; re-enabling is a pure reload")
	}
	if got := len(SortedEntries(snap.Bundles)); got != 2 {
		t.Errorf("bundles = %d, want 2", got)
	}
}

// One broken bundle must sink the whole load: serve keeps the
// old snapshot instead of serving half the folder. If this
// fails, a typo'd edit half-applies on reload.
func TestLoadRejectsWholeLoad(t *testing.T) {
	dir := bundleDir(t, map[string]string{"bad.yaml": `apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: bad
spec:
  certFile: /nonexistent/cert.pem
  keyFile: /nonexistent/key.pem
`})
	if snap, err := Load(dir); err == nil || snap != nil {
		t.Errorf("Load = %v, %v; want nil plus a refusal", snap, err)
	}
}

// The loader refuses what it was never taught: wrong versions,
// unknown kinds, missing names, duplicate names, unknown floor
// words, and unknown fields. Each is a distinct broken edit an
// operator will make; each must name the file, never guess.
func TestLoadRefusesBrokenEdits(t *testing.T) {
	cases := map[string]string{
		"wrong apiVersion": `apiVersion: example/v9
kind: TLSBundle
metadata:
  name: a
spec: {}
`,
		"unknown kind": `apiVersion: kpr/v1alpha1
kind: Transport
metadata:
  name: a
spec: {}
`,
		"missing name": `apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: ""
spec: {}
`,
		"unknown floor word": `apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: a
spec:
  minTLS: "1.1"
`,
		"unknown field": `apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: a
spec:
  cipherSuites: [ "TLS_FOO" ]
`,
		"half a TLS pair": `apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: a
spec:
  certFile: /tmp/x.pem
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := bundleDir(t, map[string]string{"r.yaml": body})
			if _, err := Load(dir); err == nil {
				t.Errorf("Load accepted the %s edit", name)
			}
		})
	}

	// The same name twice is an edit collision across files,
	// not a merge: reload must name it, not pick one.
	dir := bundleDir(t, map[string]string{
		"a.yaml":     "apiVersion: kpr/v1alpha1\nkind: TLSBundle\nmetadata:\n  name: dup\nspec: {}\n",
		"sub/b.yaml": "apiVersion: kpr/v1alpha1\nkind: TLSBundle\nmetadata:\n  name: dup\nspec: {}\n",
	})
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate Load err = %v, want a duplicate refusal", err)
	}
}

// No folder configured — or a folder that was never created —
// is the unconfigured tree, not an error: serve runs env-only.
// If this fails, a fresh checkout refuses to boot.
func TestLoadEmptyAndMissingDir(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "never-created")} {
		snap, err := Load(dir)
		if err != nil {
			t.Errorf("Load(%q) = %v, want empty and clean", dir, err)
		} else if len(snap.Bundles) != 0 {
			t.Errorf("Load(%q) bundles = %d, want 0", dir, len(snap.Bundles))
		}
	}
}

// A failed reload must keep serving the old snapshot and park
// the error where the dashboard reads it; the next clean load
// clears it. If this fails, one broken edit takes the running
// config down with it.
func TestHolderReloadKeepsOldOnFailure(t *testing.T) {
	certPEM, keyPEM := selfSigned(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "c.pem"), certPEM)
	writeFile(t, filepath.Join(dir, "k.pem"), keyPEM)
	writeFile(t, filepath.Join(dir, "good.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: web
spec:
  certFile: `+filepath.Join(dir, "c.pem")+`
  keyFile: `+filepath.Join(dir, "k.pem")+`
`))
	snap, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHolder(snap, func() (*Snapshot, error) { return Load(dir) })
	writeFile(t, filepath.Join(dir, "good.yaml"), []byte("not: [valid\n"))
	if err := h.Reload(); err == nil {
		t.Fatal("Reload accepted a broken edit")
	}
	if h.Get() != snap {
		t.Error("failed reload swapped the live snapshot")
	}
	if h.LoadErr() == nil {
		t.Error("failed reload left no error for the dashboard")
	}
	writeFile(t, filepath.Join(dir, "good.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: web
spec:
  certFile: `+filepath.Join(dir, "c.pem")+`
  keyFile: `+filepath.Join(dir, "k.pem")+`
`))
	if err := h.Reload(); err != nil {
		t.Fatalf("Reload = %v", err)
	}
	if h.LoadErr() != nil {
		t.Errorf("clean reload left LoadErr = %v", h.LoadErr())
	}
}

// Only the documented floor words resolve: empty stays Go's
// default, 1.2 and 1.3 pin, anything older refuses. If this
// fails, a typo silently weakens every listener at once.
func TestMinTLSVersionWords(t *testing.T) {
	if v, err := MinTLSVersion(""); err != nil || v != 0 || MinTLSName(v) != "" {
		t.Errorf(`MinTLSVersion("") = %04x, %v; want zero, nil, silent`, v, err)
	}
	for word, want := range map[string]uint16{"1.2": 0x0303, "1.3": 0x0304} {
		v, err := MinTLSVersion(word)
		if err != nil || v != want || MinTLSName(v) != word {
			t.Errorf("MinTLSVersion(%q) = %04x, %v; want %04x voiced back", word, v, err, want)
		}
	}
	if _, err := MinTLSVersion("1.1"); err == nil {
		t.Error("minTLS 1.1 accepted; only 1.2 and 1.3 negotiate")
	}
}

// The enabled switch answers one way: absent is on, only an
// explicit false parks. Every consumer reads through this,
// never a bare deref — a parked bundle is a decision, not an
// omission. If this fails, a missing key disables, or a
// `false` enables.
func TestEnabledSwitchSemantics(t *testing.T) {
	if !Enabled(nil) {
		t.Error("Enabled(nil) = false, want on (absent means on)")
	}
	on := true
	if !Enabled(&on) {
		t.Error("Enabled(true) = false, want on")
	}
	off := false
	if Enabled(&off) {
		t.Error("Enabled(false) = true, want parked")
	}
}

// Editor state and foreign files live next to real resources:
// dotfiles skip silently, plain non-YAML skips silently, and a
// config-shaped lookalike (.conf) refuses by name. If this
// fails, an editor backup breaks the load, or a misfired
// `.conf` loads as nothing.
func TestLoadSkipsNoiseRefusesLookalikes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".swp.yaml"), []byte("x: 1\n"))
	writeFile(t, filepath.Join(dir, "notes.txt"), []byte("todo\n"))
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with noise = %v, want empty and clean", err)
	}
	if len(snap.Bundles) != 0 {
		t.Errorf("bundles = %d, want 0 (noise is not config)", len(snap.Bundles))
	}
	writeFile(t, filepath.Join(dir, "router.conf"), []byte("x\n"))
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "router.conf") {
		t.Errorf("lookalike Load err = %v, want the file named", err)
	}
}

// A link pointing outside the folder refuses the whole load:
// config must come from the folder, never from wherever a
// link wanders. If this fails, a stray symlink silently feeds
// the snapshot from outside the reviewed tree.
func TestLoadRefusesEscape(t *testing.T) {
	dir := t.TempDir()
	outside, err := os.CreateTemp("", "escape-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(outside.Name()) }()
	if _, err := outside.WriteString("apiVersion: kpr/v1alpha1\nkind: TLSBundle\nmetadata:\n  name: x\nspec: {}\n"); err != nil {
		t.Fatal(err)
	}
	_ = outside.Close()
	if err := os.Symlink(outside.Name(), filepath.Join(dir, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("escape Load err = %v, want an escape refusal", err)
	}
}

// A file is not a folder: pointing the loader at one refuses
// with the path named, instead of walking it as nothing. If
// this fails, a typo'd ConfigDir silently serves the
// unconfigured tree.
func TestLoadRefusesFileAsDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "lonely.yaml")
	writeFile(t, file, []byte("kind: TLSBundle\n"))
	if _, err := Load(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("Load(file) err = %v, want a not-a-directory refusal", err)
	}
}

// Dot-directories skip whole: editor state and tooling droppings
// never break the load, even when they nest real-looking YAML.
// If this fails, a stray .backup folder takes the boot down.
func TestLoadSkipsDotDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".backup", "old.yaml"), []byte("kind: TLSBundle\n"))
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with a dot-dir = %v, want clean", err)
	}
	if len(snap.Bundles) != 0 {
		t.Errorf("bundles = %d, want 0 (dot-dirs never descend)", len(snap.Bundles))
	}
}

// Two names for one file load once: the second sighting skips
// by inode instead of filing the bundle twice and refusing
// its own duplicate. If this fails, linking one file under
// two names — the documented layout — breaks the load.
func TestLoadDedupesSymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "real.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: web
spec: {}
`))
	if err := os.Symlink(filepath.Join(dir, "real.yaml"), filepath.Join(dir, "alias.yaml")); err != nil {
		t.Fatal(err)
	}
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with an aliased file = %v, want one load", err)
	}
	if len(snap.Bundles) != 1 {
		t.Errorf("bundles = %d, want 1 (second sighting skips)", len(snap.Bundles))
	}
}

// Symlinked directories never descend: link the files, not
// the folders. If this fails, a linked folder either loads
// outside the reviewed tree or errors on the traversal.
func TestLoadSkipsSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sub, "inner.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: inner
spec: {}
`))
	if err := os.Symlink(sub, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with a linked dir = %v, want clean", err)
	}
	if len(snap.Bundles) != 1 {
		t.Errorf("bundles = %d, want only the directly-nested one", len(snap.Bundles))
	}
}

// A broken link refuses with its own name: EvalSymlinks fails
// before any containment check runs. If this fails, a dead
// link loads as nothing — or panics the walk.
func TestLoadRefusesBrokenLink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "gone.yaml"), filepath.Join(dir, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Errorf("broken link Load err = %v, want a resolve refusal", err)
	}
}

// Empty documents skip: separator runs (`---` with nothing
// between) carry no resource and must not shift the strict
// pass that follows. If this fails, a trailing `---` breaks
// the load or misattributes the next document.
func TestLoadSkipsEmptyDocs(t *testing.T) {
	dir := bundleDir(t, map[string]string{"r.yaml": "---\n---\napiVersion: kpr/v1alpha1\nkind: TLSBundle\nmetadata:\n  name: web\nspec: {}\n"})
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with empty docs = %v, want clean", err)
	}
	if len(snap.Bundles) != 1 {
		t.Errorf("bundles = %d, want the one real document", len(snap.Bundles))
	}
}

// mintCA issues a CA certificate: self-signed when parent is
// nil, otherwise signed by it. Key travels with the cert so
// tests mint parent before child and file chains with known
// holes.
func mintCA(t *testing.T, cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) ([]byte, *ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key, leaf
}

// A filed chain that resolves — an intermediate meeting its
// issuer in the same file — leaves no gaps. If this fails,
// complete trust reads broken.
func TestChainGapsCompleteChains(t *testing.T) {
	dir := t.TempDir()
	rootPEM, rootKey, _ := mintCA(t, "root.test", nil, nil)
	midPEM, _, _ := mintCA(t, "mid.test", rootFrom(t, rootPEM), rootKey)
	_ = midPEM
	writeFile(t, filepath.Join(dir, "chain.pem"), append(midPEM, rootPEM...))
	writeFile(t, filepath.Join(dir, "solo.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: solo
spec:
  caFile: `+filepath.Join(dir, "chain.pem")+`
`))
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if len(snap.ChainGaps) != 0 {
		t.Errorf("ChainGaps = %v, want none for complete chains", snap.ChainGaps)
	}
}

// rootFrom parses the just-minted root back: the test files
// PEM, and parsing must agree with what was filed.
func rootFrom(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("minted root does not decode")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// An intermediate whose issuer sits nowhere — not its own
// file, not a sibling bundle's — marks exactly its bundle,
// naming the missing issuer. Duplicates collapse: two
// orphans of the same root name it once. If this fails, a
// hole in filed trust cards healthy.
func TestChainGapsHoleInFile(t *testing.T) {
	dir := t.TempDir()
	_, rootKey, root := mintCA(t, "absent-root.test", nil, nil)
	mid1, _, _ := mintCA(t, "mid-one.test", root, rootKey)
	mid2, _, _ := mintCA(t, "mid-two.test", root, rootKey)
	writeFile(t, filepath.Join(dir, "holes.pem"), append(mid1, mid2...))
	writeFile(t, filepath.Join(dir, "holes.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: holes
spec:
  caFile: `+filepath.Join(dir, "holes.pem")+`
`))
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	gaps, ok := snap.ChainGaps["holes"]
	if !ok || len(gaps) != 1 || !strings.Contains(gaps[0], "absent-root.test") {
		t.Errorf("ChainGaps[holes] = %v, want the one missing root", snap.ChainGaps)
	}
}

// The union spans bundles: an intermediate resolves against a
// sibling bundle's root. Trust split across files on purpose
// — roots in one bundle, intermediates in another — stays
// quiet. If this fails, factored trust reads broken.
func TestChainGapsResolvedAcrossBundles(t *testing.T) {
	dir := t.TempDir()
	rootPEM, rootKey, root := mintCA(t, "shared-root.test", nil, nil)
	midPEM, _, _ := mintCA(t, "mid.test", root, rootKey)
	writeFile(t, filepath.Join(dir, "mid.pem"), midPEM)
	writeFile(t, filepath.Join(dir, "root.pem"), rootPEM)
	writeFile(t, filepath.Join(dir, "a.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: a
spec:
  caFile: `+filepath.Join(dir, "mid.pem")+`
`))
	writeFile(t, filepath.Join(dir, "b.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: b
spec:
  caFile: `+filepath.Join(dir, "root.pem")+`
`))
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if len(snap.ChainGaps) != 0 {
		t.Errorf("ChainGaps = %v, want none: the root lives next door", snap.ChainGaps)
	}
}
