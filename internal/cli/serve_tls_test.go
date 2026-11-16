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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/resources"
)

var httpsTestClient = &http.Client{Transport: &http.Transport{
	TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only loopback
	DisableKeepAlives: true,
}}

// waitForTLS waits for a 200 from an HTTPS endpoint, like
// waitFor does for plaintext: a TLS listener that answers
// plaintext (or nothing) never satisfies it. If this fails,
// the listener is not serving HTTPS.
func waitForTLS(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := httpsTestClient.Get(url) //nolint:gosec,noctx // test-only loopback, bounded by deadline
		if err == nil {
			drainAndClose(t, resp)
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", url)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stopServe SIGTERMs the test process: serve's own signal
// handling drains every listener, and the Run wrapper reports
// shutdown failures instead of hanging. Shared so every live
// serve run stops the same way.
func stopServe(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop after SIGTERM")
	}
}

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
  name: plain
spec:
  certFile: `+filepath.Join(dir, "web.pem")+`
  keyFile: `+filepath.Join(dir, "web.key")+`
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

// servePprof serves until its listener dies: a close voices
// itself in the log instead of reading as a crash. Closing
// first makes the Serve error deterministic — no network, no
// timing. If this fails, a lost pprof listener dies silent.
func TestServePprofVoicesListenerClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	logs := captureLog(t)
	servePprof(ln, addr)
	out := logs.String()
	for _, want := range []string{"Starting pprof on " + addr, "pprof: "} {
		if !strings.Contains(out, want) {
			t.Errorf("servePprof log lacks %q:\n%s", want, out)
		}
	}
}

// A configured dir names the config source in the boot log:
// source dir or unconfigured, the operator reads where the
// bundles came from. If this fails, serve boots on mystery
// config.
func TestServeRunNamesConfigSource(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	t.Setenv(config.EnvConfigDir, t.TempDir())
	setServeAddrs(t, 18350, 18351, 18352)
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	waitFor(t, "http://127.0.0.1:18350/health")
	stopServe(t, done)
	if out := logs.String(); !strings.Contains(out, "config: KPR_CONFIG_DIR=") {
		t.Errorf("no config source in the boot log:\n%s", out)
	}
}

// A named serving bundle puts every listener on HTTPS: the
// boot names the bundle and the floor, and each server logs
// the https scheme it bound. The edge proves off the same
// fixture the other edge tests use. If this fails, a named
// bundle serves plaintext somewhere.
func TestServeRunServesTLSOnEveryListener(t *testing.T) {
	snap := bundleSnapshot(t)
	dir := t.TempDir()
	proven := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(proven, []byte("http:\n  relativeurls: true\n"), 0o600); err != nil {
		t.Fatalf("stage proven config: %v", err)
	}
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	t.Setenv(config.EnvConfigDir, filepath.Dir(snap.Bundles["web"].Spec.CertFile))
	t.Setenv(config.EnvTLSBundle, "web")
	t.Setenv(config.EnvRegistryConfig, proven)
	t.Setenv("KPR_EDGE_ADDR", "127.0.0.1:18363")
	setServeAddrs(t, 18360, 18361, 18362)
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	waitForTLS(t, "https://127.0.0.1:18360/health")
	stopServe(t, done)
	out := logs.String()
	for _, want := range []string{`tls: serving bundle "web"`, "https://", "min TLS 1.3", "edge on 127.0.0.1:18363"} {
		if !strings.Contains(out, want) {
			t.Errorf("boot log lacks %q:\n%s", want, out)
		}
	}
}

// A bundle without a pinned floor serves on Go's default:
// the boot log says so instead of printing an empty floor
// as a promise. If this fails, an unpinned bundle voices
// nothing and the default reads as configured.
func TestServeRunServesUnpinnedFloorAsGoDefault(t *testing.T) {
	snap := bundleSnapshot(t)
	dir := filepath.Dir(snap.Bundles["plain"].Spec.CertFile)
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	t.Setenv(config.EnvConfigDir, dir)
	t.Setenv(config.EnvTLSBundle, "plain")
	setServeAddrs(t, 18386, 18387, 18388)
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	waitForTLS(t, "https://127.0.0.1:18386/health")
	stopServe(t, done)
	if out := logs.String(); !strings.Contains(out, "(min TLS Go default)") {
		t.Errorf("boot log lacks the Go default floor:\n%s", out)
	}
}

// Switched-off servers stay down with a line each: the
// receiver still takes notifications on its own port. If
// this fails, a "false" toggle boots the server anyway, or
// kills the receiver with it.
func TestServeRunTogglesBothServersOff(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	t.Setenv(config.EnvManagementEnabled, "false")
	t.Setenv(config.EnvAppEnabled, "false")
	setServeAddrs(t, 18364, 18365, 18366)
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := testClient.Post("http://127.0.0.1:18366/events", "application/json", strings.NewReader(`{}`)) //nolint:gosec,noctx // test-only loopback, bounded by deadline
		if err == nil {
			drainAndClose(t, resp)
			if resp.StatusCode == http.StatusAccepted {
				break
			}
			t.Fatalf("POST receiver /events = %d, want 202 with both frontends off", resp.StatusCode)
		}
		if time.Now().After(deadline) {
			t.Fatalf("receiver never answered with both frontends off: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopServe(t, done)
	out := logs.String()
	for _, want := range []string{
		"Management console disabled by KPR_MANAGEMENT_ENABLED=false",
		"Application server disabled by KPR_APP_ENABLED=false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("boot log lacks %q:\n%s", want, out)
		}
	}
}

// An opted-in pprof listener starts on loopback beside the
// servers: the boot names it. If this fails, the profiling
// opt-in binds nowhere.
func TestServeRunStartsPprofListener(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	t.Setenv(config.EnvPprofAddr, "127.0.0.1:0")
	setServeAddrs(t, 18367, 18368, 18369)
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	waitFor(t, "http://127.0.0.1:18367/health")
	stopServe(t, done)
	if out := logs.String(); !strings.Contains(out, "Starting pprof on 127.0.0.1:0") {
		t.Errorf("no pprof start in the boot log:\n%s", out)
	}
}

// runServeChild re-execs this test binary with the named
// marker set: serve's fatal paths exit the process, so the
// parent only asserts the exit code and the refusal line.
// stdout and stderr merge — log lines land on stderr.
func runServeChild(t *testing.T, marker string, env map[string]string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=TestServeRunRefuses" + marker + "$"}, args...)...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("child err = %v, want an exit: %s", err, out)
	}
	return string(out), code
}

// A config dir that will not load refuses the boot instead
// of serving env-only settings. If this fails, a broken dir
// boots unconfigured.
func TestServeRunRefusesBrokenConfigDir(t *testing.T) {
	if os.Getenv("KPR_TEST_CHILD") == "brokendir" {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("not: [valid\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(config.EnvConfigDir, dir)
		setServeAddrs(t, 18371, 18372, 18373)
		serveCmd.Run(serveCmd, nil)
		return
	}
	out, code := runServeChild(t, "BrokenConfigDir", map[string]string{"KPR_TEST_CHILD": "brokendir"})
	if code == 0 {
		t.Errorf("serve with a broken dir exited 0, want a fatal refusal: %s", out)
	}
	if !strings.Contains(out, "load resources") {
		t.Errorf("refusal omits the cause:\n%s", out)
	}
}

// A named bundle with no entry refuses the boot: serving
// plaintext while believing in TLS is the failure the guard
// exists for. If this fails, a typo'd bundle serves wrong.
func TestServeRunRefusesGhostBundle(t *testing.T) {
	if os.Getenv("KPR_TEST_CHILD") == "ghost" {
		t.Setenv(config.EnvConfigDir, t.TempDir())
		t.Setenv(config.EnvTLSBundle, "ghost")
		setServeAddrs(t, 18374, 18375, 18376)
		serveCmd.Run(serveCmd, nil)
		return
	}
	out, code := runServeChild(t, "GhostBundle", map[string]string{"KPR_TEST_CHILD": "ghost"})
	if code == 0 {
		t.Errorf("serve with a ghost bundle exited 0, want a fatal refusal: %s", out)
	}
	if !strings.Contains(out, "no such bundle") {
		t.Errorf("refusal omits the cause:\n%s", out)
	}
}

// Explicit console auth without a token refuses the boot:
// opting into operator access must never land open. If this
// fails, "true" without a token boots an open console.
func TestServeRunRefusesAuthWithoutToken(t *testing.T) {
	if os.Getenv("KPR_TEST_CHILD") == "authtoken" {
		t.Setenv(config.EnvConsoleAuth, "true")
		setServeAddrs(t, 18377, 18378, 18379)
		serveCmd.Run(serveCmd, nil)
		return
	}
	out, code := runServeChild(t, "AuthWithoutToken", map[string]string{"KPR_TEST_CHILD": "authtoken"})
	if code == 0 {
		t.Errorf("serve with tokenless auth exited 0, want a fatal refusal: %s", out)
	}
	if !strings.Contains(out, "KPR_CONSOLE_AUTH") {
		t.Errorf("refusal omits the cause:\n%s", out)
	}
}

// A non-loopback pprof bind refuses the boot before binding:
// heap contents must not leave the machine. If this fails,
// a wildcard debug listener opens.
func TestServeRunRefusesNonLoopbackPprof(t *testing.T) {
	if os.Getenv("KPR_TEST_CHILD") == "pprofwide" {
		t.Setenv(config.EnvPprofAddr, "garbage")
		setServeAddrs(t, 18380, 18381, 18382)
		serveCmd.Run(serveCmd, nil)
		return
	}
	out, code := runServeChild(t, "NonLoopbackPprof", map[string]string{"KPR_TEST_CHILD": "pprofwide"})
	if code == 0 {
		t.Errorf("serve with a bad pprof bind exited 0, want a fatal refusal: %s", out)
	}
	if !strings.Contains(out, config.EnvPprofAddr) {
		t.Errorf("refusal omits the cause:\n%s", out)
	}
}

// A pprof bind nothing can hold refuses the boot after the
// loopback check passes: port 99999 parses as loopback but no
// socket takes it. If this fails, a dead debug port boots
// silent about the listener it never opened.
func TestServeRunRefusesUnbindablePprof(t *testing.T) {
	if os.Getenv("KPR_TEST_CHILD") == "pprofport" {
		t.Setenv(config.EnvPprofAddr, "127.0.0.1:99999")
		setServeAddrs(t, 18383, 18384, 18385)
		serveCmd.Run(serveCmd, nil)
		return
	}
	out, code := runServeChild(t, "UnbindablePprof", map[string]string{"KPR_TEST_CHILD": "pprofport"})
	if code == 0 {
		t.Errorf("serve with an unbindable pprof exited 0, want a fatal refusal: %s", out)
	}
	if !strings.Contains(out, "pprof listen") {
		t.Errorf("refusal omits the cause:\n%s", out)
	}
}
