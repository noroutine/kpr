package fakes_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/testing/fakes"
)

// GetBlob on a repo without the link refuses naming it: the fake
// mirrors the registry's missing-blob behavior for backfill's
// skip paths. If this fails, the fake invents blobs the registry
// never served.
func TestGetBlobRefusesMissingLink(t *testing.T) {
	f := fakes.FileAPI{Root: t.TempDir()}
	digest := "sha256:" + strings.Repeat("a", 64)
	if _, err := f.GetBlob(context.Background(), "app", digest); err == nil {
		t.Error("GetBlob on linkless repo succeeded, want refusal")
	}
}

// GetBlob serves a linked blob off the fake layout: the happy
// path my refusal pin above leaves dark. If this fails, the
// fake's registry face cannot serve what it staged.
func TestGetBlobServesStagedBlob(t *testing.T) {
	root := t.TempDir()
	hex := strings.Repeat("b", 64)
	link := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", "_layers", "sha256", hex)
	data := filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", hex[:2], hex)
	mustMkdir(t, link, data)
	if err := os.WriteFile(filepath.Join(link, "link"), []byte("sha256:"+hex), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "data"), []byte("blob-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := fakes.FileAPI{Root: root}
	got, err := f.GetBlob(context.Background(), "app", "sha256:"+hex)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if string(got) != "blob-bytes" {
		t.Errorf("GetBlob = %q, want blob-bytes", got)
	}
}

// Delete without a staged outage delegates to the embedded store:
// the fake removes the row instead of refusing. If this fails, the
// happy path through FailRows never runs and its delegate rots.
func TestFailRowsDeleteDelegates(t *testing.T) {
	ctx := context.Background()
	s := store.NewMemStore()
	row := policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:x", PushedAt: time.Now().UTC()}
	if err := s.Record(ctx, row); err != nil {
		t.Fatalf("stage row: %v", err)
	}
	f := fakes.FailRows{MemStore: s}
	if err := f.Delete(ctx, "app", "v1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := s.Get(ctx, "app", "v1"); err != nil || ok {
		t.Errorf("Get after Delete = (%v, %v), want (false, nil)", ok, err)
	}
}

func mustMkdir(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// An ErrIdentityStore propagates its error on both ports: Set is
// not a quieter Get. If this fails, unlock tests stage an outage
// the fake does not deliver.
func TestErrIdentityStoreSetPropagates(t *testing.T) {
	want := errors.New("store down")
	e := fakes.ErrIdentityStore{Err: want}
	if err := e.SetIdentity(context.Background(), store.Identity{}); !errors.Is(err, want) {
		t.Errorf("SetIdentity = %v, want %v", err, want)
	}
}

// The map lease surface round-trips bytes, misses like absence,
// drops on Del, and records (never honors) TTLs. If this fails,
// the shared conformance's second medium lies.
func TestMemLeaseConnRoundTrips(t *testing.T) {
	m := fakes.NewMemLeaseConn()
	ctx := context.Background()
	if _, err := m.Get(ctx, "k"); err == nil {
		t.Error("Get on empty surface succeeded, want the miss")
	}
	if err := m.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if raw, err := m.Get(ctx, "k"); err != nil || string(raw) != "v" {
		t.Errorf("Get = (%q,%v), want (v,nil)", raw, err)
	}
	if got := m.TTL("k"); got != time.Minute {
		t.Errorf("TTL = %v, want what Hold asked", got)
	}
	if err := m.Del(ctx, "k"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if _, err := m.Get(ctx, "k"); err == nil {
		t.Error("Get after Del succeeded, want the miss")
	}
}
