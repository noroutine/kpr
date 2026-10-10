//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/event"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/storeops"
)

// startEdgeRegistry is startMountedRegistry with relativeurls on:
// the edge proves over a config file, but the registry itself must
// serve relative Locations or every redirect names the backend.
func startEdgeRegistry(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	regC, err := testcontainers.Run(ctx, fixtureRegistryImage,
		testcontainers.WithExposedPorts("5000/tcp"),
		testcontainers.WithEnv(map[string]string{
			"REGISTRY_STORAGE_DELETE_ENABLED": "true",
			"REGISTRY_HTTP_RELATIVEURLS":      "true",
		}),
		testcontainers.WithMounts(testcontainers.BindMount(root, "/var/lib/registry")),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/v2/").WithPort("5000/tcp")),
	)
	if err != nil {
		t.Fatalf("start edge registry: %v", err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, regC) })
	host, err := regC.Host(ctx)
	if err != nil {
		t.Fatalf("registry host: %v", err)
	}
	port, err := regC.MappedPort(ctx, "5000/tcp")
	if err != nil {
		t.Fatalf("registry port: %v", err)
	}
	return "http://" + host + ":" + port.Port(), root
}

// stageEdgeProof writes the proof source serve reads: relativeurls
// asserted, host empty (a set host silently overrides it).
func stageEdgeProof(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "edge-proof.yml")
	if err := os.WriteFile(path, []byte("http:\n  relativeurls: true\n"), 0o644); err != nil {
		t.Fatalf("stage edge proof: %v", err)
	}
	return path
}

// stageOnlineRegistryConfig stages the config gc reads on the
// online path: the store root plus the relativeurls assertion the
// gateway prover checks. The edge registry genuinely serves
// relative Locations (see startEdgeRegistry), so the claim is
// honest; lock_test's registry does not, and keeps the bare
// stageRegistryConfig.
func stageOnlineRegistryConfig(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := "storage:\n  filesystem:\n    rootdirectory: " + root + "\nhttp:\n  relativeurls: true\n"
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("stage online registry config: %v", err)
	}
	return path
}

// serveEdge proves RelativeURLs and serves the HOLD/DENY-gated
// proxy the way serve wires it: file store underneath (lock marker
// and lease dir are the same dir), httptest in front. It returns
// the edge base URL.
func serveEdge(t *testing.T, backend, proofPath string, st *store.FileStore, dir string) string {
	t.Helper()
	proven, err := proof.ProveRelativeURLs(proofPath)
	if err != nil {
		t.Fatalf("prove edge: %v", err)
	}
	p, err := edge.New(backend)
	if err != nil {
		t.Fatalf("edge backend: %v", err)
	}
	h, err := p.Handler(proven)
	if err != nil {
		t.Fatalf("edge handler: %v", err)
	}
	gate := &fence.Gate{Store: st, Lease: store.FileLease{Dir: dir}, Report: func(event.Event) {}}
	srv := httptest.NewServer(gate.Wrap(h))
	t.Cleanup(srv.Close)
	return srv.URL
}

// edgeRef points a ggcr tag at the edge instead of the registry.
func edgeRef(t *testing.T, edgeURL, repo, tag string) string {
	t.Helper()
	return strings.TrimPrefix(edgeURL, "http://") + "/" + repo + ":" + tag
}

// putManifest PUTs raw manifest bytes, returning status + body.
func putManifest(t *testing.T, url string, raw []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// getBytes GETs a path, returning status + body on 200.
func getBytes(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // test dials its own httptest/container URLs
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// pushViaEdge writes a real single-layer image through the proxy
// and returns its manifest bytes (read back direct) for re-PUTs.
func pushViaEdge(t *testing.T, edgeURL, repo, tag string) []byte {
	t.Helper()
	img, err := buildImage(repo, tag)
	if err != nil {
		t.Fatalf("ggcr build: %v", err)
	}
	ref, err := name.NewTag(edgeRef(t, edgeURL, repo, tag), name.Insecure)
	if err != nil {
		t.Fatalf("edge ref: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push via edge: %v", err)
	}
	manifest, err := img.RawManifest()
	if err != nil {
		t.Fatalf("raw manifest: %v", err)
	}
	return manifest
}

// A locked store fences mutating routes at the proxy while reads
// and blob uploads pass: manifest PUT/DELETE refuse 423 naming the
// remedy (even for tags that don't exist — the fence decides
// before the backend), and unlocking forwards everything again.
// If this fails, pushes bypass the lock or reads pay for it.
func TestEdgeDeniesMutationsWhenLocked(t *testing.T) {
	backend, _ := startEdgeRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	st := store.NewFileStore(dir)
	edgeURL := serveEdge(t, backend, stageEdgeProof(t), st, dir)

	if err := st.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	manifest := pushViaEdge(t, edgeURL, "test/edge", "base")

	directCode, directBody := getBytes(t, backend+"/v2/test/edge/manifests/base")
	if directCode != http.StatusOK {
		t.Fatalf("direct GET base = %d, want 200", directCode)
	}

	if err := st.SetUnlocked(ctx, false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if code, body := putManifest(t, edgeURL+"/v2/test/edge/manifests/probe", manifest); code != http.StatusLocked ||
		!strings.Contains(body, "store unlock") {
		t.Errorf("locked PUT = %d %q, want 423 naming the remedy", code, body)
	}
	req, _ := http.NewRequest(http.MethodDelete, edgeURL+"/v2/test/edge/manifests/nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("locked DELETE: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusLocked {
		t.Errorf("locked DELETE of a missing tag = %d, want 423 (fence-first, never 404)", resp.StatusCode)
	}
	if code, body := getBytes(t, edgeURL+"/v2/test/edge/manifests/base"); code != http.StatusOK ||
		!bytes.Equal(body, directBody) {
		t.Errorf("locked GET = %d (%d bytes), want 200 byte-identical to direct (%d)", code, len(body), len(directBody))
	}

	blob := []byte("e2e-edge-blob")
	upload, err := http.Post(edgeURL+"/v2/test/edge/blobs/uploads/", "application/octet-stream", bytes.NewReader(blob)) //nolint:gosec // own URLs
	if err != nil {
		t.Fatalf("locked blob POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, upload.Body)
	loc := upload.Header.Get("Location")
	_ = upload.Body.Close()
	if upload.StatusCode != http.StatusAccepted || loc == "" {
		t.Fatalf("locked blob POST = %d, want 202 with a session", upload.StatusCode)
	}
	sum := sha256.Sum256(blob)
	digest := fmt.Sprintf("sha256:%x", sum)
	session := loc
	if !strings.HasPrefix(session, "http") {
		session = edgeURL + session
	}
	closeReq, _ := http.NewRequest(http.MethodPut, session+"&digest="+digest, bytes.NewReader(blob))
	closeResp, err := http.DefaultClient.Do(closeReq)
	if err != nil {
		t.Fatalf("locked blob PUT: %v", err)
	}
	_, _ = io.Copy(io.Discard, closeResp.Body)
	_ = closeResp.Body.Close()
	if closeResp.StatusCode != http.StatusCreated {
		t.Fatalf("locked blob close = %d, want 201 (uploads pass fenced)", closeResp.StatusCode)
	}
	if code, body := getBytes(t, edgeURL+"/v2/test/edge/blobs/"+digest); code != http.StatusOK ||
		!bytes.Equal(body, blob) {
		t.Errorf("locked blob GET = %d, want 200 with the exact bytes", code)
	}

	if err := st.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if code, _ := putManifest(t, edgeURL+"/v2/test/edge/manifests/probe", manifest); code != http.StatusCreated {
		t.Errorf("unlocked PUT = %d, want 201 (forwarded)", code)
	}
	delReq, _ := http.NewRequest(http.MethodDelete, edgeURL+"/v2/test/edge/manifests/probe", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("unlocked DELETE: %v", err)
	}
	_, _ = io.Copy(io.Discard, delResp.Body)
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusAccepted {
		t.Errorf("unlocked DELETE = %d, want 202 (forwarded)", delResp.StatusCode)
	}
	if code, _ := getBytes(t, edgeURL+"/v2/test/edge/manifests/probe"); code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", code)
	}
}

// recordFence delegates to the real lease file while recording
// whether gc asked for a HOLD at all. Deny/Allow ride the
// embedded Control (honest transitions into the ring); only Hold
// is observed.
type recordFence struct {
	fence.Control
	held atomic.Bool
}

func (r *recordFence) Hold(ctx context.Context, until time.Time) (func(), error) {
	r.held.Store(true)
	return r.Control.Hold(ctx, until)
}

// An armed collect holds manifest PUTs at the proxy until it
// releases; a preview never engages the fence, so the same PUT
// lands immediately. If this fails, gc either collects unfenced
// or previews hold pushes hostage.
func TestEdgeHoldDelaysManifestPutDuringArmedGC(t *testing.T) {
	backend, root := startEdgeRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	st := store.NewFileStore(dir)
	edgeURL := serveEdge(t, backend, stageEdgeProof(t), st, dir)
	if err := st.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	manifest := pushViaEdge(t, edgeURL, "test/edge", "hold")

	api := registry.NewClient(backend)
	cfg := stageOnlineRegistryConfig(t, root)
	// The stub sleeps through the collect: the HOLD window the
	// armed PUT must wait out. The collect seam is
	// package-private, so the sleep rides the binary.
	bin, _ := stageCollectorStub(t, "sleep 5\n")
	// The world proves everything here, so nothing is accepted:
	// no cache in the edge registry's env, a proven edge
	// listening, a configured lease dir. The preflight must clear
	// on evidence alone.
	edgeAddr := strings.TrimPrefix(edgeURL, "http://")
	restore := stageRunConfig(t, backend, cfg, bin, stageTimeServer(t), edgeAddr)
	defer restore()
	var unlockOut strings.Builder
	if err := storeops.Unlock(ctx, &unlockOut, storeops.UnlockDeps{
		Rec: st, Ids: st, Rows: st,
		API: api, Clock: clock.HTTPS{}, Store: st,
	}); err != nil {
		t.Fatalf("pair sentinel: %v", err)
	}
	run := func(armed proof.ArmedRun, f fence.Controller) error {
		var out strings.Builder
		return gc.Run(ctx, &out, gc.Deps{
			Lock: st, Rec: st, Ids: st, Rows: st,
			API: api, Clock: clock.HTTPS{}, Report: func(event.Event) {},
			Store: st, Fence: f,
		}, gc.Options{Armed: armed}, gc.Accepts{})
	}

	previewFence := &recordFence{Control: fence.Control{Store: st, Lease: store.FileLease{Dir: dir}}}
	if err := run(nil, previewFence); err != nil {
		t.Fatalf("preview gc: %v", err)
	}
	if previewFence.held.Load() {
		t.Error("preview gc engaged HOLD, want unfenced")
	}
	start := time.Now()
	if code, _ := putManifest(t, edgeURL+"/v2/test/edge/manifests/holdpreview", manifest); code != http.StatusCreated {
		t.Fatalf("PUT after preview = %d, want 201", code)
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Errorf("PUT after preview took %v, want immediate (no lease)", elapsed)
	}

	armedFence := &recordFence{Control: fence.Control{Store: st, Lease: store.FileLease{Dir: dir}}}
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(proof.Arm(true, false), armedFence)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for !armedFence.held.Load() && time.Now().Before(deadline) {
		select {
		case err := <-runErr:
			t.Fatalf("armed gc returned before engaging HOLD: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !armedFence.held.Load() {
		t.Fatal("armed gc never engaged HOLD")
	}
	start = time.Now()
	if code, _ := putManifest(t, edgeURL+"/v2/test/edge/manifests/holdarmed", manifest); code != http.StatusCreated {
		t.Errorf("PUT during armed gc = %d, want 201 after release", code)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Errorf("PUT during armed gc took %v, want held-then-forwarded", elapsed)
	}
	if err := <-runErr; err != nil {
		t.Errorf("armed gc: %v", err)
	}
}
