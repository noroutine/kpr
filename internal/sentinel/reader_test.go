package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
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
	want := Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000003", ID: testIdentity, TS: "2026-09-30T12:00:00Z", Writer: "test"}
	md, err := Write(root, Repo, Tag, want)
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

	got, gotMD, err := Read(context.Background(), api, Repo, Tag)
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
		blob:     []byte(`{"v":1,"gen":"0193abcd-0000-7000-8000-000000000002"}`),
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
		if _, _, err := Read(context.Background(), tc.api, Repo, Tag); err == nil {
			t.Errorf("%s read clean, want refusal", tc.name)
		}
	}
}

// Verify is the proof primitive: the served generation must equal
// the written one. A mismatch means a different store (or a stale
// snapshot), never a pass. If this fails, gc collects a stranger's
// store on a matching URL.
// statusErr is a local 404/500 carrier: Absent matches the status
// interface, never the production client.
type statusErr struct{ status int }

func (e statusErr) Error() string   { return "status" }
func (e statusErr) StatusCode() int { return e.status }

// Silence is a 404-shaped answer or a missing file — corruption
// (unparseable bytes) and transport failure refuse as unreadable,
// and nil is never absence. If this fails, establish and refuse-dry
// trigger on the wrong bucket.
func TestAbsentClassifiesSilence(t *testing.T) {
	for _, err := range []error{
		statusErr{status: 404},
		fmt.Errorf("read: %w", statusErr{status: 404}),
		os.ErrNotExist,
	} {
		if !Absent(err) {
			t.Errorf("Absent(%v) = false, want silence", err)
		}
	}
	for _, err := range []error{
		nil,
		statusErr{status: 500},
		errors.New("connection refused"),
		errors.New("payload unparseable"),
	} {
		if Absent(err) {
			t.Errorf("Absent(%v) = true, want unreadable", err)
		}
	}
}

func TestVerifyMatchesGeneration(t *testing.T) {
	api := &stubAPI{
		manifest: []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc","size":1}}`),
		blob:     []byte(`{"v":1,"gen":"0193abcd-0000-7000-8000-000000000005"}`),
	}
	if err := Verify(context.Background(), api, Repo, Tag, "0193abcd-0000-7000-8000-000000000005"); err != nil {
		t.Errorf("Verify(served gen) = %v, want nil", err)
	}
	if err := Verify(context.Background(), api, Repo, Tag, "0193abcd-0000-7000-8000-000000000006"); err == nil {
		t.Error("Verify(other gen) against served 5 passed, want mismatch")
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
		blob:     []byte(`{"v":1,"gen":"0193abcd-0000-7000-8000-000000000005"}`),
	}
	err := Verify(context.Background(), api, Repo, Tag, "0193abcd-0000-7000-8000-000000000006")
	var mm *Mismatch
	if !errors.As(err, &mm) {
		t.Fatalf("Verify error = %v (%T), want *Mismatch", err, err)
	}
	if mm.Got != "0193abcd-0000-7000-8000-000000000005" || mm.Want != "0193abcd-0000-7000-8000-000000000006" {
		t.Errorf("Mismatch = %+v, want served vs wanted", mm)
	}
}

// No evidence at all (down registry, unknown tag) is a plain error,
// never a Mismatch: nothing answered, so nothing is stale. If this
// fails, an outage reads as a snapshot and the wrong policy fires.
func TestVerifyReadFailureIsNotMismatch(t *testing.T) {
	err := Verify(context.Background(), &stubAPI{err: errors.New("connection refused")}, Repo, Tag, "0193abcd-0000-7000-8000-000000000006")
	if err == nil {
		t.Fatal("Verify on dead registry passed, want refusal")
	}
	var mm *Mismatch
	if errors.As(err, &mm) {
		t.Errorf("dead-registry error = %v, want plain error, not *Mismatch", err)
	}
}

// LastProof returns whatever the fixed address serves without taking
// a position on it: callers compare age and decide. Absence refuses
// — "unproven" is a fact about the store, never generation zero. If
// this fails, the console and the sweep loop voice a generation
// nobody proved.
func TestLastProofServesLivePayload(t *testing.T) {
	api := &stubAPI{
		manifest: []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc"}}`),
		blob:     []byte(`{"v":1,"gen":"019-live","ts":"2026-09-30T12:00:00Z","writer":"kpr-gc"}`),
	}
	p, err := LastProof(context.Background(), api)
	if err != nil {
		t.Fatalf("LastProof: %v", err)
	}
	if p.Gen != "019-live" {
		t.Errorf("Gen = %q, want 019-live", p.Gen)
	}
	if _, err := LastProof(context.Background(), &stubAPI{err: errors.New("down")}); err == nil {
		t.Errorf("LastProof on a dead registry = nil, want refusal")
	}
}

// Age comes from the payload's wall timestamp, never the read
// moment: a generation proven an hour ago is an hour old however
// often it is read. Unparseable timestamps refuse instead of
// reading as zero — an age nobody can compute is not fresh. If
// this fails, staleness lines voice the wrong column.
func TestPayloadAge(t *testing.T) {
	now := time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		ts    string
		want  time.Duration
		refus bool
	}{
		{"fresh", "2026-09-30T12:00:00Z", time.Hour, false},
		{"stale", "2026-09-22T13:00:00Z", 8 * 24 * time.Hour, false},
		{"malformed", "last tuesday", 0, true},
		{"empty", "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Payload{Gen: "g", TS: tc.ts}.Age(now)
			if tc.refus {
				if err == nil {
					t.Fatalf("Age(%q) = %v, want refusal", tc.ts, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Age: %v", err)
			}
			if got != tc.want {
				t.Errorf("Age = %v, want %v", got, tc.want)
			}
		})
	}
}
