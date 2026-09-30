package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// stubAPI is the second implementation behind the API port: canned
// manifest/blob bytes instead of a registry. The e2e suite carries
// the production one (internal/registry over a real registry:3).
type stubAPI struct {
	manifest []byte
	blob     []byte
	err      error
}

func (s *stubAPI) GetManifest(_ context.Context, _, _ string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.manifest, nil
}

func (s *stubAPI) GetBlob(_ context.Context, _, _ string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.blob, nil
}

// Reading back a written generation returns its payload: manifest by
// tag, config digest out of the manifest, payload blob behind it. If
// this fails, the proof compares something other than what the store
// serves.
func TestReadReturnsWrittenPayload(t *testing.T) {
	root := t.TempDir()
	want := Payload{V: 1, Gen: 3, TS: "2026-09-30T12:00:00Z", Writer: "test"}
	md, err := Write(root, "kpr-sentinel", "live", want)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	manRaw, err := os.ReadFile(blobFile(root, md))
	if err != nil {
		t.Fatalf("read manifest blob: %v", err)
	}
	var man struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manRaw, &man); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	payRaw, err := os.ReadFile(blobFile(root, man.Config.Digest))
	if err != nil {
		t.Fatalf("read payload blob: %v", err)
	}
	api := &stubAPI{manifest: manRaw, blob: payRaw}

	got, gotMD, err := Read(context.Background(), api, "kpr-sentinel", "live")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Errorf("payload = %+v, want %+v", got, want)
	}
	if gotMD != md {
		t.Errorf("manifest digest = %q, want %q", gotMD, md)
	}
}

// Whatever the API cannot serve is a refusal, never a zero payload:
// unknown tag, unparseable manifest, missing config reference,
// missing blob. If this fails, a stranger's store (or a blip)
// verifies as generation 0.
func TestReadRefusesUnexplained(t *testing.T) {
	good := &stubAPI{
		manifest: []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc","size":1}}`),
		blob:     []byte(`{"v":1,"gen":2}`),
	}
	for _, tc := range []struct {
		name string
		api  API
	}{
		{"down registry", &stubAPI{err: errors.New("connection refused")}},
		{"garbage manifest", &stubAPI{manifest: []byte("not json"), blob: []byte(`{}`)}},
		{"wrong shape manifest", &stubAPI{manifest: []byte(`{"hello":1}`), blob: []byte(`{}`)}},
		{"missing config digest", &stubAPI{manifest: []byte(`{"schemaVersion":2,"config":{}}`), blob: []byte(`{}`)}},
		{"missing blob", &stubAPI{manifest: good.manifest}},
		{"garbage payload", &stubAPI{manifest: good.manifest, blob: []byte("not json")}},
		{"wrong shape payload", &stubAPI{manifest: good.manifest, blob: []byte(`{"hello":1}`)}},
	} {
		if _, _, err := Read(context.Background(), tc.api, "kpr-sentinel", "live"); err == nil {
			t.Errorf("%s read clean, want refusal", tc.name)
		}
	}
}

// Verify is the proof primitive: the served generation must equal
// the written one. A mismatch means a different store (or a stale
// snapshot), never a pass. If this fails, gc collects a stranger's
// store on a matching URL.
func TestVerifyMatchesGeneration(t *testing.T) {
	api := &stubAPI{
		manifest: []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc","size":1}}`),
		blob:     []byte(`{"v":1,"gen":5}`),
	}
	if err := Verify(context.Background(), api, "kpr-sentinel", "live", 5); err != nil {
		t.Errorf("Verify(gen 5) = %v, want nil", err)
	}
	if err := Verify(context.Background(), api, "kpr-sentinel", "live", 6); err == nil {
		t.Error("Verify(gen 6) against served 5 passed, want mismatch")
	}
}

// A served-but-wrong generation is a typed Mismatch: the store
// answered with sentinel content this writer did not write — a stale
// snapshot, not an unreachable registry. Backfill (M4) keys its
// snapshot policy off this distinction, so it must survive as a type,
// not a substring. If this fails, every refusal looks the same and
// staleness is unnameable.
func TestVerifyMismatchIsTyped(t *testing.T) {
	api := &stubAPI{
		manifest: []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc","size":1}}`),
		blob:     []byte(`{"v":1,"gen":5}`),
	}
	err := Verify(context.Background(), api, "kpr-sentinel", "live", 6)
	var mm *Mismatch
	if !errors.As(err, &mm) {
		t.Fatalf("Verify error = %v (%T), want *Mismatch", err, err)
	}
	if mm.Got != 5 || mm.Want != 6 {
		t.Errorf("Mismatch = %+v, want Got 5 Want 6", mm)
	}
}

// No evidence at all (down registry, unknown tag) is a plain error,
// never a Mismatch: nothing answered, so nothing is stale. If this
// fails, an outage reads as a snapshot and the wrong policy fires.
func TestVerifyReadFailureIsNotMismatch(t *testing.T) {
	err := Verify(context.Background(), &stubAPI{err: errors.New("connection refused")}, "kpr-sentinel", "live", 6)
	if err == nil {
		t.Fatal("Verify on dead registry passed, want refusal")
	}
	var mm *Mismatch
	if errors.As(err, &mm) {
		t.Errorf("dead-registry error = %v, want plain error, not *Mismatch", err)
	}
}
