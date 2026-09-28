package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// The gc sentinel initiates a blob upload under a probe repo: 202
// means the registry takes writes (the upload is cancelled right away,
// so no residue stays), 405 means v3 maintenance readonly, anything
// else is inconclusive. If this fails, kpr gc either collects from a
// writable registry blind or refuses a ready one.
func TestProbeRegistryModes(t *testing.T) {
	var sawDelete bool
	writable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", "/v2/kpr-gc-probe/blobs/uploads/uuid")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodDelete:
			sawDelete = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer writable.Close()
	readonly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer readonly.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	if mode, uuid, err := probeRegistry(context.Background(), writable.URL); err != nil || mode != probeWritable {
		t.Errorf("writable probe = (%v, %v), want (writable, nil)", mode, err)
	} else if uuid != "uuid" {
		t.Errorf("writable probe uuid = %q, want the Location tail", uuid)
	}
	if !sawDelete {
		t.Error("writable probe left the upload behind: no cancel DELETE seen")
	}
	if mode, _, err := probeRegistry(context.Background(), readonly.URL); err != nil || mode != probeReadonly {
		t.Errorf("readonly probe = (%v, %v), want (readonly, nil)", mode, err)
	}
	if mode, _, err := probeRegistry(context.Background(), broken.URL); err == nil || mode != probeUnknown {
		t.Errorf("broken probe = (%v, %v), want (unknown, error)", mode, err)
	}
	if mode, _, err := probeRegistry(context.Background(), "http://127.0.0.1:1"); err == nil || mode != probeUnknown {
		t.Errorf("down probe = (%v, %v), want (unknown, error)", mode, err)
	}
}

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
	dir := filepath.Join(root, "docker", "registry", "v2", "repositories", probeRepo, "_uploads", uuid)
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

// stageBin writes an executable shell stub as the collector binary.
func stageBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "collector.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("stage collector: %v", err)
	}
	return path
}

// Missing binary or config refuses with the remedy instead of failing
// mid-collect: gc degrades by construction when the store isn't
// shared into this container. If this fails, a bare image (no mounts)
// crashes on paths instead of explaining them.
func TestGCReadyGatesMissingPrereqs(t *testing.T) {
	if err := gcReady("/bin/registry", "/etc/distribution/config.yml"); err != nil {
		t.Logf("note: this host lacks %v (fine outside the image)", err)
	}
	if err := gcReady("/no/such/binary", "/etc/distribution/config.yml"); err == nil {
		t.Error("missing binary passed readiness, want refusal")
	} else if !strings.Contains(err.Error(), "/no/such/binary") {
		t.Errorf("refusal names no path: %v", err)
	}
	if err := gcReady("/bin/sh", "/no/such/config.yml"); err == nil {
		t.Error("missing config passed readiness, want refusal")
	} else if !strings.Contains(err.Error(), "/no/such/config.yml") {
		t.Errorf("refusal names no path: %v", err)
	}
}

// The store root comes from the registry config's filesystem section;
// anything else (s3, garbage, absent) refuses: local collection only
// understands the shared directory layout. If this fails, gc reads
// store paths from anywhere but the registry's own config.
func TestRegistryStoreRootParsesConfig(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(good, []byte("storage:\n  filesystem:\n    rootdirectory: /var/lib/registry\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if root, err := registryStoreRoot(good); err != nil || root != "/var/lib/registry" {
		t.Errorf("root = (%q, %v), want (/var/lib/registry, nil)", root, err)
	}
	s3 := filepath.Join(dir, "s3.yml")
	if err := os.WriteFile(s3, []byte("storage:\n  s3:\n    bucket: blobs\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if _, err := registryStoreRoot(s3); err == nil {
		t.Error("s3 config passed store root, want refusal")
	}
	if _, err := registryStoreRoot(filepath.Join(dir, "absent.yml")); err == nil {
		t.Error("absent config passed store root, want refusal")
	}
}

// The upload id is the path tail after "uploads" (query stripped): the
// handle both same-store proofs key on. If this fails, the proof looks
// for directories the registry never created.
func TestUploadUUIDParsesLocations(t *testing.T) {
	for loc, want := range map[string]string{
		"http://reg:5000/v2/probe/blobs/uploads/01a-2b?_state=x": "01a-2b",
		"/v2/probe/blobs/uploads/01a-2b":                         "01a-2b",
		"http://reg:5000/v2/_catalog":                            "",
		"":                                                       "",
	} {
		if got := uploadUUID(loc); got != want {
			t.Errorf("uploadUUID(%q) = %q, want %q", loc, got, want)
		}
	}
}

// A writable probe is only authoritative when its fresh upload dir is
// visible under the configured root: same bytes the registry just
// wrote, seen locally. If this fails, gc collects a stranger's store
// while pointed at the right URL.
func TestSameStoreUploadNeedsFreshDir(t *testing.T) {
	root := t.TempDir()
	uuid := "01a0e844-a69b-7d13-bd66-97efe391d879"
	if sameStoreUpload(root, uuid) {
		t.Error("absent upload dir proved same store, want false")
	}
	dir := filepath.Join(root, "docker", "registry", "v2", "repositories", probeRepo, "_uploads", uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("stage upload dir: %v", err)
	}
	if !sameStoreUpload(root, uuid) {
		t.Error("present upload dir proved nothing, want true")
	}
}

// Readonly proves nothing by writing, so the proof reads: a tracked
// tag's link file must resolve to the tracked digest. If this fails,
// gc in readonly mode trusts the URL alone.
func TestSameStoreTagLinkNeedsMatchingDigest(t *testing.T) {
	root := t.TempDir()
	digest := "sha256:094354e66a2a3da4f26955a83048fb9a5b6e36e8a972a3ea3628c2fcdd09a3cd"
	link := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", "_manifests", "tags", "v1", "current", "link")
	if sameStoreTagLink(root, "app", "v1", digest) {
		t.Error("absent link proved same store, want false")
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("stage link dir: %v", err)
	}
	if err := os.WriteFile(link, []byte("sha256:deadbeef\n"), 0o644); err != nil {
		t.Fatalf("stage link: %v", err)
	}
	if sameStoreTagLink(root, "app", "v1", digest) {
		t.Error("mismatched link proved same store, want false")
	}
	if err := os.WriteFile(link, []byte(digest+"\n"), 0o644); err != nil {
		t.Fatalf("stage link: %v", err)
	}
	if !sameStoreTagLink(root, "app", "v1", digest) {
		t.Error("matching link proved nothing, want true")
	}
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
	if err := runGC(context.Background(), &out, s, accept.URL, cfg, false, false); err == nil {
		t.Fatal("gc on writable registry succeeded without --force, want refusal")
	} else if !strings.Contains(err.Error(), "readonly") {
		t.Errorf("refusal names no remedy: %v", err)
	}

	registryBinPath = stageBin(t, "exit 0")
	out.Reset()
	if err := runGC(context.Background(), &out, s, accept.URL, cfg, false, true); err != nil {
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
	if err := runGC(context.Background(), &out, s, accept.URL, cfg, false, true); err == nil {
		t.Fatal("gc on a stranger's store succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "does not share") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// The collector's blobdescriptor cache must answer before anything is
// collected: an unreachable cache mis-marks (live layers look
// unreferenced) and the run deletes what it must keep. No redis
// section means inmemory cache — nothing to gate. If this fails, gc
// collects blind on a broken cache connection.
func TestGCCacheGateDialsRegistryRedis(t *testing.T) {
	dir := t.TempDir()
	withRedis := filepath.Join(dir, "redis.yml")
	if err := os.WriteFile(withRedis, []byte("redis:\n  addr: 127.0.0.1:1\n  password: wrong\n  db: 3\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if err := gcCacheReady(context.Background(), withRedis); err == nil {
		t.Error("unreachable redis cache passed, want refusal")
	} else if !strings.Contains(err.Error(), "blobdescriptor") {
		t.Errorf("refusal names no cause: %v", err)
	}
	plain := filepath.Join(dir, "plain.yml")
	if err := os.WriteFile(plain, []byte("storage:\n  filesystem:\n    rootdirectory: /var/lib/registry\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if err := gcCacheReady(context.Background(), plain); err != nil {
		t.Errorf("cacheless config gated: %v", err)
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

	if ok, err := s.AcquireGCLock(ctx, time.Minute); err != nil || !ok {
		t.Fatalf("pre-acquire = (%v, %v), want (true, nil)", ok, err)
	}
	var out strings.Builder
	if err := runGC(ctx, &out, s, deny.URL, cfg, false, false); err == nil {
		t.Fatal("gc under held lock succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "another gc") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if err := s.ReleaseGCLock(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	out.Reset()
	if err := runGC(ctx, &out, s, deny.URL, cfg, false, false); err != nil {
		t.Fatalf("gc after release = %v, want nil", err)
	}
	if ok, _ := s.AcquireGCLock(ctx, time.Minute); !ok {
		t.Error("lock still held after successful gc, want released")
	}
	_ = s.ReleaseGCLock(ctx)
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
	if err := runGC(context.Background(), &out, s, flap.URL, cfg, false, false); err == nil {
		t.Fatal("gc across a readonly→writable flip succeeded, want failure")
	} else if !strings.Contains(err.Error(), "changed during collection") {
		t.Errorf("failure names no cause: %v", err)
	}

	cfg2, s2, _ := staged(t)
	out.Reset()
	if err := runGC(context.Background(), &out, s2, down.URL, cfg2, false, false); err != nil {
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
	if err := runGC(context.Background(), &out, s, deny.URL, cfg, true, false); err != nil {
		t.Fatalf("readonly gc = %v, want nil", err)
	}
	for _, want := range []string{"garbage-collect", "--delete-untagged", "config.yml", "shared store proven via app:v1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("collector invocation lacks %q:\n%s", want, out.String())
		}
	}

	registryBinPath = stageBin(t, "exit 3")
	var fail strings.Builder
	if err := runGC(context.Background(), &fail, s, deny.URL, cfg, false, false); err == nil {
		t.Error("failing collector returned nil, want the exit surfaced")
	}

	empty := store.NewMemStore()
	var norows strings.Builder
	if err := runGC(context.Background(), &norows, empty, deny.URL, cfg, false, false); err == nil {
		t.Error("readonly gc with no tracked rows succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "no tracked digests") {
		t.Errorf("refusal names no cause: %v", err)
	}
}
