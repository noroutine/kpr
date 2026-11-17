package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/resources"
)

// The OTel switch exposes the Prometheus exposition beside
// the JSON metrics: scraped, not linked. If this fails, the
// meter provider runs blind.
func TestServerServesPrometheusWhenOTELEnabled(t *testing.T) {
	testConfig(t)
	ln := loopbackListener(t)
	s := &Server{Listener: ln, OTELEnabled: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	waitFor(t, "http://"+ln.Addr().String()+"/health")

	resp, err := testClient.Get("http://" + ln.Addr().String() + "/metrics/prometheus") //nolint:gosec,noctx // test-only loopback
	if err != nil {
		t.Fatalf("GET /metrics/prometheus: %v", err)
	}
	drainAndClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics/prometheus = %d, want 200 with OTel on", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v after cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after cancel")
	}
	waitShutdown(t, s)
}

// A shutdown with clients still holding connections waits out
// the timeout instead of hanging: the error voices which
// shutdown gave up. Open a raw socket and hold it past the
// grace period — no response is ever written, so Shutdown can
// only time out. If this fails, a stuck client wedges the
// console drain.
func TestServerShutdownErrorOnHeldConnections(t *testing.T) {
	testConfig(t)
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	waitFor(t, "http://"+ln.Addr().String()+"/health")

	held, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("hold a connection: %v", err)
	}
	defer func() { _ = held.Close() }()
	logs := captureLog(t)
	// Cancel with the socket held: the watcher's drain waits
	// out the grace period and voices the timeout instead of
	// hanging. If this fails, a stuck client wedges the
	// console drain.
	cancel()
	waitShutdown(t, s)
	if out := logs.String(); !strings.Contains(out, "shutdown error") {
		t.Errorf("shutdown timeout not logged:\n%s", out)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v after cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after cancel")
	}
}

// A console armed with auth but no token must refuse before
// binding: comparing empty against empty would otherwise serve
// the dashboard to anyone. If this fails, a miswired server
// boots authless.
func TestStartRefusesAuthWithoutToken(t *testing.T) {
	s := &Server{AuthEnabled: true}
	if err := s.Start(context.Background()); err == nil {
		t.Error("Start with auth and no token succeeded, want a refusal before binding")
	}
}

// The gate answers four ways: no credentials, wrong password,
// and wrong user refuse with a Basic challenge; the right pair
// passes through; /health stays open without credentials
// because probes carry none. If this fails, the dashboard is
// either open to anyone or closed to monitoring.
func TestRequireAuthGate(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("in"))
	})
	s := &Server{Token: "tok", AuthEnabled: true}
	h := s.requireAuth(inner)
	cases := []struct {
		name string
		path string
		user string
		pass string
		want int
	}{
		{"no credentials refused", "/", "", "", http.StatusUnauthorized},
		{"wrong password refused", "/", "ops", "nope", http.StatusUnauthorized},
		{"wrong user refused", "/", "root", "tok", http.StatusUnauthorized},
		{"right pair passes", "/", "ops", "tok", http.StatusOK},
		{"health stays open", "/health", "", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.user != "" || tc.pass != "" {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.path, tc.user, rr.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized &&
				!strings.Contains(rr.Header().Get("WWW-Authenticate"), "Basic") {
				t.Error("refusal carries no Basic challenge")
			}
		})
	}
}

// A guarded server gates the whole surface on a live socket:
// no credentials refuse, the right pair passes, and the
// health probe stays open without credentials. If this fails,
// the auth wiring in Start diverges from the gate above.
func TestAuthGatesLiveServer(t *testing.T) {
	testConfig(t)
	ln := loopbackListener(t)
	s := &Server{Listener: ln, Token: "tok", AuthEnabled: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	base := "http://" + ln.Addr().String()
	waitFor(t, base+"/health")

	get := func(path, user, pass string) int {
		req, err := http.NewRequest(http.MethodGet, base+path, nil) //nolint:noctx // test-only loopback
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if user != "" || pass != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := testClient.Do(req) //nolint:gosec // test-only loopback
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		drainAndClose(t, resp)
		return resp.StatusCode
	}
	if got := get("/api/overview", "", ""); got != http.StatusUnauthorized {
		t.Errorf("open overview = %d, want 401", got)
	}
	if got := get("/api/overview", "ops", "tok"); got != http.StatusOK {
		t.Errorf("authed overview = %d, want 200", got)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v after cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after cancel")
	}
	waitShutdown(t, s)
}

// Every server answers /ops/ honestly: 503 with the reason
// when no operator handler is wired, so a dashboard link never
// lands on a 404 from the catch-all. If this fails, an unwired
// prefix misleads instead of refusing.
func TestOpsPrefixAnswers503WhenUnwired(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/ops/", nil)
	rr := httptest.NewRecorder()
	s.opsHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/ops/ = %d, want 503 with the reason", rr.Code)
	}
}

// A wired operator handler serves through the prefix: the 503
// is only the unwired answer. If this fails, mounting an
// operator surface still 503s.
func TestOpsHandlerWired(t *testing.T) {
	s := &Server{Ops: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("acted"))
	})}
	req := httptest.NewRequest(http.MethodGet, "/ops/act", nil)
	rr := httptest.NewRecorder()
	s.opsHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "acted" {
		t.Errorf("/ops/act = %d %q, want 200 acted", rr.Code, rr.Body.String())
	}
}

// A broken edit must sink the reload, not the live snapshot:
// the endpoint voices the refusal, the old bundles keep
// serving, and the error waits on the holder for the
// dashboard. The next clean file applies. If this fails, one
// bad edit takes the running config down with it.
func TestReloadSwapsCleanSnapshotOnly(t *testing.T) {
	dir := t.TempDir()
	good := "apiVersion: kpr/v1alpha1\nkind: TLSBundle\nmetadata:\n  name: web\nspec: {}\n"
	mustWrite(t, filepath.Join(dir, "b.yaml"), []byte(good))
	snap, err := resources.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Resources: resources.NewHolder(snap, func() (*resources.Snapshot, error) {
		return resources.Load(dir)
	})}

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/config/reload", nil)
		rr := httptest.NewRecorder()
		s.reloadHandler(rr, req)
		return rr
	}
	if rr := post(); rr.Code != http.StatusOK {
		t.Fatalf("clean reload = %d, want 200: %s", rr.Code, rr.Body.String())
	} else {
		var res reloadResult
		if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil || res.Applied != 1 {
			t.Errorf("clean reload body = %q, %v; want applied:1", rr.Body.String(), err)
		}
	}
	mustWrite(t, filepath.Join(dir, "b.yaml"), []byte("not: [valid\n"))
	before := s.Resources.Get()
	if rr := post(); rr.Code != http.StatusBadRequest {
		t.Fatalf("broken reload = %d, want 400", rr.Code)
	}
	if s.Resources.Get() != before {
		t.Error("broken reload swapped the live snapshot")
	}
	// The refusal waits on the holder for the overview: the
	// page voices it beside the still-serving old cards.
	testConfig(t)
	overviewReq := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	overviewRR := httptest.NewRecorder()
	s.overviewHandler(overviewRR, overviewReq)
	var model overviewData
	if err := json.Unmarshal(overviewRR.Body.Bytes(), &model); err != nil {
		t.Fatalf("overview unparseable: %v", err)
	}
	if model.ReloadErr == "" {
		t.Error("overview hides the refused reload")
	}
	if len(model.Bundles) != 1 || model.Bundles[0].Name != "web" {
		t.Errorf("overview bundles = %+v, want the old snapshot still serving", model.Bundles)
	}
	if _, err := resources.Load(dir); err == nil {
		t.Fatal("broken dir loads, the fixture is not broken")
	}

	// A holder-less server refuses reloads instead of
	// inventing a config to swap.
	bare := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/config/reload", nil)
	rr := httptest.NewRecorder()
	bare.reloadHandler(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("holder-less reload = %d, want 503", rr.Code)
	}
}

// A reload is a POST: anything else refuses with 405, never a
// load. If this fails, a stray GET from a browser or a prober
// triggers a config reload.
func TestReloadRefusesNonPost(t *testing.T) {
	dir := t.TempDir()
	snap, err := resources.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Resources: resources.NewHolder(snap, func() (*resources.Snapshot, error) {
		return resources.Load(dir)
	})}
	req := httptest.NewRequest(http.MethodGet, "/api/config/reload", nil)
	rr := httptest.NewRecorder()
	s.reloadHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET reload = %d, want 405", rr.Code)
	}
}

// The overview endpoint voices the clock card and every bundle
// from the same model the status command reads. A local clock
// reads unchecked (never a fake skew); a bundle with a real
// leaf reads its judged standing; a refused reload waits on
// the holder for the page. If this fails, the console and the
// CLI disagree — or the page voices numbers it never measured.
func TestOverviewShowsClockAndBundles(t *testing.T) {
	testConfig(t)
	dir := t.TempDir()
	certPEM, keyPEM := bundleFixture(t, time.Now().Add(90*24*time.Hour))
	mustWrite(t, filepath.Join(dir, "c.pem"), certPEM)
	mustWrite(t, filepath.Join(dir, "k.pem"), keyPEM)
	mustWrite(t, filepath.Join(dir, "b.yaml"), []byte(`apiVersion: kpr/v1alpha1
kind: TLSBundle
metadata:
  name: web
spec:
  certFile: `+filepath.Join(dir, "c.pem")+`
  keyFile: `+filepath.Join(dir, "k.pem")+`
  minTLS: "1.3"
`))
	snap, err := resources.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := &Server{Now: func() time.Time { return fixed }, Resources: resources.NewHolder(snap,
		func() (*resources.Snapshot, error) { return resources.Load(dir) })}

	out := getOverview(t, s)
	if out.Clock.Method != "local" || out.Clock.Standing != keeper.StandingUnchecked {
		t.Errorf("clock = %+v, want local/unchecked", out.Clock)
	}
	if len(out.Bundles) != 1 {
		t.Fatalf("bundles = %+v, want the one card", out.Bundles)
	}
	card := out.Bundles[0]
	for _, want := range []string{"ok", "web", "bundles.test", "days left", "1.3"} {
		if !strings.Contains(card.Standing+" "+card.Name+" "+card.Identity+" "+
			card.Lifetime+" "+card.MinTLS, want) {
			t.Errorf("bundle card omits %q: %+v", want, card)
		}
	}
	if out.Source != "unconfigured" {
		t.Errorf("source = %q, want unconfigured with no dir", out.Source)
	}
}

// Standings judge the expiry horizon: expired past NotAfter,
// degraded under thirty days, ok beyond, parked when
// disabled, unproven when no leaf materialized. The thirty-day
// boundary pins twice — a card one day off mislabels a
// renewal window. If this fails, the overview cries wolf or,
// worse, stays quiet past expiry.
func TestStandBundleMatrix(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	leaf := func(notAfter time.Time) *tls.Certificate {
		certPEM, keyPEM := bundleFixture(t, notAfter)
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return &cert
	}
	bundle := func() resources.TLSBundle {
		b := resources.TLSBundle{Name: "web"}
		b.Spec.CertFile = "/srv/tls/web.pem"
		b.Spec.KeyFile = "/srv/tls/web.key"
		return b
	}
	cases := []struct {
		name string
		card BundleCard
		want string
	}{
		{"ok at ninety days", StandBundle("web", bundle(), leaf(now.Add(90*24*time.Hour)), 0, now, nil), "ok"},
		{"degraded at twenty-nine", StandBundle("web", bundle(), leaf(now.Add(29*24*time.Hour)), 0, now, nil), "degraded"},
		{"expired yesterday", StandBundle("web", bundle(), leaf(now.Add(-24*time.Hour)), 0, now, nil), "expired"},
		{"unproven without leaf", StandBundle("web", bundle(), nil, 0, now, nil), "unproven"},
	}
	for _, tc := range cases {
		if tc.card.Standing != tc.want {
			t.Errorf("%s: standing = %q, want %q", tc.name, tc.card.Standing, tc.want)
		}
	}
	parked := resources.TLSBundle{Name: "web"}
	parked.Spec.Enabled = newFalse()
	if got := StandBundle("web", parked, leaf(now.Add(90*24*time.Hour)), 0, now, nil); got.Standing != "parked" {
		t.Errorf("disabled bundle standing = %q, want parked", got.Standing)
	}

	// The card voices the leaf: common name, floored days, and a
	// lifetime line a human reads at a glance. If this fails,
	// the card names the standing but hides what expires.
	healthy := StandBundle("web", bundle(), leaf(now.Add(90*24*time.Hour)), 0, now, nil)
	if healthy.Subject != "bundles.test" {
		t.Errorf("subject = %q, want bundles.test", healthy.Subject)
	}
	if healthy.DaysLeft != 90 || healthy.Lifetime != "90 days left" {
		t.Errorf("lifetime = %d/%q, want 90/\"90 days left\"", healthy.DaysLeft, healthy.Lifetime)
	}

	// Floor, not truncation: a leaf dead eleven hours reads
	// expired yesterday, never "expires today" with an ok
	// standing. If this fails, a dead-at-dawn cert reads
	// healthy all morning.
	if got := StandBundle("web", bundle(), leaf(now.Add(-11*time.Hour)), 0, now, nil); got.Standing != "expired" ||
		got.Lifetime != "expired yesterday" {
		t.Errorf("eleven hours dead = %q/%q, want expired/\"expired yesterday\"", got.Standing, got.Lifetime)
	}

	// A CA file with no leaf is configured, not missing: it
	// reads ok with no lifetime, naming its trust source. If
	// this fails, a trust-only bundle alarms as unproven.
	trust := resources.TLSBundle{Name: "ca"}
	trust.Spec.CAFile = "/srv/tls/ca.pem"
	if got := StandBundle("ca", trust, nil, 0, now, nil); got.Standing != "ok" || !got.TrustOnly ||
		got.Lifetime != "trust only" || got.Identity != "CA /srv/tls/ca.pem" {
		t.Errorf("trust-only card = %+v, want ok trust-only naming the CA", got)
	}

	// No leaf and no trust reads unproven with an explicit
	// identity, never an empty string operators must
	// interpret.
	if got := StandBundle("web", bundle(), nil, 0, now, nil); got.Lifetime != "unknown" ||
		got.Identity != "unreadable leaf" {
		t.Errorf("unproven card = %q/%q, want unknown/unreadable leaf", got.Lifetime, got.Identity)
	}

	// Lifetimes voice every horizon in words: the count while
	// valid, the overdue span past expiry, "trust only" for
	// CA-only bundles naming the trust source.
	for _, tc := range []struct {
		days     int
		lifetime string
	}{
		{1, "1 day left"},
		{0, "expires today"},
		{-2, "expired 2 days ago"},
	} {
		if got := lifetimeText(tc.days); got != tc.lifetime {
			t.Errorf("lifetimeText(%d) = %q, want %q", tc.days, got, tc.lifetime)
		}
	}
	if got := issuerOrUnknown(""); got != "unknown" {
		t.Errorf("empty issuer voices %q, want unknown", got)
	}
	if got := trustIdentity(""); got != "system roots" {
		t.Errorf("empty CA voices %q, want system roots", got)
	}

	// The leaf prefers the parsed Leaf when the loader kept
	// one, else the first chain element; garbage parses to
	// unproven, never a guessed identity.
	withLeaf := leaf(now.Add(90 * 24 * time.Hour))
	withLeaf.Leaf = mustParseLeaf(t, withLeaf)
	if got := StandBundle("web", bundle(), withLeaf, 0, now, nil); got.Standing != "ok" {
		t.Errorf("parsed-leaf bundle = %q, want ok", got.Standing)
	}
	garbage := &tls.Certificate{Certificate: [][]byte{[]byte("not-a-cert")}}
	if got := StandBundle("web", bundle(), garbage, 0, now, nil); got.Standing != "unproven" {
		t.Errorf("garbage-leaf bundle = %q, want unproven", got.Standing)
	}
	if got := StandBundle("web", bundle(), &tls.Certificate{}, 0, now, nil); got.Standing != "unproven" {
		t.Errorf("empty-leaf bundle = %q, want unproven", got.Standing)
	}

	// An empty CN falls back to the first DNS name: some
	// issuers leave the subject blank, and the card must
	// still name the leaf.
	sanKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sanTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		DNSNames:     []string{"san.test"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(90 * 24 * time.Hour),
	}
	sanDER, err := x509.CreateCertificate(rand.Reader, sanTmpl, sanTmpl, &sanKey.PublicKey, sanKey)
	if err != nil {
		t.Fatal(err)
	}
	sanCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sanDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: mustMarshalEC(t, sanKey)}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := StandBundle("web", bundle(), &sanCert, 0, now, nil); got.Subject != "san.test" {
		t.Errorf("blank-CN subject = %q, want the first DNS name", got.Subject)
	}
	// Without the loader-kept Leaf the card falls back to the
	// first chain element: same standing, parsed one step
	// later.
	sanCert.Leaf = nil
	if got := StandBundle("web", bundle(), &sanCert, 0, now, nil); got.Standing != "ok" ||
		got.Subject != "san.test" {
		t.Errorf("chain-parsed bundle = %+v, want ok with the DNS subject", got)
	}
}

func mustMarshalEC(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func mustParseLeaf(t *testing.T, cert *tls.Certificate) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// A filed chain with a hole cards incomplete, not ok and not
// unproven: the bundle names an issuer no filed CA carries,
// so verification would fail either way but never succeed.
// The card names the missing issuer and keeps the leaf facts
// it does have. If this fails, a broken trust anchor cards
// healthy.
func TestStandBundleIncompleteTrust(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	want := []string{"CN=absent-root.test"}
	trust := resources.TLSBundle{Name: "ca"}
	trust.Spec.CAFile = "/srv/tls/ca.pem"
	if got := StandBundle("ca", trust, nil, 0, now, want); got.Standing != "incomplete" ||
		got.Lifetime != "incomplete chain" || !got.TrustOnly {
		t.Errorf("gappy trust card = %+v, want incomplete", got)
	} else if !strings.Contains(got.Identity, "missing issuer CN=absent-root.test") {
		t.Errorf("gappy trust identity = %q, want the missing issuer named", got.Identity)
	}

	bundle := resources.TLSBundle{Name: "web"}
	bundle.Spec.CertFile, bundle.Spec.KeyFile = "/srv/x.pem", "/srv/x.key"
	got := StandBundle("web", bundle, leafAt(t, now.Add(90*24*time.Hour)), 0, now, want)
	if got.Standing != "incomplete" || got.Lifetime != "incomplete chain" {
		t.Errorf("gappy identity card = %+v, want incomplete", got)
	} else if !strings.Contains(got.Identity, "bundles.test") ||
		!strings.Contains(got.Identity, "missing issuer CN=absent-root.test") {
		t.Errorf("gappy identity = %q, want leaf facts plus the hole", got.Identity)
	}
	if got.DaysLeft != 90 || got.Expires != now.Add(90*24*time.Hour).Format("2006-01-02") {
		t.Errorf("gappy card lost its horizon: %+v", got)
	}
}

// leafAt mints a real leaf expiring at notAfter: the horizon
// math needs a parseable certificate, never a hand-built one.
func leafAt(t *testing.T, notAfter time.Time) *tls.Certificate {
	t.Helper()
	certPEM, keyPEM := bundleFixture(t, notAfter)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &cert
}

func newFalse() *bool { b := false; return &b }

func bundleFixture(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "bundles.test"},
		NotBefore:    notAfter.Add(-48 * time.Hour),
		NotAfter:     notAfter,
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

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// countSource scripts the clock transport: every probe lands
// here, so the memoization test pins the probe count without
// any network.
type countSource struct {
	calls  int
	offset time.Duration
	err    error
}

func (c *countSource) Offset(_ context.Context, _ string) (time.Duration, error) {
	c.calls++
	return c.offset, c.err
}

// A server ahead by nine seconds must read back as +9s of
// skew, memoized for a minute: a down time source stalls one
// render per window, not every render. Past the window the
// card re-probes. A silent server reads error, never a fake
// zero. If this fails, the overview voices numbers it never
// measured — or one slow server stalls every page load.
func TestClockCardProbesOncePerWindow(t *testing.T) {
	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithTimeMethod(clock.MethodNTP).
		WithTimeServer("time.test:123").
		Build()))
	fixed := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	src := &countSource{offset: 9 * time.Second}
	s := &Server{Now: func() time.Time { return fixed }, clockSource: src}
	cfg := config.Current()

	first := s.clockCard(context.Background(), cfg)
	second := s.clockCard(context.Background(), cfg)
	if src.calls != 1 {
		t.Fatalf("two renders probed %d times, want the memoized 1", src.calls)
	}
	if first.Skew != second.Skew || first.Skew != 9*time.Second {
		t.Errorf("skew = %v/%v, want the measured +9s twice", first.Skew, second.Skew)
	}
	if first.Standing != keeper.StandingOK {
		t.Errorf("standing = %q, want ok on a live probe", first.Standing)
	}

	// Past the window the card re-probes: the reading stays
	// fresh without a restart.
	s.Now = func() time.Time { return fixed.Add(clockTTL + time.Second) }
	s.clockCard(context.Background(), cfg)
	if src.calls != 2 {
		t.Errorf("post-window render probed %d times total, want 2", src.calls)
	}

	// A silent server reads error, never a fake zero — and the
	// error memoizes the same way.
	src.err = errSilentServer
	s.Now = func() time.Time { return fixed.Add(3 * (clockTTL + time.Second)) }
	if got := s.clockCard(context.Background(), cfg); got.Standing != keeper.StandingUnreachable {
		t.Errorf("silent server standing = %q, want unreachable", got.Standing)
	}
	calls := src.calls
	s.clockCard(context.Background(), cfg)
	if src.calls != calls {
		t.Error("error card re-probed inside its window")
	}
}

var errSilentServer = errTest("time.test:123 silent in test")

type errTest string

func (e errTest) Error() string { return string(e) }

// A TLS-configured console serves HTTPS: the scheme switches
// with the bundle, and the boot line names it. If this fails,
// serve believes it secured a listener that still answers
// plaintext.
func TestServerServesHTTPSWhenTLSConfigured(t *testing.T) {
	testConfig(t)
	certPEM, keyPEM := bundleFixture(t, time.Now().Add(24*time.Hour))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln := loopbackListener(t)
	s := &Server{Listener: ln, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := captureLog(t)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only loopback
		DisableKeepAlives: true,
	}}
	deadline := time.Now().Add(10 * time.Second)
	var resp *http.Response
	for {
		resp, err = client.Get("https://" + ln.Addr().String() + "/health") //nolint:gosec,noctx // test-only loopback
		if err == nil {
			drainAndClose(t, resp)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for https console: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "https://") {
		t.Errorf("boot log lacks the https scheme:\n%s", logs.String())
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v after cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after cancel")
	}
	waitShutdown(t, s)
}
