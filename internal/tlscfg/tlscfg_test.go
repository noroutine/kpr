package tlscfg

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
	"testing"
	"time"
)

// selfSigned mints a throwaway CA-less leaf: fast ECDSA, valid
// for the test only. Files, never fixtures — no checked-in keys,
// ever.
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

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The roster call returns every parsable leaf in file order
// and skips what a pool would skip: a non-certificate block
// is not a refusal, but a file that yields nothing is. Chain
// checks read the roster; verification reads the pool.
func TestLoadCertificatesListsRoster(t *testing.T) {
	dir := t.TempDir()
	one, _ := selfSigned(t)
	two, _ := selfSigned(t)
	caFile := writeFile(t, dir, "ca.pem", append(append([]byte("not a pem block\n"), one...), two...))
	certs, err := LoadCertificates(caFile)
	if err != nil {
		t.Fatalf("LoadCertificates = %v, want two certs", err)
	}
	if len(certs) != 2 {
		t.Fatalf("LoadCertificates returned %d certs, want 2 in file order", len(certs))
	}
	if _, err := x509.ParseCertificate(certs[0].Raw); err != nil {
		t.Errorf("first roster entry does not parse: %v", err)
	}
	if _, err := LoadCertificates(writeFile(t, dir, "nocert.pem", []byte("no certs here"))); err == nil {
		t.Error("certless file listed, want a refusal")
	}
	if _, err := LoadCertificates(filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("missing file listed, want a refusal")
	}
	// A non-certificate block and a garbled certificate are
	// skipped, not fatal: the roster voices what parses, and a
	// file with nothing parseable still refuses.
	junkKey := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("garbage")})
	junkCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("garbage")})
	mixed, err := LoadCertificates(writeFile(t, dir, "mixed.pem", append(append(junkKey, junkCert...), one...)))
	if err != nil {
		t.Fatalf("mixed bundle refused: %v", err)
	}
	if len(mixed) != 1 {
		t.Errorf("mixed bundle listed %d certs, want the one parseable leaf", len(mixed))
	}
}

// A bundle file (leaf plus chain in one) must load as one
// certificate: this is the certbot fullchain.pem layout, and a
// refusal here forces every deployment onto split files.
func TestLoadServerBundle(t *testing.T) {
	dir := t.TempDir()
	leaf, key := selfSigned(t)
	chain, _ := selfSigned(t)
	certFile := writeFile(t, dir, "fullchain.pem", append(append([]byte{}, leaf...), chain...))
	keyFile := writeFile(t, dir, "privkey.pem", key)
	cert, ok, err := LoadServer(certFile, keyFile, "")
	if err != nil || !ok {
		t.Fatalf("LoadServer(bundle) = %v, %v; want a cert", ok, err)
	}
	if len(cert.Certificate) != 2 {
		t.Errorf("bundle holds %d certificates, want leaf plus chain", len(cert.Certificate))
	}
	if _, err := x509.ParseCertificate(cert.Certificate[0]); err != nil {
		t.Errorf("leaf does not parse: %v", err)
	}
}

// A split layout (leaf plus a separate chain file) must load the
// same way: certbot's cert.pem plus chain.pem, and any operator
// layout that keeps the two apart.
func TestLoadServerSplit(t *testing.T) {
	dir := t.TempDir()
	leaf, key := selfSigned(t)
	chain, _ := selfSigned(t)
	certFile := writeFile(t, dir, "cert.pem", leaf)
	chainFile := writeFile(t, dir, "chain.pem", chain)
	keyFile := writeFile(t, dir, "key.pem", key)
	cert, ok, err := LoadServer(certFile, keyFile, chainFile)
	if err != nil || !ok {
		t.Fatalf("LoadServer(split) = %v, %v; want a cert", ok, err)
	}
	if len(cert.Certificate) != 2 {
		t.Errorf("split holds %d certificates, want leaf plus chain", len(cert.Certificate))
	}
}

// Empty paths mean unconfigured, not broken: optional TLS gates
// on the flag, and a refusal here would force every plaintext
// deployment to invent dummy files.
func TestLoadServerEmptyIsUnconfigured(t *testing.T) {
	if _, ok, err := LoadServer("", "", ""); err != nil || ok {
		t.Errorf("LoadServer(empty) = %v, %v; want false, nil", ok, err)
	}
}

// Half a pair, missing files, garbage, and a mismatched key must
// all refuse with the file named: serving half a chain (or the
// wrong key) silently downgrades every connection through it.
func TestLoadServerRefusals(t *testing.T) {
	dir := t.TempDir()
	leaf, key := selfSigned(t)
	_, otherKeyPEM := selfSigned(t)
	otherKey := writeFile(t, dir, "other-key.pem", otherKeyPEM)
	certFile := writeFile(t, dir, "cert.pem", leaf)
	keyFile := writeFile(t, dir, "key.pem", key)
	for name, args := range map[string][3]string{
		"half pair":   {certFile, "", ""},
		"missing":     {filepath.Join(dir, "nope.pem"), keyFile, ""},
		"bad chain":   {certFile, keyFile, filepath.Join(dir, "nope.pem")},
		"garbage":     {writeFile(t, dir, "junk.pem", []byte("not pem")), keyFile, ""},
		"wrong key":   {certFile, otherKey, ""},
		"missing key": {certFile, filepath.Join(dir, "nope-key.pem"), ""},
	} {
		if _, _, err := LoadServer(args[0], args[1], args[2]); err == nil {
			t.Errorf("%s loaded, want a refusal", name)
		}
	}
}

// A CA file must load into a pool that verifies the leaf it
// signed; empty means system roots, and a file with no certs
// refuses instead of verifying nothing.
func TestLoadPool(t *testing.T) {
	dir := t.TempDir()
	leaf, _ := selfSigned(t)
	// The leaf is self-signed, so it is its own CA for the test.
	caFile := writeFile(t, dir, "ca.pem", leaf)
	pool, err := LoadPool(caFile)
	if err != nil || pool == nil {
		t.Fatalf("LoadPool = %v, %v; want a pool", pool, err)
	}
	if pool == nil {
		t.Fatal("nil pool")
	}
	if pool2, err := LoadPool(""); err != nil || pool2 != nil {
		t.Errorf("LoadPool(empty) = %v, %v; want nil, nil", pool2, err)
	}
	if _, err := LoadPool(writeFile(t, dir, "empty.pem", []byte("no certs here"))); err == nil {
		t.Error("certless CA file loaded, want a refusal")
	}
	if _, err := LoadPool(filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("missing CA file loaded, want a refusal")
	}
}
