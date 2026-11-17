package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/keeper"
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

// Without a store or registry the overview still answers, with the
// cards red: a zero Server (like today's tests build) must never 500
// the model. If this fails, keeper wiring breaks the console it
// extends.
func TestKeeperBannerDegradedWithoutBackends(t *testing.T) {
	testConfig(t)
	out := getOverview(t, &Server{})
	if out.Registry.Standing != keeper.StandingUnavailable {
		t.Errorf("registry = %+v, want unavailable with no prober", out.Registry)
	}
	if out.Store.Standing != keeper.StandingUnavailable {
		t.Errorf("store = %+v, want unavailable with no store", out.Store)
	}
	if out.Clock.Standing != keeper.StandingUnchecked {
		t.Errorf("clock = %+v, want unchecked local with no probing", out.Clock)
	}
}

// The edge card carries the live fence posture: closed with no
// gate (disabled or unproven edge), open with the posture serve
// proved. If this fails, the operator can't see whether pushes
// are fenced.
func TestEdgeCardRendersPosture(t *testing.T) {
	testConfig(t)
	render := func(s *Server) overviewData {
		req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
		rr := httptest.NewRecorder()
		s.overviewHandler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		var out overviewData
		if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	if out := render(&Server{}); out.Edge.Standing != keeper.StandingClosed || out.Edge.Open {
		t.Errorf("gateless edge = %+v, want closed", out.Edge)
	}
	if out := render(&Server{Edge: &fence.Gate{Store: store.NewMemStore()}}); !out.Edge.Open || out.Edge.Standing == keeper.StandingClosed {
		t.Errorf("gated edge = %+v, want open", out.Edge)
	}
	// Would-deny: a locked store with zero edge traffic still reads
	// deny — the posture leads the first refused push, not trails
	// it. If this fails, lock/unlock moves nothing on the
	// dashboard until someone pushes.
	locked := store.NewMemStore()
	if err := locked.SetUnlocked(context.Background(), false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if out := render(&Server{Edge: &fence.Gate{Store: locked}, Store: locked}); out.Edge.Standing != keeper.StandingDenied {
		t.Errorf("locked edge = %+v, want deny before any traffic", out.Edge)
	}
	// The other side of the same branch: an unlocked store with an
	// open gate reads pass, never deny. The verdict is pinned on
	// its element (">deny" would be the verdict voicing deny):
	// the stylesheet names the deny bulb, so a whole-body
	// substring would trip on styling, not posture. If this
	// fails, the fence posture cannot tell open from closed.
	open := store.NewMemStore()
	if err := open.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if out := render(&Server{Edge: &fence.Gate{Store: open}, Store: open}); out.Edge.Deny || out.Edge.Standing == keeper.StandingDenied {
		t.Errorf("unlocked edge = %+v, want no deny", out.Edge)
	}
}

// Activity reads like `status activity` — friendly lines, capped
// at ten with the uncapped total beside them — while /api/activity
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
	// A row-less control event (a fence flip) rides the same ring:
	// no repo:tag prefix, the outcome alone. If this fails, control
	// events render as malformed rows.
	_ = s.PushActivity(c, store.Outcome{Reason: "gc-fence", Outcome: "held", At: time.Now().UTC()})
	// A due row with no push stamp renders "unknown", never epoch:
	// the plan shows the reason without inventing an age. If this
	// fails, timeless rows claim 1970.
	_ = s.Record(c, policy.Row{Repo: "timeless", Tag: "v1", Due: true, Reason: "ttl"})
	srv := &Server{Store: s, Registry: liveRegistry(t)}
	out := getOverview(t, srv)
	if len(out.Activity) != keeper.ActivityTail {
		t.Errorf("activity tail holds %d lines, want the capped %d", len(out.Activity), keeper.ActivityTail)
	}
	if out.ActivityTotal != 13 {
		t.Errorf("activity total = %d, want all 13", out.ActivityTotal)
	}
	joined := strings.Join(out.Activity, "\n")
	for _, want := range []string{"app:v1 — deleted (untag), ", "held (gc-fence), "} {
		if !strings.Contains(joined, want) {
			t.Errorf("overview activity missing %q", want)
		}
	}
	// The control event must not wear a row prefix: ": — held" is
	// the row branch misfiring on an empty repo:tag. If this fails,
	// row-less events render as malformed rows.
	if strings.Contains(joined, ": — held") {
		t.Errorf("control event rendered with a row prefix:\n%s", joined)
	}
	if len(out.Plan) != 1 || out.Plan[0].Repo != "timeless" || out.Plan[0].Pushed != "unknown" {
		t.Errorf("plan = %+v, want the timeless row with unknown push", out.Plan)
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
	if len(got.Activity) != 13 {
		t.Fatalf("activity api holds %d records, want all 13 (uncapped)", len(got.Activity))
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

// A broken pipe on the activity feed must not panic the console:
// the encode error is logged and the handler returns. If this
// fails, one wedged watcher takes the console down with it.
func TestActivityEndpointToleratesWriteError(t *testing.T) {
	testConfig(t)
	logs := captureLog(t)
	s := &Server{Store: keeperStore(t)}
	req := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	s.activityHandler(errWriter{header: http.Header{}}, req)
	if n := strings.Count(logs.String(), "Error"); n != 1 {
		t.Errorf("logged %d write errors, want 1 (activity feed)", n)
	}
}

// Only GET serves the feed: anything else is 405, never a dump.
// If this fails, the endpoint answers methods it never defined.
func TestActivityEndpointRejectsNonGet(t *testing.T) {
	s := &Server{Store: keeperStore(t)}
	req := httptest.NewRequest(http.MethodPost, "/api/activity", nil)
	rr := httptest.NewRecorder()
	s.activityHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rr.Code)
	}
}

// A dead activity ring reads 503 with its cause, never 500 and
// never an empty feed pretending health. If this fails, a redis
// hiccup looks like an idle keeper.
func TestActivityEndpointDegradesOnRingError(t *testing.T) {
	testConfig(t)
	s := &Server{Store: errActivityStore{store.NewMemStore()}}
	req := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	rr := httptest.NewRecorder()
	s.activityHandler(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "activity unreadable") {
		t.Errorf("body hides the cause:\n%s", rr.Body.String())
	}
}

// errActivityStore fails the ring read: the backend-outage
// stand-in for the feed.
type errActivityStore struct {
	store.Store
}

func (errActivityStore) Activity(context.Context) ([]store.Outcome, error) {
	return nil, errors.New("redis: connection refused")
}

// A zero stamp degrades to words, never January 1, 1970: the ring
// shows "unknown" beside nothing, the feed likewise. If this
// fails, unwritten clocks render as epoch.
func TestActivityZeroStampReadsUnknown(t *testing.T) {
	testConfig(t)
	s := store.NewMemStore()
	_ = s.PushActivity(context.Background(), store.Outcome{Repo: "app", Tag: "v1",
		Reason: "untag", Outcome: "deleted"})
	srv := &Server{Store: s, Registry: liveRegistry(t)}
	out := getOverview(t, srv)
	if len(out.Activity) != 1 || !strings.Contains(out.Activity[0], "unknown") {
		t.Errorf("activity = %q, want the zero stamp as unknown", out.Activity)
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	apiRR := httptest.NewRecorder()
	srv.activityHandler(apiRR, apiReq)
	if !strings.Contains(apiRR.Body.String(), `"at":"unknown"`) {
		t.Errorf("feed hides the zero stamp:\n%s", apiRR.Body.String())
	}
}

// The redis card names addr and DB even degraded: the operator
// must see which redis is unreachable, not just red. If this
// fails, the card hides the backend it complains about.
// Redis naming now lives in keeper.FetchStoreCard (spike
// milestone 1); the case moved with the function, not dropped.

// Clock, endpoints, and the app link read off the cards: the
// local transport voices unchecked (never a fake zero skew),
// the cards name the endpoints serve was given, and the app
// status URL keeps the browser's host with the app port. If
// this fails, the console guesses where it should voice.
func TestInfoCardsVoiceConfigAndProof(t *testing.T) {
	testConfig(t)
	out := getOverview(t, &Server{
		RegistryURL: "http://registry:5000",
		EdgeAddr:    ":5000",
		Edge:        &fence.Gate{Store: store.NewMemStore()},
	})
	if out.Clock.Standing != keeper.StandingUnchecked || out.Clock.Method != "local" {
		t.Errorf("clock = %+v, want unchecked local", out.Clock)
	}
	if out.ClockNow == "" {
		t.Error("clock carries no server render stamp")
	}
	if out.Registry.URL != "http://localhost:5000" {
		t.Errorf("registry url = %q, want the configured endpoint", out.Registry.URL)
	}
	if out.Edge.EdgeAddr != ":5000" || out.Edge.RegistryURL != "http://registry:5000" {
		t.Errorf("edge addrs = %+v", out.Edge)
	}
	if out.AppStatusURL == "" || !strings.HasSuffix(out.AppStatusURL, ":18080/api/status") {
		t.Errorf("app status url = %q, want the browser host with the app port", out.AppStatusURL)
	}
}

// Local/unreachable transport cases now live in
// keeper.FetchClockCard's matrix (spike milestone 4); the live
// https source case moved with them, not dropped.

// Mount refusal now lives in keeper beside fsStats (spike
// milestone 1); the case moved with the function, not dropped.

// Unproven-on-error now lives in keeper.FetchProofCard (spike
// milestone 2); the case moved with the function, not dropped.
// The index passthrough below still proves the section renders
// the shared card.

// Filesystem size is proof-gated: an unproven dir is nobody's
// store to report on, while a proven one voices capacity. If this
// fails, the console sizes a stranger's disk — or stays mute on
// its own.
// Proof gating now lives in keeper.FetchStoreCard (spike
// milestone 1); the cases moved with the function, not dropped.

// Byte tiers now live in human.Capacity (spike milestone 0);
// the cases moved with the function, not dropped.

// Percent math now lives in human.UsedPercent (spike milestone
// 0); the cases moved with the function, not dropped.

// Nonsense sizes never reach the card: zero total, or free above
// total — including the free==total edge a looser check would let
// through. If this fails, a corrupt Statfs voices petabytes as
// capacity.
// Size guards now live in keeper beside the store card (spike
// milestone 1); the truth tables moved with the functions, not
// dropped.

// Nonsense blocks refuse without a syscall: zero total and
// free-above-total never reach the card, whatever the filesystem
// claims. If this fails, a corrupt Statfs voices petabytes as
// capacity.
// Moved to keeper with sizesFromBlocks (spike milestone 1).

// fsSizes voices block math, not block counts: totals are raw
// blocks times the fragment unit. If this fails, the card sizes
// block counts as bytes.
// Moved to keeper with fsSizes (spike milestone 1).

// Moved to keeper with FetchClockCard (spike milestone 4).

// Skew sign now lives in human.SignedOffset (spike milestone
// 0); the cases moved with the function, not dropped.

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

// The overview shows only what kpr tracks: due rows with reasons
// (the plan), outcome counts, and the activity ring — never a registry
// catalog. If this fails, the console either leaks registry browsing
// or hides the pending work.
func TestKeeperSectionsRenderTrackedState(t *testing.T) {
	testConfig(t)
	out := getOverview(t, &Server{Store: keeperStore(t), Registry: liveRegistry(t)})
	if !out.StoreOK || !out.RegistryOK {
		t.Errorf("banner = store %v registry %v, want both ok", out.StoreOK, out.RegistryOK)
	}
	if len(out.Plan) != 1 || out.Plan[0].Repo != "scratch" || out.Plan[0].Tag != "10m" ||
		out.Plan[0].Reason != "ttl:10m elapsed" {
		t.Errorf("plan = %+v, want the due row with its reason", out.Plan)
	}
	joined := strings.Join(out.Activity, "\n")
	if !strings.Contains(joined, "deleted") {
		t.Errorf("activity hides the outcome:\n%s", joined)
	}
	if out.Tracked != 2 || out.Due != 1 {
		t.Errorf("counters = %d tracked/%d due, want 2/1", out.Tracked, out.Due)
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
	out := getOverview(t, srv)
	if out.Tracked != 4 || out.Due != 3 {
		t.Errorf("counters = %d tracked/%d due, want 4/3", out.Tracked, out.Due)
	}
	if len(out.Activity) != 4 {
		t.Errorf("activity holds %d lines, want one per outcome", len(out.Activity))
	}
	// Order is repo, then tag — fetch it from the JSON array, not
	// from rendered text. If this fails, the plan list lost its
	// sort.
	ordered := []string{"a-repo:c", "a-repo:m", "b-repo:z"}
	if len(out.Plan) != len(ordered) {
		t.Fatalf("plan holds %d rows, want %d: %+v", len(out.Plan), len(ordered), out.Plan)
	}
	for i, want := range ordered {
		if got := out.Plan[i].Repo + ":" + out.Plan[i].Tag; got != want {
			t.Errorf("plan[%d] = %q, want %q", i, got, want)
		}
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
	waitShutdown(t, s)
}

// A denied fence voices deny in the card: the model decides deny
// in keeper, the SPA only colors the word. If this fails, deny
// reads as a calm standing.
func TestEdgeDeniedStandingOnDeny(t *testing.T) {
	testConfig(t)
	locked := store.NewMemStore()
	if err := locked.SetUnlocked(context.Background(), false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	out := getOverview(t, &Server{Edge: &fence.Gate{Store: locked}, Store: locked})
	if out.Edge.Standing != keeper.StandingDenied {
		t.Errorf("edge = %+v, want deny voiced", out.Edge)
	}
}

// stubProbe answers the banner probe without HTTP: the console must
// consume the registry through a small port, not a concrete client.
// If this fails, the dashboard is still coupled to the transport.
type stubProbe struct{ err error }

func (f stubProbe) Reachable(ctx context.Context) error { return f.err }

// A stale deny flip must not survive a live unlock: the store
// reading is the answer wherever a store exists. A refused push
// while locked flips deny true; unlocking after must clear the
// card with no further traffic. If this fails, unlock moves
// nothing on the dashboard until someone pushes.
func TestKeeperSnapshotLiveUnlockClearsStaleDeny(t *testing.T) {
	testConfig(t)
	ctx := context.Background()
	st := store.NewMemStore()
	if err := st.SetUnlocked(ctx, false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	g := &fence.Gate{Store: st}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rr := httptest.NewRecorder()
	g.Wrap(next).ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/v2/repo/manifests/tag", nil))
	if rr.Code != http.StatusLocked {
		t.Fatalf("locked push = %d, want 423 to stage the deny flip", rr.Code)
	}
	if err := st.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	s := &Server{Store: st, Edge: g, Registry: stubProbe{}}
	if d := keeper.FetchEdgeCard(ctx, s.Edge, s.Store, s.EdgeAddr, s.RegistryURL); d.Deny {
		t.Error("snapshot denies after unlock, want the live reading to clear the stale flip")
	}
}

// A reachable registry greens the banner through the port alone: the
// stub never dials, so no loopback server is bound. If this fails,
// the console probes the concrete client instead of its port.
func TestKeeperBannerGreenWithStubRegistry(t *testing.T) {
	testConfig(t)
	s := &Server{Store: keeperStore(t), Registry: stubProbe{}}
	if d := keeper.FetchStatus(context.Background(), s.Store, s.Registry); !d.RegistryOK {
		t.Error("stub-reachable registry renders red, want green")
	}
}
