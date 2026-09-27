package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testCtx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = cancel
	return c
}

// A confirmed delete must hit DELETE /v2/<repo>/manifests/<ref> and
// report deleted. If this fails, the sweeper cannot delete anything —
// every pass is a no-op.
func TestDeleteManifestSuccess(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	outcome, err := c.DeleteManifest(testCtx(), "app", "sha256:abc")
	if err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if outcome != OutcomeDeleted {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeDeleted)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v2/app/manifests/sha256:abc" {
		t.Errorf("request = %s %s, want DELETE /v2/app/manifests/sha256:abc", gotMethod, gotPath)
	}
}

// A manifest already gone upstream (NAME_UNKNOWN/MANIFEST_UNKNOWN)
// counts as success: the row resolves instead of retrying forever. If
// this fails, deleted-outside rows haunt every future pass.
func TestDeleteManifestUnknownCountsAsGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"not found"}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	outcome, err := c.DeleteManifest(testCtx(), "app", "gone")
	if err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if outcome != OutcomeGone {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeGone)
	}
}

// A manifest held by an index (zot-style 405 DENIED) is untracked, not
// retried forever — the index owner collects the child. If this fails,
// the sweeper hammers an un-deletable manifest every tick.
func TestDeleteManifestDeniedUntracks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED","message":"held by index"}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	outcome, err := c.DeleteManifest(testCtx(), "app", "child")
	if err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if outcome != OutcomeHeld {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeHeld)
	}
}

// A tag delete refused as UNSUPPORTED (distribution:3 answers 405 to
// every tag delete) is an error the caller surfaces: the row stays due
// and visible instead of resolving something still there. Callers must
// prefer digests; this path is the digest-less fallback failing
// honestly. If this fails, unsupported deletes masquerade as success.
func TestDeleteManifestUnsupportedTagDeleteFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED","message":"unsupported"}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.DeleteManifest(testCtx(), "app", "v1"); err == nil {
		t.Error("UNSUPPORTED tag delete succeeded, want an error")
	}
}

// A 500 stays a retryable error: the row stays due and the next tick
// retries. If this fails, registry blips either resolve rows that are
// still there or spin without surfacing the failure.
func TestDeleteManifestServerErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.DeleteManifest(testCtx(), "app", "v1"); err == nil {
		t.Error("500 delete succeeded, want a retryable error")
	}
}

// An unreachable registry is a clear error, not a hang past the test
// context. If this fails, a down registry wedges the sweep instead of
// failing fast into next-tick retry.
func TestDeleteManifestUnreachable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	if _, err := c.DeleteManifest(testCtx(), "app", "v1"); err == nil {
		t.Error("unreachable delete succeeded, want an error")
	}
}

// A registry blip on the catalog read is an error, never an empty tag
// list: reap skips catalog selectors for that repo on error, but an
// empty list would read as "every tracked row untagged". If this
// fails, a 500 becomes mass untagging downstream.
func TestCatalogServerErrorFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL).Catalog(testCtx(), "app"); err == nil {
		t.Error("catalog on 500 succeeded, want an error")
	}
}

// keep-N needs the live tag list per repo from the plain catalog API.
// If this fails, reap cannot tell the freshest N from the dead weight.
func TestCatalogListsTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/app/tags/list" {
			t.Errorf("path = %s, want /v2/app/tags/list", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"name":"app","tags":["v3","v2","v1"]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	tags, err := c.Catalog(testCtx(), "app")
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(tags) != 3 || tags[0] != "v3" {
		t.Errorf("tags = %v, want [v3 v2 v1]", tags)
	}
}

// A non-200 base probe is unreachable too: the banner must redden on
// a sick registry, not just a dead socket. If this fails, a 500ing
// registry shows green.
func TestReachableRejectsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL).Reachable(testCtx()); err == nil {
		t.Error("Reachable on 500 registry = nil, want an error")
	}
}

// The console banner needs one cheap reachability probe. If this
// fails, the banner reports a down registry as up (or vice versa).
func TestReachableProbesBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL).Reachable(testCtx()); err != nil {
		t.Errorf("Reachable = %v, want nil", err)
	}
	if err := NewClient("http://127.0.0.1:1").Reachable(testCtx()); err == nil {
		t.Error("Reachable on dead port = nil, want an error")
	}
}
