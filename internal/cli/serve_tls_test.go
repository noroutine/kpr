package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/resources"
)

// Only a loopback bind exposes the pprof debug listener: heap
// contents must not leave the machine. Anything else — a
// wildcard, a remote address, garbage — refuses the boot. If
// this fails, a typo'd pprof bind leaks internals or a valid
// one refuses.
func TestCheckPprofAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:6060", "[::1]:6060"} {
		if err := checkPprofAddr(addr); err != nil {
			t.Errorf("checkPprofAddr(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:6060", "192.0.2.1:6060", "garbage"} {
		err := checkPprofAddr(addr)
		if err == nil {
			t.Errorf("checkPprofAddr(%q) = nil, want refusal", addr)
		} else if !strings.Contains(err.Error(), config.EnvPprofAddr) {
			t.Errorf("refusal %q does not name %s", err, config.EnvPprofAddr)
		}
	}
}

// The serving bundle resolves four ways: empty names plaintext,
// a filed identity bundle resolves with its floor, and unknown
// names, parked bundles, and trust-only bundles refuse. The
// dangerous one is the name that almost resolves — serving
// plaintext while the operator believes they configured TLS,
// or serving a bundle with no identity. If this fails, a
// typo'd name serves wrong instead of refusing.
func TestBundleTLSConfigMatrix(t *testing.T) {
	snap := bundleSnapshot(t)

	if got, floor, err := bundleTLSConfig(snap, ""); err != nil || got != nil || floor != 0 {
		t.Errorf("empty name = %v, %04x, %v; want nil, 0, nil (plaintext)", got, floor, err)
	}
	got, floor, err := bundleTLSConfig(snap, "web")
	if err != nil {
		t.Fatalf("web bundle = %v, want the serving TLS", err)
	}
	if len(got.Certificates) != 1 || floor != tls.VersionTLS13 {
		t.Errorf("web bundle = %d certs floor %04x, want 1 cert and TLS 1.3", len(got.Certificates), floor)
	}
	for name, want := range map[string]string{
		"ghost":  "no such bundle",
		"parked": "parked",
		"trust":  "no identity",
	} {
		if _, _, err := bundleTLSConfig(snap, name); err == nil {
			t.Errorf("%s accepted, want a refusal", name)
		} else if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s refusal = %v, want %q naming the bundle", name, err, want)
		}
	}
}

// bundleSnapshot builds a snapshot with three bundles: web (an
// identity with a 1.3 floor), parked (disabled), and trust (CA
// only) — plus nothing named ghost. Filed through the real
// loader, so the matrix exercises what serve boots on.
func bundleSnapshot(t *testing.T) *resources.Snapshot {
	t.Helper()
	certPEM, keyPEM := servingPair(t)
	dir := t.TempDir()
	writePair(t, dir, "web.pem", certPEM)
	writePair(t, dir, "web.key", keyPEM)
	writePair(t, dir, "ca.pem", certPEM)
	writePair(t, dir, "web.yaml", []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: web
spec:
  certFile: `+filepath.Join(dir, "web.pem")+`
  keyFile: `+filepath.Join(dir, "web.key")+`
  minTLS: "1.3"
---
apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: parked
spec:
  certFile: `+filepath.Join(dir, "web.pem")+`
  keyFile: `+filepath.Join(dir, "web.key")+`
  enabled: false
---
apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: trust
spec:
  caFile: `+filepath.Join(dir, "ca.pem")+`
`))
	snap, err := resources.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// servingPair mints a throwaway identity for the serving
// tests: fast ECDSA, valid for the test only. Files live in
// t.TempDir(), never committed.
func servingPair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "serve.test"},
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
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func writePair(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
