package registry

import (
	"context"
	"errors"
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

// Non-200 manifest/blob reads report a typed status: callers tell
// absence (404) from failure without parsing messages. If this
// fails, silence and corruption collapse into one bucket and every
// verdict guesses.
func TestGetNotFoundTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	_, merr := c.GetManifest(testCtx(), "app", "v1")
	var mse *StatusError
	if !errors.As(merr, &mse) || mse.Status != http.StatusNotFound {
		t.Fatalf("GetManifest err = %v, want *StatusError 404", merr)
	}
	_, berr := c.GetBlob(testCtx(), "app", "sha256:abc")
	var bse *StatusError
	if !errors.As(berr, &bse) || bse.Status != http.StatusNotFound {
		t.Fatalf("GetBlob err = %v, want *StatusError 404", berr)
	}
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

// An unrecognized status is an error even with a parseable body: only
// the three known classifications resolve, everything else surfaces.
// If this fails, a novel registry refusal reads as a resolution.
func TestDeleteManifestUnexpectedStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNKNOWN","message":"boom"}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if outcome, err := c.DeleteManifest(testCtx(), "app", "sick"); err == nil {
		t.Errorf("DeleteManifest(403) = (%q, nil), want error", outcome)
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

// The sentinel reader fetches manifests by tag: the request must ask
// for a manifest media type (else the registry 406s) and return the
// exact bytes. If this fails, generation read-back compares garbage.
func TestGetManifestSendsAccept(t *testing.T) {
	var gotAccept, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept, gotPath = r.Header.Get("Accept"), r.URL.Path
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	}))
	defer srv.Close()

	body, err := NewClient(srv.URL).GetManifest(testCtx(), "kpr-sentinel", "live")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if string(body) != `{"schemaVersion":2}` {
		t.Errorf("body = %q, want exact bytes", body)
	}
	if gotPath != "/v2/kpr-sentinel/manifests/live" {
		t.Errorf("path = %s, want /v2/kpr-sentinel/manifests/live", gotPath)
	}
	if gotAccept == "" {
		t.Error("no Accept header sent, want a manifest media type")
	}
}

// A missing sentinel tag is absence of proof, never an empty
// manifest: callers must refuse, not compare zero values. If this
// fails, a stranger's store reads as "generation 0".
func TestGetManifestUnknownFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"not found"}]}`))
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL).GetManifest(testCtx(), "kpr-sentinel", "live"); err == nil {
		t.Error("GetManifest on 404 succeeded, want an error")
	}
}

// The sentinel payload comes back as a blob by digest under the same
// repo (the layer link we craft). If this fails, the reader cannot
// reach the structured info behind the manifest.
func TestGetBlobFetchesByDigest(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"v":1,"gen":3}`))
	}))
	defer srv.Close()

	body, err := NewClient(srv.URL).GetBlob(testCtx(), "kpr-sentinel", "sha256:abc")
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if string(body) != `{"v":1,"gen":3}` {
		t.Errorf("body = %q, want exact bytes", body)
	}
	if gotPath != "/v2/kpr-sentinel/blobs/sha256:abc" {
		t.Errorf("path = %s, want /v2/kpr-sentinel/blobs/sha256:abc", gotPath)
	}
}

// A blob the registry never saw is an error, never empty bytes: an
// empty payload would unmarshal to generation 0 and pass a careless
// comparison. If this fails, missing evidence reads as old evidence.
func TestGetBlobUnknownFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"BLOB_UNKNOWN","message":"not found"}]}`))
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL).GetBlob(testCtx(), "kpr-sentinel", "sha256:abc"); err == nil {
		t.Error("GetBlob on 404 succeeded, want an error")
	}
}
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

// errBody fails mid-read: a registry that headers 200 then dies.
// The read refuses with the body cause, never with truncated bytes
// presented as a manifest. If this fails, a cut connection parses
// as content.
type errBody struct{ err error }

func (b errBody) Read([]byte) (int, error) { return 0, b.err }
func (b errBody) Close() error             { return nil }

type errRoundTripper struct{ err error }

func (f errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: http.StatusOK,
		Body: errBody{errors.New("connection reset")}, Header: http.Header{}}, nil
}

// A dead peer fails every verb with the transport cause: catalog,
// delete, manifest, blob — no verb invents an answer. If this
// fails, one verb guesses where the others refuse.
func TestDeadPeerFailsEveryVerb(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	if _, err := c.Catalog(testCtx(), "app"); err == nil {
		t.Error("catalog on dead peer succeeded, want refusal")
	}
	if _, err := c.DeleteManifest(testCtx(), "app", "v1"); err == nil {
		t.Error("delete on dead peer succeeded, want refusal")
	}
	if _, err := c.GetManifest(testCtx(), "app", "v1"); err == nil {
		t.Error("manifest on dead peer succeeded, want refusal")
	}
	if _, err := c.GetBlob(testCtx(), "app", "sha256:abc"); err == nil {
		t.Error("blob on dead peer succeeded, want refusal")
	}
}

// An unbuildable request refuses before dialing: a misconfigured
// base URL is a wiring error, never a dial. If this fails, a bad
// base URL dials garbage.
func TestBadBaseURLRefusesBeforeDial(t *testing.T) {
	c := NewClient("http://exa\tmple.com")
	if _, err := c.Catalog(testCtx(), "app"); err == nil {
		t.Error("catalog on bad URL succeeded, want refusal")
	}
	if _, err := c.DeleteManifest(testCtx(), "app", "v1"); err == nil {
		t.Error("delete on bad URL succeeded, want refusal")
	}
}

// A 200 with a dying body refuses at the read: truncation is not
// content. If this fails, cut connections parse as manifests.
func TestDyingBodyRefusesAtRead(t *testing.T) {
	c := NewClient("http://registry:5000")
	c.client = &http.Client{Transport: errRoundTripper{}}
	if _, _, err := c.getAccept(testCtx(), "/v2/app/tags/list", ""); err == nil {
		t.Error("read of dying body succeeded, want refusal")
	}
}

// Catalog answers that are not 200, or not JSON, refuse naming the
// repo: absence of tags is never an empty list. If this fails, a
// sick registry reads as tagless.
func TestCatalogBadAnswersRefuse(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	if _, err := NewClient(broken.URL).Catalog(testCtx(), "app"); err == nil {
		t.Error("catalog on 500 succeeded, want refusal")
	}
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer garbage.Close()
	if _, err := NewClient(garbage.URL).Catalog(testCtx(), "app"); err == nil {
		t.Error("catalog on garbage succeeded, want refusal")
	}
}

// A 404 with a garbage body is retryable, not gone: no code means
// no classification. If this fails, an unparseable 404 untracks a
// row the registry may still serve.
func TestDeleteGarbage404IsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL).DeleteManifest(testCtx(), "app", "v1"); err == nil {
		t.Error("delete on garbage 404 succeeded, want the retryable error")
	}
}

// The status error text keeps its shape (op + status) and exposes
// the code for classifiers: logs and asserted outputs do not move.
// If this fails, absence stops classifying.
func TestStatusErrorShape(t *testing.T) {
	err := &StatusError{Op: "manifest app:v1", Status: 404}
	if got := err.Error(); got != "manifest app:v1: registry status 404" {
		t.Errorf("Error() = %q, want the op + status shape", got)
	}
	var _ interface{ StatusCode() int } = err
	if err.StatusCode() != 404 {
		t.Errorf("StatusCode() = %d, want 404", err.StatusCode())
	}
}
