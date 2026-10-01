package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stageGCStore writes a registry config pointing at root and returns
// the config path: every gc run test collects against staged ground,
// never the host's.
func stageGCStore(t *testing.T, root string) string {
	t.Helper()
	cfg := `storage:
  filesystem:
    rootdirectory: ` + root + "\n"
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("stage gc config: %v", err)
	}
	return path
}

// unlockedStore stages intent-open state: gc tests vary proof and
// collection, never the marker. Fresh-locked is pinned by the
// storetest contract and the dedicated refusal tests.
func unlockedStore(t *testing.T) *store.MemStore {
	t.Helper()
	s := store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	return s
}

// serveRegistry is a file-backed fake registry over root: the probe
// initiate classifies writable/readonly, manifest and blob GETs read
// what the run just wrote to the store, DELETE cancels uploads. A
// root that never receives the generation serves 404s — a stranger's
// store on demand.
func serveRegistry(t *testing.T, root string, writable bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/") {
			if !writable {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Location", "/v2/noroutine/kpr-gc-probe/blobs/uploads/uuid")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		serveSentinelFiles(w, r, root)
	}))
}

// serveSentinelFiles answers the non-probe half: upload cancels plus
// manifest/blob reads straight from the staged root.
func serveSentinelFiles(w http.ResponseWriter, r *http.Request, root string) {
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if tag, ok := strings.CutPrefix(r.URL.Path, "/v2/noroutine/kpr-sentinel/manifests/"); ok {
		raw, err := os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "repositories", "noroutine/kpr-sentinel", "_manifests", "tags", tag, "current", "link"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		serveBlobFile(w, root, strings.TrimSpace(string(raw)))
		return
	}
	if digest, ok := strings.CutPrefix(r.URL.Path, "/v2/noroutine/kpr-sentinel/blobs/"); ok {
		hex := strings.TrimPrefix(digest, "sha256:")
		if _, err := os.Stat(filepath.Join(root, "docker", "registry", "v2", "repositories", "noroutine/kpr-sentinel", "_layers", "sha256", hex, "link")); err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		serveBlobFile(w, root, digest)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func serveBlobFile(w http.ResponseWriter, root, digest string) {
	hex := strings.TrimPrefix(digest, "sha256:")
	raw, err := os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", hex[:2], hex, "data"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = w.Write(raw)
}

// A lock release failure after a good run warns (with the TTL bound)
// instead of failing the run: the collection already happened. If
// this fails, a redis blip at release time rewrites history.
func TestRunGCReleaseFailureWarns(t *testing.T) {
	root := t.TempDir()
	srv := serveRegistry(t, root, false)
	defer srv.Close()

	cfg := stageGCStore(t, root)
	s := &releaseFailStore{MemStore: unlockedStore(t)}

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	var out strings.Builder
	if err := gc.Run(context.Background(), &out, gc.ProbeRegistry, s, gc.RunCollector, registry.NewClient(srv.URL), srv.URL, cfg, registryBinPath, s, s, s, "127.0.0.1:1", gc.Options{}); err != nil {
		t.Fatalf("gc with failing release = %v, want nil (warn only)", err)
	}
	if !strings.Contains(out.String(), "lock release failed") {
		t.Errorf("release failure warned nothing:\n%s", out.String())
	}
}

type releaseFailStore struct {
	*store.MemStore
}

func (releaseFailStore) ReleaseLock(context.Context, string) error { return errRelease }

var errRelease = errors.New("release failed")

// A broken pipe during the forced-writable warning fails the run
// instead of collecting deaf: the operator never saw the risk they
// accepted. If this fails, gc nods along with nobody listening.
func TestRunGCForceWarnWriteError(t *testing.T) {
	root := t.TempDir()
	srv := serveRegistry(t, root, true)
	defer srv.Close()

	cfg := stageGCStore(t, root)

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	s := unlockedStore(t)
	if err := gc.Run(context.Background(), errWriter{}, gc.ProbeRegistry, s, gc.RunCollector, registry.NewClient(srv.URL), srv.URL, cfg, registryBinPath, s, s, s, "127.0.0.1:1", gc.Options{Force: true}); err == nil {
		t.Error("forced gc with broken output succeeded, want the write error")
	}
}

// stageBin writes an executable shell stub as the collector binary.
func stageBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "collector.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("stage collector: %v", err)
	}
	return path
}

// A writable registry refuses gc without --force (the operator flips
// readonly first), and proceeds with it after warning — but only when
// the generation just written reads back from the local store. A
// stranger's registry at the right URL refuses even forced. If this
// fails, collection runs against live writes, or against the wrong
// store entirely.
func TestRunGCRefusesWritableWithoutForce(t *testing.T) {
	root := t.TempDir()
	srv := serveRegistry(t, root, true)
	defer srv.Close()

	cfg := stageGCStore(t, root)
	s := unlockedStore(t)

	oldBin := registryBinPath
	registryBinPath = "/bin/sh"
	defer func() { registryBinPath = oldBin }()

	var out strings.Builder
	if err := gc.Run(context.Background(), &out, gc.ProbeRegistry, s, gc.RunCollector, registry.NewClient(srv.URL), srv.URL, cfg, registryBinPath, s, s, s, "127.0.0.1:1", gc.Options{}); err == nil {
		t.Fatal("gc on writable registry succeeded without --force, want refusal")
	} else if !strings.Contains(err.Error(), "readonly") {
		t.Errorf("refusal names no remedy: %v", err)
	}

	registryBinPath = stageBin(t, "exit 0")
	out.Reset()
	if err := gc.Run(context.Background(), &out, gc.ProbeRegistry, s, gc.RunCollector, registry.NewClient(srv.URL), srv.URL, cfg, registryBinPath, s, s, s, "127.0.0.1:1", gc.Options{Force: true}); err != nil {
		t.Fatalf("forced gc = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "Warning") {
		t.Errorf("forced gc warned nothing:\n%s", out.String())
	}
}

// A stranger's registry at the right URL — the generation never
// reads back — refuses even forced. If this fails, gc happily
// collects whatever directory the mount points at.
func TestRunGCDifferentStoreRefuses(t *testing.T) {
	srv := serveRegistry(t, t.TempDir(), true)
	defer srv.Close()

	root := t.TempDir()
	cfg := stageGCStore(t, root)
	s := unlockedStore(t)

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	var out strings.Builder
	if err := gc.Run(context.Background(), &out, gc.ProbeRegistry, s, gc.RunCollector, registry.NewClient(srv.URL), srv.URL, cfg, registryBinPath, s, s, s, "127.0.0.1:1", gc.Options{Force: true}); err == nil {
		t.Fatal("gc on a stranger's store succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "does not share") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A held gc lock refuses the run: gc serializes on kpr:gc:lock (same
// key make gc honors), and a finished run releases it. If this fails,
// two collectors race the same store, or one run wedges the rest.
func TestRunGCLockContention(t *testing.T) {
	root := t.TempDir()
	srv := serveRegistry(t, root, false)
	defer srv.Close()

	ctx := context.Background()
	cfg := stageGCStore(t, root)
	s := unlockedStore(t)

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	if ok, err := s.AcquireLock(ctx, store.GCLockKey, time.Minute); err != nil || !ok {
		t.Fatalf("pre-acquire = (%v, %v), want (true, nil)", ok, err)
	}
	var out strings.Builder
	if err := gc.Run(ctx, &out, gc.ProbeRegistry, s, gc.RunCollector, registry.NewClient(srv.URL), srv.URL, cfg, registryBinPath, s, s, s, "127.0.0.1:1", gc.Options{}); err == nil {
		t.Fatal("gc under held lock succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "another gc") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if err := s.ReleaseLock(ctx, store.GCLockKey); err != nil {
		t.Fatalf("release: %v", err)
	}
	out.Reset()
	if err := gc.Run(ctx, &out, gc.ProbeRegistry, s, gc.RunCollector, registry.NewClient(srv.URL), srv.URL, cfg, registryBinPath, s, s, s, "127.0.0.1:1", gc.Options{}); err != nil {
		t.Fatalf("gc after release = %v, want nil", err)
	}
	if ok, _ := s.AcquireLock(ctx, store.GCLockKey, time.Minute); !ok {
		t.Error("lock still held after successful gc, want released")
	}
	_ = s.ReleaseLock(ctx, store.GCLockKey)
}

// The post-run probe detects a mode flip mid-collect (readonly went
// writable: something may have written under the mark phase). Changed
// mode fails the run — the delete already happened, silence would be
// the lie. A failed post-probe only warns: unknown is not observed
// interference. If this fails, gc blesses runs it watched go sideways.
func TestRunGCPostProbeFlip(t *testing.T) {
	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	flipRoot := t.TempDir()
	flap := serveFlapRegistry(t, flipRoot, http.StatusMethodNotAllowed, http.StatusAccepted)
	defer flap.Close()
	flipCfg := stageGCStore(t, flipRoot)
	var out strings.Builder
	flipStore := unlockedStore(t)
	if err := gc.Run(context.Background(), &out, gc.ProbeRegistry, flipStore, gc.RunCollector, registry.NewClient(flap.URL), flap.URL, flipCfg, registryBinPath, flipStore, flipStore, flipStore, "127.0.0.1:1", gc.Options{}); err == nil {
		t.Fatal("gc across a readonly→writable flip succeeded, want failure")
	} else if !strings.Contains(err.Error(), "changed during collection") {
		t.Errorf("failure names no cause: %v", err)
	}

	deadRoot := t.TempDir()
	down := serveFlapRegistry(t, deadRoot, http.StatusMethodNotAllowed, http.StatusInternalServerError)
	defer down.Close()
	deadCfg := stageGCStore(t, deadRoot)
	out.Reset()
	downStore := unlockedStore(t)
	if err := gc.Run(context.Background(), &out, gc.ProbeRegistry, downStore, gc.RunCollector, registry.NewClient(down.URL), down.URL, deadCfg, registryBinPath, downStore, downStore, downStore, "127.0.0.1:1", gc.Options{}); err != nil {
		t.Fatalf("gc with dead post-probe = %v, want nil (warn only)", err)
	}
	if !strings.Contains(out.String(), "post-run probe") {
		t.Errorf("dead post-probe warned nothing:\n%s", out.String())
	}
}

// serveFlapRegistry answers the first upload-initiate with first and
// every later one with rest, serving sentinel files throughout so the
// proof passes and only the probe verdict moves.
func serveFlapRegistry(t *testing.T, root string, first, rest int) *httptest.Server {
	t.Helper()
	var posts int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/") {
			posts++
			status := rest
			if posts == 1 {
				status = first
			}
			if status == http.StatusAccepted {
				w.Header().Set("Location", "/v2/noroutine/kpr-gc-probe/blobs/uploads/uuid")
			}
			w.WriteHeader(status)
			return
		}
		serveSentinelFiles(w, r, root)
	}))
}

// A readonly registry runs the stock collector with the operator's
// flags after the fresh generation reads back — with no tracked rows
// at all: an empty redis proves as well as a full one. A failing
// collector surfaces (never silent). If this fails, gc either drops
// flags, demands rows it no longer needs, or swallows the exit.
func TestRunGCReadonlyRunsBinary(t *testing.T) {
	root := t.TempDir()
	srv := serveRegistry(t, root, false)
	defer srv.Close()

	cfg := stageGCStore(t, root)
	s := unlockedStore(t)

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "echo \"collector args: $@\"")
	defer func() { registryBinPath = oldBin }()

	var out strings.Builder
	if err := gc.Run(context.Background(), &out, gc.ProbeRegistry, s, gc.RunCollector, registry.NewClient(srv.URL), srv.URL, cfg, registryBinPath, s, s, s, "127.0.0.1:1", gc.Options{DeleteUntagged: true}); err != nil {
		t.Fatalf("readonly gc = %v, want nil", err)
	}
	for _, want := range []string{"garbage-collect", "--delete-untagged", "config.yml", "shared store proven via noroutine/kpr-sentinel:latest generation "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("collector invocation lacks %q:\n%s", want, out.String())
		}
	}

	registryBinPath = stageBin(t, "exit 3")
	var fail strings.Builder
	if err := gc.Run(context.Background(), &fail, gc.ProbeRegistry, s, gc.RunCollector, registry.NewClient(srv.URL), srv.URL, cfg, registryBinPath, s, s, s, "127.0.0.1:1", gc.Options{}); err == nil {
		t.Error("failing collector returned nil, want the exit surfaced")
	}
}

// The event renderer voices each stage loud: probe verdicts, the
// collector pid (a long mark phase must look alive), the flip
// WARNING, failures with cause. If this fails, gc runs quiet about
// exactly the moments the operator watches.
func TestRenderGCEventVoicesStages(t *testing.T) {
	var out strings.Builder
	report := renderGCEvent(&out, true)
	report(gc.Event{Stage: gc.StagePreProbe, Message: "readonly"})
	report(gc.Event{Stage: gc.StageStarted, PID: 4242})
	report(gc.Event{Stage: gc.StagePostProbe, Message: "readonly"})
	report(gc.Event{Stage: gc.StageModeFlip, Message: "readonly→writable"})
	report(gc.Event{Stage: gc.StageFailure, Error: "exit status 3: boom"})
	for _, want := range []string{
		"sentinel: registry is READONLY",
		"collector started (pid 4242)",
		"dry-run, nothing will be deleted",
		"sentinel: registry still READONLY",
		"WARNING: registry flipped readonly→writable mid-run",
		"collector failed: exit status 3: boom",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("rendered events lack %q:\n%s", want, out.String())
		}
	}
	var real strings.Builder
	renderGCEvent(&real, false)(gc.Event{Stage: gc.StageStarted, PID: 7})
	if strings.Contains(real.String(), "dry-run") {
		t.Errorf("real-run start claims dry-run:\n%s", real.String())
	}
	renderGCEvent(&real, false)(gc.Event{Stage: gc.StageStarted})
}
