package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// bundleDir files a config dir with three bundles: web (a
// healthy identity), trust (CA only), and parked (disabled) —
// plus nothing named ghost. The identity outlives the healthy
// horizon so the judge reads ok, not degraded-by-age.
func bundleDir(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "bundles.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	der, err = x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	dir := t.TempDir()
	writePair(t, dir, "web.pem", certPEM)
	writePair(t, dir, "web.key", keyPEM)
	writePair(t, dir, "ca.pem", certPEM)
	writePair(t, dir, "bundles.yaml", []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: web
spec:
  certFile: `+filepath.Join(dir, "web.pem")+`
  keyFile: `+filepath.Join(dir, "web.key")+`
---
apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: trust
spec:
  caFile: `+filepath.Join(dir, "ca.pem")+`
---
apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: parked
spec:
  certFile: `+filepath.Join(dir, "web.pem")+`
  keyFile: `+filepath.Join(dir, "web.key")+`
  enabled: false
`))
	return dir
}

// An unconfigured tree prints no bundles and names its source:
// the backend command reads the same model as the dashboard, so
// the two never disagree about what a row means. If this fails,
// the CLI and the console read different bundles.
func TestCollectBundlesUnconfigured(t *testing.T) {
	cfg := config.NewBuilder().Build()
	got, err := collectBundles(cfg)
	if err != nil {
		t.Fatalf("collectBundles: %v", err)
	}
	if len(got.Bundles) != 0 || got.Source != "unconfigured" {
		t.Errorf("unconfigured = %+v, want no bundles", got)
	}
	if lines := got.Lines(); len(lines) != 1 || lines[0] != "source unconfigured" {
		t.Errorf("lines = %q, want the bare source line", lines)
	}
}

// A filed bundle reads its judged standing through the same
// judge the dashboard uses; the serving bundle resolves like
// serve resolves it. A ghost name, a parked bundle, and a dir
// that will not load all refuse instead of printing guesses.
// If this fails, a typo'd serving name prints an https card it
// cannot back.
func TestCollectBundlesMatrix(t *testing.T) {
	dir := bundleDir(t)
	filed := config.NewBuilder().WithConfigDir(dir).WithTLSBundle("web").Build()
	got, err := collectBundles(filed)
	if err != nil {
		t.Fatalf("collectBundles: %v", err)
	}
	if len(got.Bundles) != 3 {
		t.Fatalf("bundles = %+v, want parked, trust, and web", got.Bundles)
	}
	if got.Bundles[0].Name != "parked" || got.Bundles[0].Standing != "parked" {
		t.Errorf("first = %+v, want the parked card first (sorted)", got.Bundles[0])
	}
	if got.Bundles[1].Name != "trust" || !got.Bundles[1].TrustOnly {
		t.Errorf("trust = %+v, want the trust-only card second", got.Bundles[1])
	}
	if got.Bundles[2].Name != "web" || got.Bundles[2].Standing != "ok" {
		t.Errorf("web = %+v, want the ok identity last", got.Bundles[2])
	}
	if got.Source != "KPR_CONFIG_DIR="+dir {
		t.Errorf("source = %q", got.Source)
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"ghost", "no such bundle"},
		{"parked", "parked"},
	} {
		cfg := config.NewBuilder().WithConfigDir(dir).WithTLSBundle(tc.name).Build()
		if _, err := collectBundles(cfg); err == nil {
			t.Errorf("%s accepted, want a refusal", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s refusal = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// The command renders text lines by default and the bundle model
// as JSON with --json: machines read the cards verbatim. If this
// fails, scripts parse prose or the text drifts from the model.
func TestStatusBundlesCommandRenders(t *testing.T) {
	dir := bundleDir(t)
	t.Setenv(config.EnvConfigDir, dir)
	t.Setenv(config.EnvTLSBundle, "web")

	run := func(args ...string) string {
		var buf strings.Builder
		RootCmd.SetOut(&buf)
		RootCmd.SetArgs(append([]string{"status", "bundles"}, args...))
		defer RootCmd.SetArgs(nil)
		defer RootCmd.SetOut(nil)
		Execute()
		return buf.String()
	}
	text := run()
	for _, want := range []string{"ok bundle web", "bundle trust trust only", "source KPR_CONFIG_DIR="} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}

	var decoded bundleStatus
	if err := json.Unmarshal([]byte(run("--json")), &decoded); err != nil {
		t.Fatalf("json unparseable: %v", err)
	}
	if len(decoded.Bundles) != 3 || decoded.Bundles[2].Name != "web" {
		t.Errorf("json bundles = %+v, want the filed triple sorted", decoded.Bundles)
	}
}
