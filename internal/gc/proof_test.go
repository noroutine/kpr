package gc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A sentinel upload nobody can find is a different store — false,
// after the retry bound, not a hang. If this fails, gc blocks on a
// stranger's registry instead of refusing it.
func TestSameStoreUploadMissingDir(t *testing.T) {
	if SameStoreUpload(t.TempDir(), "no-such-uuid") {
		t.Error("missing upload dir proved same-store, want false")
	}
}

// A writable probe is only authoritative when its fresh upload dir is
// visible under the configured root: same bytes the registry just
// wrote, seen locally. If this fails, gc collects a stranger's store
// while pointed at the right URL.
func TestSameStoreUploadNeedsFreshDir(t *testing.T) {
	root := t.TempDir()
	uuid := "01a0e844-a69b-7d13-bd66-97efe391d879"
	if SameStoreUpload(root, uuid) {
		t.Error("absent upload dir proved same store, want false")
	}
	dir := filepath.Join(root, "docker", "registry", "v2", "repositories", ProbeRepo, "_uploads", uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("stage upload dir: %v", err)
	}
	if !SameStoreUpload(root, uuid) {
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
	if SameStoreTagLink(root, "app", "v1", digest) {
		t.Error("absent link proved same store, want false")
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("stage link dir: %v", err)
	}
	if err := os.WriteFile(link, []byte("sha256:deadbeef\n"), 0o644); err != nil {
		t.Fatalf("stage link: %v", err)
	}
	if SameStoreTagLink(root, "app", "v1", digest) {
		t.Error("mismatched link proved same store, want false")
	}
	if err := os.WriteFile(link, []byte(digest+"\n"), 0o644); err != nil {
		t.Fatalf("stage link: %v", err)
	}
	if !SameStoreTagLink(root, "app", "v1", digest) {
		t.Error("matching link proved nothing, want true")
	}
}

// Missing binary or config refuses with the remedy instead of failing
// mid-collect: gc degrades by construction when the store isn't
// shared into this container. If this fails, a bare image (no mounts)
// crashes on paths instead of explaining them.
func TestReadyGatesMissingPrereqs(t *testing.T) {
	if err := Ready("/bin/registry", "/etc/distribution/config.yml"); err != nil {
		t.Logf("note: this host lacks %v (fine outside the image)", err)
	}
	if err := Ready("/no/such/binary", "/etc/distribution/config.yml"); err == nil {
		t.Error("missing binary passed readiness, want refusal")
	} else if !strings.Contains(err.Error(), "/no/such/binary") {
		t.Errorf("refusal names no path: %v", err)
	}
	if err := Ready("/bin/sh", "/no/such/config.yml"); err == nil {
		t.Error("missing config passed readiness, want refusal")
	} else if !strings.Contains(err.Error(), "/no/such/config.yml") {
		t.Errorf("refusal names no path: %v", err)
	}
}

// The store root comes from the registry config's filesystem section;
// anything else (s3, garbage, absent) refuses: local collection only
// understands the shared directory layout. If this fails, gc reads
// store paths from anywhere but the registry's own config.
func TestStoreRootParsesConfig(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(good, []byte("storage:\n  filesystem:\n    rootdirectory: /var/lib/registry\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if root, err := StoreRoot(good); err != nil || root != "/var/lib/registry" {
		t.Errorf("root = (%q, %v), want (/var/lib/registry, nil)", root, err)
	}
	s3 := filepath.Join(dir, "s3.yml")
	if err := os.WriteFile(s3, []byte("storage:\n  s3:\n    bucket: blobs\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if _, err := StoreRoot(s3); err == nil {
		t.Error("s3 config passed store root, want refusal")
	}
	if _, err := StoreRoot(filepath.Join(dir, "absent.yml")); err == nil {
		t.Error("absent config passed store root, want refusal")
	}
}

// The collector's blobdescriptor cache must answer before anything is
// collected: an unreachable cache mis-marks (live layers look
// unreferenced) and the run deletes what it must keep. No redis
// section means inmemory cache — nothing to gate. If this fails, gc
// collects blind on a broken cache connection.
func TestCacheGateDialsRegistryRedis(t *testing.T) {
	dir := t.TempDir()
	withRedis := filepath.Join(dir, "redis.yml")
	if err := os.WriteFile(withRedis, []byte("redis:\n  addr: 127.0.0.1:1\n  password: wrong\n  db: 3\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if err := CacheReady(context.Background(), withRedis); err == nil {
		t.Error("unreachable redis cache passed, want refusal")
	} else if !strings.Contains(err.Error(), "blobdescriptor") {
		t.Errorf("refusal names no cause: %v", err)
	}
	plain := filepath.Join(dir, "plain.yml")
	if err := os.WriteFile(plain, []byte("storage:\n  filesystem:\n    rootdirectory: /var/lib/registry\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if err := CacheReady(context.Background(), plain); err != nil {
		t.Errorf("cacheless config gated: %v", err)
	}
}
