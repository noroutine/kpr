package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
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
	for _, want := range []string{"Keeper", "dry-run", "unreachable"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q (degraded banner)", want)
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
		"deleted",   // activity outcome
		"reachable", // registry probe
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "/v2/app/tags/list") {
		t.Error("dashboard links registry catalog browsing, want tracked-state only")
	}
}

// POST /api/sweep triggers a pass and answers its summary as JSON —
// the synchronous feedback `sweep` watches. GET is rejected, and a
// console without a sweeper answers 503 instead of panicking. If this
// fails, the CLI trigger has no endpoint to call.
func TestSweepEndpointTriggersPass(t *testing.T) {
	testConfig(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer fake.Close()

	s := &Server{
		Store:   keeperStore(t),
		Sweeper: &sweep.Sweeper{Store: store.NewMemStore(), Registry: registry.NewClient(fake.URL)},
	}
	s.Sweeper.Store = s.Store
	s.Sweeper.DryRun = true

	req := httptest.NewRequest(http.MethodPost, "/api/sweep", nil)
	rr := httptest.NewRecorder()
	s.sweepHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var sum sweep.Summary
	if err := json.NewDecoder(rr.Body).Decode(&sum); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sum.Trigger != "POST" || sum.Planned != 1 {
		t.Errorf("summary = %+v, want POST trigger with 1 planned", sum)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/sweep", nil)
	grr := httptest.NewRecorder()
	s.sweepHandler(grr, get)
	if grr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", grr.Code)
	}

	bare := &Server{}
	breq := httptest.NewRequest(http.MethodPost, "/api/sweep", nil)
	brr := httptest.NewRecorder()
	bare.sweepHandler(brr, breq)
	if brr.Code != http.StatusServiceUnavailable {
		t.Errorf("sweeper-less status = %d, want 503", brr.Code)
	}
}
