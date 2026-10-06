package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/fencing"
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
	body := render(&Server{Edge: &fencing.Gate{Store: store.NewMemStore()}})
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
	if body := render(&Server{Edge: &fencing.Gate{Store: locked}, Store: locked}); !strings.Contains(body, "deny") {
		t.Error("dashboard with a locked store must read deny before any traffic")
	}
	// The other side of the same branch: an unlocked store with an
	// open gate reads pass, never deny. If this fails, the fence
	// posture cannot tell open from closed.
	open := store.NewMemStore()
	if err := open.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if body := render(&Server{Edge: &fencing.Gate{Store: open}, Store: open}); !strings.Contains(body, "pass") || strings.Contains(body, "deny") {
		t.Error("dashboard with an unlocked store must read pass, not deny")
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
	// A row-less control event (a fence flip) rides the same ring:
	// no repo:tag prefix, the outcome alone. If this fails, control
	// events render as malformed rows.
	_ = s.PushActivity(c, store.Outcome{Reason: "gc-fence", Outcome: "held", At: time.Now().UTC()})
	// A due row with no push stamp renders "unknown", never epoch:
	// the plan shows the reason without inventing an age. If this
	// fails, timeless rows claim 1970.
	_ = s.Record(c, policy.Row{Repo: "timeless", Tag: "v1", Due: true, Reason: "ttl"})
	srv := &Server{Store: s, Registry: liveRegistry(t)}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.indexHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"<pre>", "last 10 of 13", "/api/activity",
		"app:v1 — deleted (untag), ", "held (gc-fence), ",
		"timeless:v1", "unknown"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard activity missing %q", want)
		}
	}
	// The control event must not wear a row prefix: ": — held" is
	// the row branch misfiring on an empty repo:tag. If this fails,
	// row-less events render as malformed rows.
	if strings.Contains(body, ": — held") {
		t.Errorf("control event rendered with a row prefix:\n%s", body)
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
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.indexHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unknown") {
		t.Errorf("dashboard hides the zero stamp:\n%s", rr.Body.String())
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
func TestRedisStoreCardNamesAddrAndDB(t *testing.T) {
	testConfig(t)
	rs := store.NewRedisStore("redis:6379", "", 0)
	defer func() { _ = rs.Close() }()
	got := storeSnapshot(context.Background(), rs, false, config.Current(), false)
	if got.Name != "redis" || got.Detail != "redis:6379 db 0" {
		t.Errorf("redis card = %q/%q, want redis with addr and DB", got.Name, got.Detail)
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
		Edge:        &fencing.Gate{Store: store.NewMemStore()},
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

// An unreadable mount gets no ink, not an error card: Statfs
// failure reads empty, and the card drops the size line. If this
// fails, a dead mount breaks the store card.
func TestFsStatsEmptyOnUnreadableMount(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	if got := fsStats(absent); got != "" {
		t.Errorf("fsStats(absent) = %q, want empty", got)
	}
	if _, _, ok := fsSizes(absent); ok {
		t.Error("fsSizes(absent) ok, want refusal")
	}
}

// A silent sentinel reads unproven, never stale: the proof card
// shows no generation until the registry serves one. If this
// fails, a dead sentinel borrows yesterday's generation.
func TestSentinelSnapshotUnprovenOnError(t *testing.T) {
	testConfig(t)
	s := &Server{Sentinel: stubSentinelAPI{err: errors.New("registry: 500")}}
	got := s.sentinelSnapshot(context.Background())
	if got.Proven || got.Gen != "" {
		t.Errorf("snapshot = %+v, want unproven with no generation", got)
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
// decimal below, MB under a gig — boundaries included, so a tier
// edge never flips units. If this fails, the capacity glance reads
// like a raw byte count.
func TestFmtBytesTiers(t *testing.T) {
	const kb = 1024
	for _, tc := range []struct {
		n    uint64
		want string
	}{
		{500 * 1024 * 1024, "500 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{5 * 1024 * 1024 * 1024, "5.0 GB"},
		{10 * 1024 * 1024 * 1024, "10 GB"},
		{91 * 1024 * 1024 * 1024, "91 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
		{10 * 1024 * 1024 * 1024 * 1024, "10 TB"},
		{6300 * 1024 * 1024 * 1024, "6.2 TB"},
		{10*1024*1024*1024*1024 - kb, "10.0 TB"},
	} {
		if got := fmtBytes(tc.n); got != tc.want {
			t.Errorf("fmtBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// The percent math is the only unprobed part of the capacity card:
// whole percent used, free==total reading zero. If this fails, the
// card's percent lies while free and total read true.
func TestUsedPercent(t *testing.T) {
	for _, tc := range []struct {
		total, free uint64
		want        uint64
	}{
		{100, 30, 70},
		{100, 100, 0},
		{100, 0, 100},
		{3, 1, 66},
	} {
		if got := usedPercent(tc.total, tc.free); got != tc.want {
			t.Errorf("usedPercent(%d, %d) = %d, want %d", tc.total, tc.free, got, tc.want)
		}
	}
}

// Nonsense sizes never reach the card: zero total, or free above
// total — including the free==total edge a looser check would let
// through. If this fails, a corrupt Statfs voices petabytes as
// capacity.
func TestSaneSizes(t *testing.T) {
	for _, tc := range []struct {
		total, free uint64
		want        bool
	}{
		{0, 0, false},
		{100, 101, false},
		{100, 100, true},
		{100, 30, true},
	} {
		if got := saneSizes(tc.total, tc.free); got != tc.want {
			t.Errorf("saneSizes(%d, %d) = %v, want %v", tc.total, tc.free, got, tc.want)
		}
	}
	// The live path over a real dir stays sane: block math that
	// underflows units reads zero total and refuses.
	if total, free, ok := fsSizes(t.TempDir()); !ok || total == 0 || free > total {
		t.Errorf("fsSizes(tempdir) = %d/%d/%v, want sane sizes", total, free, ok)
	}
}

// Nonsense blocks refuse without a syscall: zero total and
// free-above-total never reach the card, whatever the filesystem
// claims. If this fails, a corrupt Statfs voices petabytes as
// capacity.
func TestSizesFromBlocksRefusesNonsense(t *testing.T) {
	if _, _, ok := sizesFromBlocks(0, 0, 4096); ok {
		t.Error("sizesFromBlocks(0 blocks) ok, want refusal")
	}
	if _, _, ok := sizesFromBlocks(100, 200, 4096); ok {
		t.Error("sizesFromBlocks(free above total) ok, want refusal")
	}
	if total, free, ok := sizesFromBlocks(100, 30, 4096); !ok || total != 409600 || free != 122880 {
		t.Errorf("sizesFromBlocks = %d/%d/%v, want 409600/122880/true", total, free, ok)
	}
}

// fsSizes voices block math, not block counts: totals are raw
// blocks times the fragment unit. If this fails, the card sizes
// block counts as bytes.
func TestFsSizesMultipliesByUnit(t *testing.T) {
	dir := t.TempDir()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	unit := fsBlockUnit(&fs)
	total, free, ok := fsSizes(dir)
	if !ok {
		t.Fatal("fsSizes refused a plain temp dir")
	}
	if total != uint64(fs.Blocks)*unit || free != uint64(fs.Bavail)*unit {
		t.Errorf("fsSizes = %d/%d, want blocks×unit %d/%d",
			total, free, uint64(fs.Blocks)*unit, uint64(fs.Bavail)*unit)
	}
}

// clockSourceFor mirrors the CLI composition root: same method,
// same transport, no second wiring. If this fails, the console
// checks time through a different transport than the mints.
func TestClockSourceForMirrorsMethods(t *testing.T) {
	if _, ok := clockSourceFor(clock.MethodNTP).(clock.NTP); !ok {
		t.Errorf("ntp source = %T, want clock.NTP", clockSourceFor(clock.MethodNTP))
	}
	if _, ok := clockSourceFor(clock.MethodHTTPS).(clock.HTTPS); !ok {
		t.Errorf("https source = %T, want clock.HTTPS", clockSourceFor(clock.MethodHTTPS))
	}
	if _, ok := clockSourceFor(clock.MethodLocal).(clock.Local); !ok {
		t.Errorf("local source = %T, want clock.Local", clockSourceFor(clock.MethodLocal))
	}
	if _, ok := clockSourceFor(clock.Method("bogus")).(clock.Local); !ok {
		t.Errorf("unknown method source = %T, want clock.Local fallback", clockSourceFor(clock.Method("bogus")))
	}
}

func httpsConfig(t *testing.T, server string) {
	t.Helper()
	t.Setenv(config.EnvTimeMethod, "https")
	t.Setenv(config.EnvTimeServer, server)
	t.Cleanup(config.SetCurrent(config.NewBuilder().FromEnv().Build()))
}

// An https time source the console can read voices its skew: the
// card shows the same transport the mints check through. If this
// fails, the clock card cannot read what the proofs rely on.
func TestClockSnapshotVoicesHTTPSSkew(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	httpsConfig(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	method, server, skew, _ := clockSnapshot(ctx)
	if method != "https" || server != srv.URL {
		t.Errorf("snapshot = %q/%q, want https/%s", method, server, srv.URL)
	}
	if !strings.HasPrefix(skew, "+") && !strings.HasPrefix(skew, "-") {
		t.Errorf("skew = %q, want a signed offset", skew)
	}
}

// An unreachable time source reads unreachable, never slow: the
// caller's probe timeout bounds it. If this fails, a dead time
// server hangs the dashboard.
func TestClockSnapshotUnreachableSource(t *testing.T) {
	httpsConfig(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, skew, note := clockSnapshot(ctx)
	if skew != "unreachable" {
		t.Errorf("skew = %q, want unreachable", skew)
	}
	// The bound voices its exact tuning: any arithmetic on Tolerance
	// renames this line. If this fails, the card's 30s moved without
	// the docs.
	if note != "tolerance 30s" {
		t.Errorf("note = %q, want the 30s tolerance named", note)
	}
}

// Skew voices its sign: ahead +, behind -. If this fails, the card
// flips behind/ahead.
func TestFormatOffset(t *testing.T) {
	if got := formatOffset(5 * time.Second); got != "+5s" {
		t.Errorf("formatOffset(+5s) = %q, want +5s", got)
	}
	if got := formatOffset(-5 * time.Second); got != "-5s" {
		t.Errorf("formatOffset(-5s) = %q, want -5s", got)
	}
	if got := formatOffset(0); got != "+0s" {
		t.Errorf("formatOffset(0) = %q, want +0s", got)
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
