// Package tlscfg loads TLS file material: serving certificates
// (leaf or certbot-style bundle, plus an optional separate chain)
// and trust pools. It parses, never stores: callers keep the
// returned values in their own snapshots. Every failure names
// the file — a typo'd path must refuse the load, not serve
// plaintext or half a chain.
package tlscfg

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// LoadServer reads a serving certificate: certFile holds the leaf,
// or the full chain when the issuer ships bundles (certbot's
// fullchain.pem); chainFile, when set, appends one more chain
// file for split layouts (cert.pem plus chain.pem). Empty
// certFile and keyFile together mean "no TLS configured" and
// return false — callers gate optional TLS on that, never on an
// error string. Exactly one of the pair set is a refusal.
func LoadServer(certFile, keyFile, chainFile string) (tls.Certificate, bool, error) {
	if certFile == "" && keyFile == "" {
		return tls.Certificate{}, false, nil
	}
	if certFile == "" || keyFile == "" {
		return tls.Certificate{}, false, fmt.Errorf("tlscfg: certFile and keyFile go together, got %q and %q",
			certFile, keyFile)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("tlscfg: read cert %s: %w", certFile, err)
	}
	if chainFile != "" {
		chainPEM, err := os.ReadFile(chainFile)
		if err != nil {
			return tls.Certificate{}, false, fmt.Errorf("tlscfg: read chain %s: %w", chainFile, err)
		}
		certPEM = append(certPEM, '\n')
		certPEM = append(certPEM, chainPEM...)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("tlscfg: read key %s: %w", keyFile, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("tlscfg: parse cert %s with key %s: %w", certFile, keyFile, err)
	}
	return cert, true, nil
}

// LoadPool reads a PEM CA bundle for peer verification, or
// returns nil for system roots when caFile is empty. A file
// that parses to zero certificates refuses: an empty trust
// pool verifies nothing, which must never pass as configured.
func LoadPool(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	certs, err := LoadCertificates(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	return pool, nil
}

// LoadCertificates parses every CERTIFICATE block in a PEM CA
// bundle, in file order. Unparseable blocks skip the way
// AppendCertsFromPEM skips them; a file that yields zero
// certificates refuses, like LoadPool. Callers that need the
// roster itself (chain-gap checks) read this; callers that
// need a verification pool read LoadPool.
func LoadCertificates(caFile string) ([]*x509.Certificate, error) {
	raw, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("tlscfg: read CA %s: %w", caFile, err)
	}
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tlscfg: CA %s holds no certificates", caFile)
	}
	return out, nil
}
