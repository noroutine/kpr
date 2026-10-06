package fencing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/event"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// A locked store refuses manifest PUTs fast and loud, naming the
// remedy — and the backend never sees the request. If this
// fails, locked registries stopped refusing or the sweeper lost
// a delete to the proxy.
func TestGateDeniesManifestPutWhenLocked(t *testing.T) {
	backed := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backed = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	g := lockedGate(t.TempDir(), nil)
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	resp, err := putManifest(front.URL)
	if err != nil {
		t.Fatalf("PUT = %v, want refusal", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusLocked {
		t.Errorf("locked PUT status = %d, want 423", resp.StatusCode)
	}
	if !strings.Contains(string(body), "store unlock") {
		t.Errorf("locked PUT body = %q, want the remedy named", body)
	}
	if backed {
		t.Error("backend saw a locked manifest PUT, want nothing")
	}
}

// Manifest DELETEs refuse the same way: destroying references is
// a write. If this fails, locked deletes leak through.
func TestGateDeniesManifestDeleteWhenLocked(t *testing.T) {
	backed := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backed = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()

	g := lockedGate(t.TempDir(), nil)
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodDelete, front.URL+"/v2/x/manifests/sha256:abc", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE = %v, want refusal", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusLocked {
		t.Errorf("locked DELETE status = %d, want 423", resp.StatusCode)
	}
	if backed {
		t.Error("backend saw a locked manifest DELETE, want nothing")
	}
}

// Blob uploads pass while locked: bytes alone create no
// references. If this fails, the lock started guarding bytes
// instead of semantic mutation.
func TestGatePassesBlobUploadWhenLocked(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/v2/x/blobs/uploads/uuid")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()

	g := lockedGate(t.TempDir(), nil)
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	resp, err := http.Post(front.URL+"/v2/x/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatalf("upload initiate = %v, want 202", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("locked upload status = %d, want 202", resp.StatusCode)
	}
}

// Reads pass while locked: the lock guards mutation. If this
// fails, reads started needing intent.
func TestGatePassesReadsWhenLocked(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"repositories":[]}`))
	}))
	defer backend.Close()

	g := lockedGate(t.TempDir(), nil)
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	resp, err := http.Get(front.URL + "/v2/_catalog")
	if err != nil {
		t.Fatalf("GET = %v, want response", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("locked GET status = %d, want 200", resp.StatusCode)
	}
}

// An unlocked store forwards manifest PUTs: intent opens the
// edge the way it opens the use cases. If this fails, unlock
// stopped releasing the fence.
func TestGateForwardsManifestPutWhenUnlocked(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	st := store.NewMemStore()
	if err := st.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	g := &Gate{Store: st, Dir: t.TempDir()}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	resp, err := putManifest(front.URL)
	if err != nil {
		t.Fatalf("PUT = %v, want 201", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("unlocked PUT status = %d, want 201", resp.StatusCode)
	}
}

// An unreadable marker refuses: unknown is not intent, and a
// fence that fails open on outage is decoration. If this fails,
// store blips started opening the edge.
func TestGateRefusesWhenMarkerUnreadable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	g := &Gate{Store: errLocker{err: errors.New("boom")}, Dir: t.TempDir()}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	resp, err := putManifest(front.URL)
	if err != nil {
		t.Fatalf("PUT = %v, want refusal", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("blind PUT status = %d, want 503", resp.StatusCode)
	}
}

// A live HOLD delays manifest PUTs until expiry, then forwards:
// gc finalize holds, never drops. If this fails, holds stopped
// holding or started dropping.
func TestGateDelaysManifestPutDuringHold(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	dir := t.TempDir()
	st := store.NewMemStore()
	if err := st.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	hf := store.HoldFile{Dir: dir}
	release, err := hf.Hold(context.Background(), time.Now().Add(150*time.Millisecond))
	if err != nil {
		t.Fatalf("engage hold: %v", err)
	}
	defer release()

	g := &Gate{Store: st, Dir: dir}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	start := time.Now()
	resp, err := putManifest(front.URL)
	if err != nil {
		t.Fatalf("held PUT = %v, want 201 after expiry", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("held PUT forwarded after %v, want it held to expiry", elapsed)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("held PUT status = %d, want 201 after expiry", resp.StatusCode)
	}
}

// A HOLD that expires onto a locked store refuses: held requests
// face the current marker on release, never a blind forward. If
// this fails, lock-during-gc leaks pushes past the fence.
func TestGateHoldThenDenyWhenLocked(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	dir := t.TempDir()
	hf := store.HoldFile{Dir: dir}
	release, err := hf.Hold(context.Background(), time.Now().Add(120*time.Millisecond))
	if err != nil {
		t.Fatalf("engage hold: %v", err)
	}
	defer release()

	g := lockedGate(dir, nil)
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	start := time.Now()
	resp, err := putManifest(front.URL)
	if err != nil {
		t.Fatalf("held PUT = %v, want 423 after expiry", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Errorf("held PUT answered after %v, want it held to expiry", elapsed)
	}
	if resp.StatusCode != http.StatusLocked {
		t.Errorf("held-then-locked PUT status = %d, want 423", resp.StatusCode)
	}
}

// An expired HOLD forwards immediately: stale leases never wedge
// pushes. If this fails, crashed collects started wedging the
// edge.
func TestGateIgnoresExpiredHold(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	dir := t.TempDir()
	st := store.NewMemStore()
	if err := st.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	hf := store.HoldFile{Dir: dir}
	release, err := hf.Hold(context.Background(), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("engage hold: %v", err)
	}
	defer release()

	g := &Gate{Store: st, Dir: dir}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	start := time.Now()
	resp, err := putManifest(front.URL)
	if err != nil {
		t.Fatalf("PUT = %v, want 201", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("stale-hold PUT took %v, want immediate", elapsed)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("stale-hold PUT status = %d, want 201", resp.StatusCode)
	}
}

// A released HOLD wakes waiting PUTs promptly: sleepers re-read
// the lease instead of serving the full nominal term. If this
// fails, short collects started stalling pushes to the lease
// bound.
func TestGateWakesWhenHoldReleased(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	dir := t.TempDir()
	st := store.NewMemStore()
	if err := st.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	hf := store.HoldFile{Dir: dir}
	release, err := hf.Hold(context.Background(), time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("engage hold: %v", err)
	}
	g := &Gate{Store: st, Dir: dir}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	done := make(chan int, 1)
	go func() {
		resp, err := putManifest(front.URL)
		if err != nil {
			done <- -1
			return
		}
		defer func() { _ = resp.Body.Close() }()
		done <- resp.StatusCode
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	release()
	select {
	case code := <-done:
		if code != http.StatusCreated {
			t.Errorf("released PUT status = %d, want 201", code)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("released PUT took %v after release, want prompt wake", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("released PUT never returned, waiter missed the release")
	}
}

// A present-but-expired lease says so loudly once: pushes flow
// (fail open) but never silently past a fence gc outran. If this
// fails, overrun collects started flowing quiet.
func TestGateLoudOnceOnExpiredHold(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	dir := t.TempDir()
	st := store.NewMemStore()
	if err := st.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	hf := store.HoldFile{Dir: dir}
	release, err := hf.Hold(context.Background(), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("engage hold: %v", err)
	}
	defer release()

	var stages []string
	g := &Gate{Store: st, Dir: dir, Report: func(e event.Event) { stages = append(stages, e.Stage) }}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	for i := 0; i < 2; i++ {
		resp, err := putManifest(front.URL)
		if err != nil {
			t.Fatalf("PUT = %v, want 201", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expired-hold PUT status = %d, want 201", resp.StatusCode)
		}
	}
	count := 0
	for _, s := range stages {
		if s == StageHoldExpired {
			count++
		}
	}
	if count != 1 {
		t.Errorf("hold_expired emitted %d times over 2 requests, want exactly once", count)
	}
}

// A lease expiring mid-wait says hold_expired: the loud hole,
// not a quiet forward. If this fails, overrun collects started
// flowing silent past the waiter.
func TestGateLoudOnMidWaitExpiry(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	dir := t.TempDir()
	st := store.NewMemStore()
	if err := st.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	hf := store.HoldFile{Dir: dir}
	release, err := hf.Hold(context.Background(), time.Now().Add(150*time.Millisecond))
	if err != nil {
		t.Fatalf("engage hold: %v", err)
	}
	defer release()

	var stages []string
	g := &Gate{Store: st, Dir: dir, Report: func(e event.Event) { stages = append(stages, e.Stage) }}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	resp, err := putManifest(front.URL)
	if err != nil {
		t.Fatalf("PUT = %v, want 201 after expiry", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expired-mid-wait PUT status = %d, want 201", resp.StatusCode)
	}
	found := false
	for _, s := range stages {
		if s == StageHoldExpired {
			found = true
		}
	}
	if !found {
		t.Errorf("stages = %v, want hold_expired among them", stages)
	}
}

// A corrupt hold file forwards: leases fail open, evidence does
// not. If this fails, garbage started fencing.
func TestGateIgnoresCorruptHold(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, store.HoldFileName), []byte("{nope"), 0o600); err != nil {
		t.Fatalf("stage corrupt hold: %v", err)
	}
	st := store.NewMemStore()
	if err := st.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	g := &Gate{Store: st, Dir: dir}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	resp, err := putManifest(front.URL)
	if err != nil {
		t.Fatalf("PUT = %v, want 201", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("corrupt-hold PUT status = %d, want 201", resp.StatusCode)
	}
}

// Deny flips emit one lifecycle event and one ring outcome each
// way — edge-triggered, never per request. If this fails, fence
// flips went silent or started spamming.
func TestGateEmitsOnDenyFlips(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	var stages []string
	st := store.NewMemStore()
	g := &Gate{Store: st, Dir: t.TempDir(), Report: func(e event.Event) { stages = append(stages, e.Stage) }}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	put := func() {
		resp, err := putManifest(front.URL)
		if err != nil {
			t.Fatalf("PUT = %v, want response", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	put() // locked: engage
	put() // locked: no repeat
	if err := st.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	put() // unlocked: release

	want := []string{StageDenyEngage, StageDenyRelease}
	if len(stages) != len(want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Fatalf("stages = %v, want %v", stages, want)
		}
	}
	activity, err := st.Activity(context.Background())
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	if len(activity) != 2 {
		t.Fatalf("ring holds %d outcomes, want 2 flip outcomes", len(activity))
	}
}

// Snapshot is the console's level reading of the edge-triggered
// fence: deny/held reflect the last flip, whatever drove it. If
// this fails, the console reports a posture the fence doesn't hold.
func TestGateSnapshotTracksFlips(t *testing.T) {
	g := &Gate{Store: store.NewMemStore()}
	if deny, held := g.Snapshot(); deny || held {
		t.Fatalf("fresh Snapshot = (%v,%v), want (false,false)", deny, held)
	}
	g.flipDeny(true, "locked")
	if deny, held := g.Snapshot(); !deny || held {
		t.Fatalf("denied Snapshot = (%v,%v), want (true,false)", deny, held)
	}
	g.flipHeld(true, "hold")
	if deny, held := g.Snapshot(); !deny || !held {
		t.Fatalf("held Snapshot = (%v,%v), want (true,true)", deny, held)
	}
	g.flipDeny(false, "unlocked")
	if deny, held := g.Snapshot(); deny || !held {
		t.Fatalf("released Snapshot = (%v,%v), want (false,true)", deny, held)
	}
}

type errLocker struct {
	err error
}

func (s errLocker) IsUnlocked(context.Context) (bool, error)          { return false, s.err }
func (s errLocker) PushActivity(context.Context, store.Outcome) error { return nil }

// putManifest speaks the real method: manifest pushes are PUT,
// and the fence only gates PUT/DELETE. A POST here would pass
// by design and prove nothing.
func putManifest(frontURL string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPut, frontURL+"/v2/x/manifests/latest", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	return http.DefaultClient.Do(req)
}

// No HOLD source means no HOLD waits: an empty dir skips the
// lease logic entirely and goes straight to the lock evaluation.
// If this fails, sourceless gates consult phantom leases.
func TestGateWithoutHoldSourceSkipsLease(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	st := store.NewMemStore()
	if err := st.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	g := &Gate{Store: st}
	front := httptest.NewServer(gateHandler(t, backend.URL, g))
	defer front.Close()

	resp, err := putManifest(front.URL)
	if err != nil {
		t.Fatalf("PUT = %v, want 201", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("sourceless PUT status = %d, want 201", resp.StatusCode)
	}
}

// A gone client stops waiting: the sleeper returns unfenced-false
// instead of serving the term. If this fails, disconnects pile up
// behind dead leases.
func TestWaitReleaseStopsOnDisconnect(t *testing.T) {
	g := &Gate{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, "/v2/x/manifests/latest", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if expired := g.waitRelease(r, time.Now().Add(time.Hour)); expired {
		t.Error("waitRelease on a gone client reported expired, want false (stop, don't tell)")
	}
}

// The expiry re-read consults the file under a divergent clock:
// the waiter's real clock says past, the gate clock says the lease
// is still live, and the file breaks the tie toward honoring it.
// If this fails, clock divergence between waiter and gate reports
// phantom overruns.
func TestWaitReleaseRereadsAtExpiry(t *testing.T) {
	dir := t.TempDir()
	h := store.HoldFile{Dir: dir}
	release, err := h.Hold(context.Background(), time.Now().Add(50*time.Millisecond))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer release()
	g := &Gate{Dir: dir, Now: func() time.Time { return time.Now().Add(-time.Hour) }}
	r, err := http.NewRequest(http.MethodPut, "/v2/x/manifests/latest", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if expired := g.waitRelease(r, time.Now().Add(50*time.Millisecond)); expired {
		t.Error("waitRelease with a live lease reported expired, want the lease honored")
	}
}

// The default clock is the wall: an unpinned gate reads time.Now.
// If this fails, gates without pins read zero time and every lease
// looks expired.
func TestGateNowDefaultsToWall(t *testing.T) {
	if got := (&Gate{}).now(); time.Since(got) > time.Minute {
		t.Errorf("unpinned now = %v, want the wall clock", got)
	}
}

// Flip events do not need a store: a storeless gate still emits
// the flip for the report path, skipping only the ring write. If
// this fails, storeless gates panic on the ring.
func TestEmitWithoutStoreSkipsRing(t *testing.T) {
	var flips []event.Event
	g := &Gate{Report: func(e event.Event) { flips = append(flips, e) }}
	g.flipHeld(true, "held for test")
	if len(flips) != 1 || flips[0].Stage != StageHoldEngage {
		t.Errorf("flips = %v, want the hold_engage flip", flips)
	}
}

func lockedGate(dir string, report func(e event.Event)) *Gate {
	st := store.NewMemStore()
	if err := st.SetUnlocked(context.Background(), false); err != nil {
		panic(err)
	}
	g := &Gate{Store: st, Dir: dir}
	if report != nil {
		g.Report = report
	}
	return g
}

func mintRelative(t *testing.T) proof.RelativeURLs {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/config.yml"
	if err := os.WriteFile(path, []byte("http:\n  relativeurls: true\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	p, err := proof.ProveRelativeURLs(path)
	if err != nil {
		t.Fatalf("ProveRelativeURLs = %v, want mint", err)
	}
	return p
}

func gateHandler(t *testing.T, backend string, g *Gate) http.Handler {
	t.Helper()
	p, err := edge.New(backend)
	if err != nil {
		t.Fatalf("New = %v, want proxy", err)
	}
	h, err := p.Handler(mintRelative(t))
	if err != nil {
		t.Fatalf("Handler = %v, want handler", err)
	}
	return g.Wrap(h)
}
