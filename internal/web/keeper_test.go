package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

func keeperStore(t *testing.T) *store.MemStore {
	t.Helper()
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: time.Now().UTC().Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:b",
		PushedAt: time.Now().UTC().Add(-time.Hour)})
	_ = s.PushActivity(c, store.Outcome{Repo: "scratch", Tag: "10m",
		Reason: "ttl:10m elapsed", Outcome: "deleted", At: time.Now().UTC()})
	return s
}

func liveRegistry(t *testing.T) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return registry.NewClient(srv.URL)
}

// Without a store or registry the console still renders, with the
// banner red: a zero Server (like today's tests build) must never 500
// the dashboard. If this fails, keeper wiring breaks the console it
// extends.
func TestKeeperBannerDegradedWithoutBackends(t *testing.T) {
	testConfig(t)
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.indexHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Keeper", "unreachable"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q (degraded banner)", want)
		}
	}
	// The console never sweeps, so it advertises no sweep posture:
	// armed/dry-run lives in the CLI that runs the pass. If this
	// fails, sweep safety wording crept back into a console that
	// owns none of it.
	for _, gone := range []string{"dry-run", "armed"} {
		if strings.Contains(body, gone) {
			t.Errorf("dashboard advertises %q (sweep posture the console doesn't own)", gone)
		}
	}
}

// The gateway gets its own place: closed with no gate (disabled
// or unproven edge), open with the live fence posture when serve
// proves it. If this fails, the operator can't see whether pushes
// are fenced.
func TestGatewaySectionRendersPosture(t *testing.T) {
	testConfig(t)
	render := func(s *Server) string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		s.indexHandler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		return rr.Body.String()
	}
	if body := render(&Server{}); !strings.Contains(body, "Gateway") || !strings.Contains(body, "closed") {
		t.Error("dashboard without a gate must render the gateway closed")
	}
	body := render(&Server{Edge: &edge.Gate{Store: store.NewMemStore()}})
	for _, want := range []string{"Gateway", "open", "pass"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard with a gate missing %q (open fence)", want)
		}
	}
	// Would-deny: a locked store with zero edge traffic still reads
	// deny — the posture leads the first refused push, not trails
	// it. If this fails, lock/unlock moves nothing on the
	// dashboard until someone pushes.
	locked := store.NewMemStore()
	if err := locked.SetUnlocked(context.Background(), false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if body := render(&Server{Edge: &edge.Gate{Store: locked}, Store: locked}); !strings.Contains(body, "deny") {
		t.Error("dashboard with a locked store must read deny before any traffic")
	}
}

// Activity ages read human beside the exact stamp: the operator
// glances ("5m ago") without losing precision. Zero and future
// stamps degrade to words, never a huge duration. If this fails,
// the console sends the operator back to timestamp arithmetic.
func TestHumanAgeReadsLikeCLI(t *testing.T) {
	now := time.Now().UTC()
	if got := humanAge(now.Add(-90 * time.Second)); got != "1m30s ago" {
		t.Errorf("humanAge(-90s) = %q, want 1m30s ago", got)
	}
	if got := humanAge(now.Add(time.Hour)); got != "0s ago" {
		t.Errorf("humanAge(future) = %q, want clamped 0s ago", got)
	}
	if got := humanAge(time.Time{}); got != "unknown" {
		t.Errorf("humanAge(zero) = %q, want unknown", got)
	}
}

// Activity reads like `store status` in a code block — friendly
// lines, capped at ten and said out loud — while /api/activity
// serves every record with precise stamps. If this fails, the
// console either hides the cap or loses precision.
func TestActivitySectionRendersStatusShape(t *testing.T) {
	testConfig(t)
	s := store.NewMemStore()
	c := context.Background()
	for i := 0; i < 12; i++ {
		_ = s.PushActivity(c, store.Outcome{Repo: "app", Tag: "v1",
			Reason: "untag", Outcome: "deleted", At: time.Now().UTC()})
	}
	srv := &Server{Store: s, Registry: liveRegistry(t)}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.indexHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"<pre>", "last 10 of 12", "/api/activity",
		"app:v1 — deleted (untag), "} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard activity missing %q", want)
		}
	}
	section := body[strings.Index(body, ">Activity</div>"):]
	if end := strings.Index(section, "</pre>"); end >= 0 {
		section = section[:end]
	}
	if strings.Contains(section, "<li>") {
		t.Error("dashboard activity still renders bullets, want the code block")
	}
	// Placement: the card lives in Keeper, not its own section. If
	// this fails, activity drifted back out of Keeper.
	if keeper, activity := strings.Index(body, "🧹 Keeper"), strings.Index(body, ">Activity</div>"); activity < keeper {
		t.Error("Activity card renders outside Keeper")
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	apiRR := httptest.NewRecorder()
	srv.activityHandler(apiRR, apiReq)
	if apiRR.Code != http.StatusOK {
		t.Fatalf("activity api = %d, want 200", apiRR.Code)
	}
	var got struct {
		Activity []struct {
			At      string `json:"at"`
			Outcome string `json:"outcome"`
		} `json:"activity"`
	}
	if err := json.Unmarshal(apiRR.Body.Bytes(), &got); err != nil {
		t.Fatalf("activity api unparseable: %v", err)
	}
	if len(got.Activity) != 12 {
		t.Fatalf("activity api holds %d records, want all 12 (uncapped)", len(got.Activity))
	}
	if _, err := time.Parse(time.RFC3339, got.Activity[0].At); err != nil {
		t.Errorf("activity api at = %q, want a precise stamp", got.Activity[0].At)
	}

	bare := &Server{}
	bareReq := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	bareRR := httptest.NewRecorder()
	bare.activityHandler(bareRR, bareReq)
	if bareRR.Code != http.StatusServiceUnavailable {
		t.Errorf("storeless activity api = %d, want 503", bareRR.Code)
	}
}

// Clock, endpoints, and capacity read off the cards: the local
// transport voices unchecked (never a fake zero skew), the cards
// name the endpoints serve was given, and the file card stays
// silent about size until a proof says whose dir it is. If this
// fails, the console guesses where it should voice.
func TestInfoCardsVoiceConfigAndProof(t *testing.T) {
	testConfig(t)
	srv := &Server{
		RegistryURL: "http://registry:5000",
		EdgeAddr:    ":5000",
		Edge:        &edge.Gate{Store: store.NewMemStore()},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.indexHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Clock", "local", "machine clock", "unchecked",
		"Server time", "as kpr sees it",
		"http://registry:5000", ":5000 → http://registry:5000",
		`href="http://example.com:18080/api/status"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q (clock/endpoints)", want)
		}
	}
}

// The local transport voices itself without touching the network:
// machine clock, no skew, unchecked. If this fails, the card
// either fakes a zero skew or leaves the transport unnamed.
func TestClockSnapshotVoicesLocal(t *testing.T) {
	testConfig(t)
	method, server, skew, note := clockSnapshot(context.Background())
	if method != "local" || server != "machine clock" || skew != "—" || note != "unchecked" {
		t.Errorf("local snapshot = %q/%q/%q/%q, want local transport voiced", method, server, skew, note)
	}
}

// Filesystem size is proof-gated: an unproven dir is nobody's
// store to report on, while a proven one voices capacity. If this
// fails, the console sizes a stranger's disk — or stays mute on
// its own.
func TestFileStoreFsNoteGatedOnProof(t *testing.T) {
	testConfig(t)
	ctx := context.Background()
	st := store.NewFileStore(t.TempDir())
	cfg := config.Current()
	if got := storeSnapshot(ctx, st, true, cfg, true); got.FsNote == "" {
		t.Error("proven file store voices no size, want free/total")
	} else if !strings.Contains(got.FsNote, "free of ") || !strings.Contains(got.FsNote, "% used)") {
		t.Errorf("FsNote = %q, want '<free> free of <total> (<pct>%% used)'", got.FsNote)
	}
	if got := storeSnapshot(ctx, st, true, cfg, false); got.FsNote != "" {
		t.Errorf("unproven file store voices %q, want silence", got.FsNote)
	}
	if got := storeSnapshot(ctx, nil, false, cfg, true); got.FsNote != "" {
		t.Errorf("storeless snapshot voices %q, want silence", got.FsNote)
	}
}

// Byte tiers stay operator-shaped: whole units above ten, one
// decimal below, MB under a gig. If this fails, the capacity
// glance reads like a raw byte count.
func TestFmtBytesTiers(t *testing.T) {
	for _, tc := range []struct {
		n    uint64
		want string
	}{
		{500 * 1024 * 1024, "500 MB"},
		{5 * 1024 * 1024 * 1024, "5.0 GB"},
		{91 * 1024 * 1024 * 1024, "91 GB"},
		{6300 * 1024 * 1024 * 1024, "6.2 TB"},
	} {
		if got := fmtBytes(tc.n); got != tc.want {
			t.Errorf("fmtBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// The app-status link keeps the browser's host and swaps the port
// (the endpoint lives on the other listener — a relative href
// would 404). Brackets survive IPv6, emptiness stays empty. If
// this fails, the console links a 404 or a malformed URL.
func TestAppStatusURLKeepsBrowserHost(t *testing.T) {
	for _, tc := range []struct {
		hostport string
		want     string
	}{
		{"localhost:9300", "http://localhost:18080/api/status"},
		{"example.com", "http://example.com:18080/api/status"},
		{"[::1]:9300", "http://[::1]:18080/api/status"},
		{"", ""},
	} {
		if got := appStatusURL(tc.hostport, 18080); got != tc.want {
			t.Errorf("appStatusURL(%q) = %q, want %q", tc.hostport, got, tc.want)
		}
	}
}

// The dashboard shows only what kpr tracks: due rows with reasons
// (the plan), outcome counts, and the activity ring — never a registry
// catalog. If this fails, the console either leaks registry browsing
// or hides the pending work.
func TestKeeperSectionsRenderTrackedState(t *testing.T) {
	testConfig(t)
	s := &Server{Store: keeperStore(t), Registry: liveRegistry(t)}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.indexHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"scratch", "10m", "ttl:10m elapsed", // the plan, with reason
		"deleted", // activity outcome
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// The probe cell anchored on its label: both banner cells share the
	// same markup (and "unreachable" contains "reachable"), so neither
	// a bare word nor a bare cell proves the REGISTRY row is green.
	registryGreen := regexp.MustCompile(`Registry</div>\s*<div class="value">reachable</div>`)
	if !registryGreen.MatchString(body) {
		t.Error("registry banner cell is not green")
	}
	if strings.Contains(body, "/v2/app/tags/list") {
		t.Error("dashboard links registry catalog browsing, want tracked-state only")
	}
}

// Counters count every outcome kind and the plan sorts by repo then
// tag: the dashboard is the operator's at-a-glance state, so a swapped
// comparator or a miscounted outcome must fail here, not in production.
// If this fails, the console misreports what the next sweep would do.
func TestKeeperCountersAndPlanOrder(t *testing.T) {
	testConfig(t)
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "b-repo", Tag: "z", Digest: "sha256:1",
		PushedAt: time.Now().UTC().Add(-time.Hour), Due: true, Reason: "keep-n:exceeds 10"})
	_ = s.Record(c, policy.Row{Repo: "a-repo", Tag: "m", Digest: "sha256:2",
		PushedAt: time.Now().UTC().Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"})
	_ = s.Record(c, policy.Row{Repo: "a-repo", Tag: "c", Digest: "sha256:4",
		PushedAt: time.Now().UTC().Add(-2 * time.Hour), Due: true, Reason: "keep-n:exceeds 10"})
	_ = s.Record(c, policy.Row{Repo: "a-repo", Tag: "a", Digest: "sha256:3",
		PushedAt: time.Now().UTC().Add(-time.Hour)})
	for _, outcome := range []string{"deleted", "planned", "failed", "untracked"} {
		_ = s.PushActivity(c, store.Outcome{Repo: "a-repo", Tag: "m",
			Reason: "r", Outcome: outcome, At: time.Now().UTC()})
	}
	srv := &Server{Store: s, Registry: liveRegistry(t)}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.indexHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"4 / 3",
		"performed 1 · planned 1 · failed 1 · untracked 1",
		"a-repo:c", "a-repo:m", "b-repo:z",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// Order is checked inside the Plan section: the Activity card
	// above it names the same repos, so a whole-body index would
	// hit the activity lines first. If this fails, the plan list
	// lost its repo-then-tag sort.
	plan := body[strings.Index(body, "📋 Plan"):]
	ordered := []string{"a-repo:c", "a-repo:m", "b-repo:z"}
	last := -1
	for _, want := range ordered {
		at := strings.Index(plan, want)
		if at <= last {
			t.Errorf("plan order broken at %q (repo, then tag)", want)
			break
		}
		last = at
	}
}

// The console never sweeps: POST /api/sweep is not served (the sweep
// pass lives in the CLI, serve serves endpoints only). If this fails,
// a sweep trigger crept back into the console and the serve/sweep
// split is broken.
func TestConsoleDoesNotServeSweepEndpoint(t *testing.T) {
	testConfig(t)
	ln := loopbackListener(t)
	s := &Server{Listener: ln, Store: keeperStore(t)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	waitFor(t, "http://"+ln.Addr().String()+"/health")

	resp, err := testClient.Post( //nolint:gosec,noctx // test-only loopback
		"http://"+ln.Addr().String()+"/api/sweep", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/sweep: %v", err)
	}
	drainAndClose(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /api/sweep = %d, want 404 (sweep lives in the CLI)", resp.StatusCode)
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
}

// stubProbe answers the banner probe without HTTP: the console must
// consume the registry through a small port, not a concrete client.
// If this fails, the dashboard is still coupled to the transport.
type stubProbe struct{ err error }

func (f stubProbe) Reachable(ctx context.Context) error { return f.err }

// A reachable registry greens the banner through the port alone: the
// stub never dials, so no loopback server is bound. If this fails,
// the console probes the concrete client instead of its port.
func TestKeeperBannerGreenWithStubRegistry(t *testing.T) {
	testConfig(t)
	s := &Server{Store: keeperStore(t), Registry: stubProbe{}}
	if d := s.keeperSnapshot(context.Background()); !d.RegistryOK {
		t.Error("stub-reachable registry renders red, want green")
	}
}
