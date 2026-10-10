package edge

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/proof"
)

// The proxy forwards bytes untouched: status, headers, and body
// arrive identical, including Range resumes. If this fails, the
// front door stopped being transparent.
func TestProxyForwardsUntouched(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes 2-4/10")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("cde"))
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo", r.URL.RequestURI())
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write(body)
	}))
	defer backend.Close()

	p, err := New(backend.URL)
	if err != nil {
		t.Fatalf("New = %v, want proxy", err)
	}
	h, err := p.Handler(proveRelative(t))
	if err != nil {
		t.Fatalf("Handler = %v, want handler", err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v2/x/blobs/uploads/?digest=sha256:abc", strings.NewReader("payload"))
	req.Header.Set("Range", "bytes=2-4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxied request = %v, want response", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(got) != "cde" {
		t.Errorf("range resume = %d %q, want 206 cde", resp.StatusCode, got)
	}
	if ce := resp.Header.Get("Content-Range"); ce != "bytes 2-4/10" {
		t.Errorf("Content-Range = %q, want passthrough", ce)
	}

	resp2, err := http.Post(front.URL+"/v2/x/manifests/latest", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("proxied POST = %v, want response", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusTeapot || string(body2) != "{}" {
		t.Errorf("POST = %d %q, want 418 {}", resp2.StatusCode, body2)
	}
	if echo := resp2.Header.Get("X-Echo"); echo != "/v2/x/manifests/latest" {
		t.Errorf("X-Echo = %q, want path preserved", echo)
	}
}

// A relative upstream Location passes untouched: the proven
// case. If this fails, the guard started mangling good
// responses.
func TestProxyKeepsRelativeLocation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/v2/x/blobs/uploads/uuid?_state=abc")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()

	var logged []string
	h := guarded(t, backend.URL, &logged)
	front := httptest.NewServer(h)
	defer front.Close()

	resp, err := http.Post(front.URL+"/v2/x/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatalf("POST = %v, want response", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if loc := resp.Header.Get("Location"); loc != "/v2/x/blobs/uploads/uuid?_state=abc" {
		t.Errorf("Location = %q, want untouched relative", loc)
	}
	if len(logged) != 0 {
		t.Errorf("logged %v, want silence on the proven case", logged)
	}
}

// An absolute upstream Location naming the backend is a fence
// bypass: the guard rewrites it to the path and says so loudly.
// If this fails, clients can walk around the edge.
func TestProxyRewritesAbsoluteBackendLocation(t *testing.T) {
	const upstream = "http://registry:5000/v2/x/blobs/uploads/uuid?_state=abc"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", upstream)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()

	var logged []string
	h := guarded(t, backend.URL, &logged)
	front := httptest.NewServer(h)
	defer front.Close()

	// The client must not follow the bypass: stop redirects.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Post(front.URL+"/v2/x/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatalf("POST = %v, want response", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if loc := resp.Header.Get("Location"); loc != "/v2/x/blobs/uploads/uuid?_state=abc" {
		t.Errorf("Location = %q, want path-only rewrite", loc)
	}
	if len(logged) == 0 {
		t.Error("no loud line on rewrite, want one")
	}
}

// No proof, no edge: the handler refuses a nil RelativeURLs
// instead of opening a bypass. If this fails, the proof gate
// stopped gating.
func TestProxyRefusesWithoutProof(t *testing.T) {
	p, err := New("http://127.0.0.1:9")
	if err != nil {
		t.Fatalf("New = %v, want proxy", err)
	}
	if _, err := p.Handler(nil); err == nil {
		t.Error("Handler(nil) = nil, want refusal")
	}
}

// A bad backend URL refuses at construction: guessing backends
// is worse than not proxying. If this fails, construction
// stopped validating.
func TestNewRefusesBadBackend(t *testing.T) {
	if _, err := New("://bad"); err == nil {
		t.Error("New(bad) = nil, want refusal")
	}
}

// A schemeless backend refuses at the scheme check: it parses,
// but names no target. If this fails, relative strings become
// proxy backends.
func TestNewRefusesSchemeless(t *testing.T) {
	if _, err := New("no-scheme-no-host"); err == nil {
		t.Error("New(schemeless) = nil, want refusal")
	} else if !strings.Contains(err.Error(), "want scheme://host") {
		t.Errorf("refusal = %q, want the shape named", err.Error())
	}
}

// A nil proof refuses the handler in-package too: the compiler
// allows it, the gate does not. If this fails, an unproven edge
// serves.
func TestHandlerRefusesNilProofInPackage(t *testing.T) {
	p, err := New("http://127.0.0.1:9")
	if err != nil {
		t.Fatalf("New = %v, want proxy", err)
	}
	if h, err := p.Handler(nil); err == nil || h != nil {
		t.Errorf("Handler(nil) = (%v, %v), want ErrNoProof", h, err)
	}
}

// An unparseable upstream Location passes through untouched and
// loud: the guard mangles nothing it cannot parse, but says so.
// If this fails, corrupt Locations rewrite silently (or crash the
// guard).
func TestGuardLeavesUnparseableLocation(t *testing.T) {
	var logged []string
	p, err := New("http://127.0.0.1:9")
	if err != nil {
		t.Fatalf("New = %v, want proxy", err)
	}
	p.Logf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	resp := &http.Response{Header: http.Header{"Location": {"http://exa\tmple.com/x"}}}
	resp.Request = &http.Request{URL: &url.URL{Path: "/v2/x/manifests/latest"}}
	if err := p.guardLocation(resp); err != nil {
		t.Fatalf("guardLocation = %v, want nil (leave, don't fail)", err)
	}
	if got := resp.Header.Get("Location"); got != "http://exa\tmple.com/x" {
		t.Errorf("Location = %q, want it untouched", got)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "unparseable upstream Location") {
		t.Errorf("logged = %v, want the loud leave", logged)
	}
}

func guarded(t *testing.T, backend string, logged *[]string) http.Handler {
	t.Helper()
	p, err := New(backend)
	if err != nil {
		t.Fatalf("New = %v, want proxy", err)
	}
	p.Logf = func(format string, args ...any) {
		*logged = append(*logged, format)
	}
	h, err := p.Handler(proveRelative(t))
	if err != nil {
		t.Fatalf("Handler = %v, want handler", err)
	}
	return h
}

func proveRelative(t *testing.T) proof.RelativeURLs {
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
