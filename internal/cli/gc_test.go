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
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stageGCStore writes a registry config pointing at root and returns
// the config path: every runGC test collects against staged ground,
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

// stageUploadDir plants the uuid dir a writable probe must find.
func stageUploadDir(t *testing.T, root, uuid string) {
	t.Helper()
	dir := filepath.Join(root, "docker", "registry", "v2", "repositories", gc.ProbeRepo, "_uploads", uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("stage upload dir: %v", err)
	}
}

// stageTagLink plants a tag link resolving to digest.
func stageTagLink(t *testing.T, root, repo, tag, digest string) {
	t.Helper()
	link := filepath.Join(root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags", tag, "current", "link")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("stage link dir: %v", err)
	}
	if err := os.WriteFile(link, []byte(digest+"\n"), 0o644); err != nil {
		t.Fatalf("stage link: %v", err)
	}
}

// A lock release failure after a good run warns (with the TTL bound)
// instead of failing the run: the collection already happened. If
// this fails, a redis blip at release time rewrites history.
func TestRunGCReleaseFailureWarns(t *testing.T) {
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer deny.Close()

	root := t.TempDir()
	cfg := stageGCStore(t, root)
	digest := "sha256:094354e66a2a3da4f26955a83048fb9a5b6e36e8a972a3ea3628c2fcdd09a3cd"
	s := &releaseFailStore{MemStore: store.NewMemStore()}
	_ = s.Record(context.Background(), policy.Row{Repo: "app", Tag: "v1", Digest: digest, PushedAt: time.Now()})
	stageTagLink(t, root, "app", "v1", digest)

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	var out strings.Builder
	if err := runGC(context.Background(), &out, s, deny.URL, cfg, GCOptions{}); err != nil {
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
	accept := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "/v2/kpr-gc-probe/blobs/uploads/uuid")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer accept.Close()

	root := t.TempDir()
	cfg := stageGCStore(t, root)
	stageUploadDir(t, root, "uuid")

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	if err := runGC(context.Background(), errWriter{}, store.NewMemStore(), accept.URL, cfg, GCOptions{Force: true}); err == nil {
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
// the probe upload is visible in the local store. An invisible upload
// means a stranger's registry at the right URL and refuses even
// forced. If this fails, collection runs against live writes, or
// against the wrong store entirely.
func TestRunGCRefusesWritableWithoutForce(t *testing.T) {
	accept := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "/v2/kpr-gc-probe/blobs/uploads/uuid")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer accept.Close()

	root := t.TempDir()
	cfg := stageGCStore(t, root)
	stageUploadDir(t, root, "uuid")
	s := store.NewMemStore()

	oldBin := registryBinPath
	registryBinPath = "/bin/sh"
	defer func() { registryBinPath = oldBin }()

	var out strings.Builder
	if err := runGC(context.Background(), &out, s, accept.URL, cfg, GCOptions{}); err == nil {
		t.Fatal("gc on writable registry succeeded without --force, want refusal")
	} else if !strings.Contains(err.Error(), "readonly") {
		t.Errorf("refusal names no remedy: %v", err)
	}

	registryBinPath = stageBin(t, "exit 0")
	out.Reset()
	if err := runGC(context.Background(), &out, s, accept.URL, cfg, GCOptions{Force: true}); err != nil {
		t.Fatalf("forced gc = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "Warning") {
		t.Errorf("forced gc warned nothing:\n%s", out.String())
	}
}

// A stranger's registry at the right URL — probe upload invisible in
// the local store — refuses even forced. If this fails, gc happily
// collects whatever directory the mount points at.
func TestRunGCDifferentStoreRefuses(t *testing.T) {
	accept := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "/v2/kpr-gc-probe/blobs/uploads/uuid")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer accept.Close()

	root := t.TempDir()
	cfg := stageGCStore(t, root)
	s := store.NewMemStore()

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	var out strings.Builder
	if err := runGC(context.Background(), &out, s, accept.URL, cfg, GCOptions{Force: true}); err == nil {
		t.Fatal("gc on a stranger's store succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "does not share") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A held gc lock refuses the run: gc serializes on kpr:gc:lock (same
// key make gc honors), and a finished run releases it. If this fails,
// two collectors race the same store, or one run wedges the rest.
func TestRunGCLockContention(t *testing.T) {
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer deny.Close()

	ctx := context.Background()
	root := t.TempDir()
	cfg := stageGCStore(t, root)
	digest := "sha256:094354e66a2a3da4f26955a83048fb9a5b6e36e8a972a3ea3628c2fcdd09a3cd"
	s := store.NewMemStore()
	_ = s.Record(ctx, policy.Row{Repo: "app", Tag: "v1", Digest: digest, PushedAt: time.Now()})
	stageTagLink(t, root, "app", "v1", digest)

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	if ok, err := s.AcquireLock(ctx, store.GCLockKey, time.Minute); err != nil || !ok {
		t.Fatalf("pre-acquire = (%v, %v), want (true, nil)", ok, err)
	}
	var out strings.Builder
	if err := runGC(ctx, &out, s, deny.URL, cfg, GCOptions{}); err == nil {
		t.Fatal("gc under held lock succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "another gc") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if err := s.ReleaseLock(ctx, store.GCLockKey); err != nil {
		t.Fatalf("release: %v", err)
	}
	out.Reset()
	if err := runGC(ctx, &out, s, deny.URL, cfg, GCOptions{}); err != nil {
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
	flap := httptest.NewServer(flipFlop(405, 202))
	defer flap.Close()
	down := httptest.NewServer(flipFlop(405, 500))
	defer down.Close()

	staged := func(t *testing.T) (string, *store.MemStore, string) {
		root := t.TempDir()
		cfg := stageGCStore(t, root)
		digest := "sha256:094354e66a2a3da4f26955a83048fb9a5b6e36e8a972a3ea3628c2fcdd09a3cd"
		s := store.NewMemStore()
		_ = s.Record(context.Background(), policy.Row{Repo: "app", Tag: "v1", Digest: digest, PushedAt: time.Now()})
		stageTagLink(t, root, "app", "v1", digest)
		return cfg, s, root
	}
	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	cfg, s, _ := staged(t)
	var out strings.Builder
	if err := runGC(context.Background(), &out, s, flap.URL, cfg, GCOptions{}); err == nil {
		t.Fatal("gc across a readonly→writable flip succeeded, want failure")
	} else if !strings.Contains(err.Error(), "changed during collection") {
		t.Errorf("failure names no cause: %v", err)
	}

	cfg2, s2, _ := staged(t)
	out.Reset()
	if err := runGC(context.Background(), &out, s2, down.URL, cfg2, GCOptions{}); err != nil {
		t.Fatalf("gc with dead post-probe = %v, want nil (warn only)", err)
	}
	if !strings.Contains(out.String(), "post-run probe") {
		t.Errorf("dead post-probe warned nothing:\n%s", out.String())
	}
}

// flipFlop answers the first upload-initiate with first and every later
// one with rest: a mode change mid-run on demand.
func flipFlop(first, rest int) http.HandlerFunc {
	var calls int
	return func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(first)
			return
		}
		w.WriteHeader(rest)
	}
}

// A readonly registry runs the stock collector with the operator's
// flags — but only after a tracked tag resolves to its digest in the
// local store. No tracked rows, or a mismatched link, refuses: URL
// alone proves nothing. A failing collector surfaces (never silent).
// If this fails, gc either drops flags, trusts strangers, or swallows
// the collector's exit.
func TestRunGCReadonlyRunsBinary(t *testing.T) {
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer deny.Close()

	root := t.TempDir()
	cfg := stageGCStore(t, root)
	digest := "sha256:094354e66a2a3da4f26955a83048fb9a5b6e36e8a972a3ea3628c2fcdd09a3cd"
	s := store.NewMemStore()
	_ = s.Record(context.Background(), policy.Row{Repo: "app", Tag: "v1", Digest: digest, PushedAt: time.Now()})
	stageTagLink(t, root, "app", "v1", digest)

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "echo \"collector args: $@\"")
	defer func() { registryBinPath = oldBin }()

	var out strings.Builder
	if err := runGC(context.Background(), &out, s, deny.URL, cfg, GCOptions{DeleteUntagged: true}); err != nil {
		t.Fatalf("readonly gc = %v, want nil", err)
	}
	for _, want := range []string{"garbage-collect", "--delete-untagged", "config.yml", "shared store proven via app:v1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("collector invocation lacks %q:\n%s", want, out.String())
		}
	}

	registryBinPath = stageBin(t, "exit 3")
	var fail strings.Builder
	if err := runGC(context.Background(), &fail, s, deny.URL, cfg, GCOptions{}); err == nil {
		t.Error("failing collector returned nil, want the exit surfaced")
	}

	empty := store.NewMemStore()
	var norows strings.Builder
	if err := runGC(context.Background(), &norows, empty, deny.URL, cfg, GCOptions{}); err == nil {
		t.Error("readonly gc with no tracked rows succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "no tracked digests") {
		t.Errorf("refusal names no cause: %v", err)
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
