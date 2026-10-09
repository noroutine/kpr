package gc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			w.Header().Set("Location", "/v2/noroutine/kpr-gc-probe/blobs/uploads/uuid")
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

	if mode, uuid, err := probeRegistry(context.Background(), writable.URL); err != nil || mode != ModeWritable {
		t.Errorf("writable probe = (%v, %v), want (writable, nil)", mode, err)
	} else if uuid != "uuid" {
		t.Errorf("writable probe uuid = %q, want the Location tail", uuid)
	}
	if !sawDelete {
		t.Error("writable probe left the upload behind: no cancel DELETE seen")
	}
	if mode, _, err := probeRegistry(context.Background(), readonly.URL); err != nil || mode != ModeReadonly {
		t.Errorf("readonly probe = (%v, %v), want (readonly, nil)", mode, err)
	}
	if mode, _, err := probeRegistry(context.Background(), broken.URL); err == nil || mode != ModeUnknown {
		t.Errorf("broken probe = (%v, %v), want (unknown, error)", mode, err)
	}
	if mode, _, err := probeRegistry(context.Background(), "http://127.0.0.1:1"); err == nil || mode != ModeUnknown {
		t.Errorf("down probe = (%v, %v), want (unknown, error)", mode, err)
	}
}

// The mode wrapper names the inconclusive: a 500-serving peer is
// "unknown" with the sentinel named, not a bare error. If this
// fails, an erroring registry reads as a classified mode.
func TestProbeRegistryModeNamesInconclusive(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	if mode, err := ProbeRegistryMode(context.Background(), broken.URL); mode != "unknown" || err == nil {
		t.Fatalf("broken mode = (%q, %v), want (unknown, error)", mode, err)
	} else if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("refusal names no probe status: %v", err)
	}
	if mode, err := ProbeRegistryMode(context.Background(), "http://127.0.0.1:1"); mode != "unknown" || err == nil {
		t.Fatalf("down mode = (%q, %v), want (unknown, error)", mode, err)
	}
}

// An unbuildable request is unknown too: the probe never left, so
// no verdict exists. If this fails, a misconfigured registry URL
// panics path-building instead of refusing.
func TestProbeRegistryBadURLIsUnknown(t *testing.T) {
	if mode, _, err := probeRegistry(context.Background(), "http://exa\tmple.com"); err == nil || mode != ModeUnknown {
		t.Errorf("bad-URL probe = (%v, %v), want (unknown, error)", mode, err)
	}
}

// An upload id comes from the path segment after "uploads" —
// nothing else. A Location with no id (or no uploads leg at all)
// proves nothing, and must not panic the proof. If this fails, a
// registry answering odd Locations either crashes gc or mints
// evidence from thin air.
func TestUploadUUIDEdges(t *testing.T) {
	if got := uploadUUID("http://reg:5000/v2/noroutine/kpr-gc-probe/blobs/uploads/abc123"); got != "abc123" {
		t.Errorf("uploadUUID = %q, want abc123", got)
	}
	for _, loc := range []string{
		"",
		"http://reg:5000/v2/noroutine/kpr-gc-probe/blobs/uploads/",
		"http://reg:5000/v2/noroutine/kpr-gc-probe/blobs/uploads",
		"http://reg:5000/v2/repositories",
		"http://reg:5000/v2/noroutine/kpr-gc-probe/blobs/uploads/abc?digest=sha256:x",
	} {
		if loc == "http://reg:5000/v2/noroutine/kpr-gc-probe/blobs/uploads/abc?digest=sha256:x" {
			if got := uploadUUID(loc); got != "abc" {
				t.Errorf("uploadUUID(%q) = %q, want abc (query stripped)", loc, got)
			}
			continue
		}
		if got := uploadUUID(loc); got != "" {
			t.Errorf("uploadUUID(%q) = %q, want empty (proves nothing)", loc, got)
		}
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

func TestProbeBadLocationSkipsCancel(t *testing.T) {
	var sawDelete bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", "http://::invalid")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodDelete:
			sawDelete = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	mode, uuid, err := probeRegistry(context.Background(), srv.URL)
	if err != nil || mode != ModeWritable {
		t.Fatalf("bad-location probe = (%v, %q, %v), want (writable, \"\", nil)", mode, uuid, err)
	}
	if uuid != "" {
		t.Errorf("bad-location probe uuid = %q, want empty (proves nothing)", uuid)
	}
	if sawDelete {
		t.Error("bad-location probe attempted a cancel DELETE, want it skipped")
	}
}

// A failed cancel DELETE skips the close, cleanly: there is no
// body on an errored request. If this fails, a registry that takes
// the probe but refuses the cancel panics the probe on a nil body.
func TestProbeDeleteFailureSkipsClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1:1/v2/noroutine/kpr-gc-probe/blobs/uploads/u1")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	mode, uuid, err := probeRegistry(context.Background(), srv.URL)
	if err != nil || mode != ModeWritable {
		t.Fatalf("refused-cancel probe = (%v, %q, %v), want (writable, u1, nil)", mode, uuid, err)
	}
	if uuid != "u1" {
		t.Errorf("refused-cancel probe uuid = %q, want u1", uuid)
	}
}

// The string verdict feeds the e2e gate and the operator: writable,
// readonly, or unknown with the cause. If this fails, the gate reads
// a word the probe never said.
func TestProbeRegistryModeStrings(t *testing.T) {
	writable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer writable.Close()
	readonly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer readonly.Close()
	if mode, err := ProbeRegistryMode(context.Background(), writable.URL); err != nil || mode != "writable" {
		t.Errorf("mode = (%q, %v), want (writable, nil)", mode, err)
	}
	if mode, err := ProbeRegistryMode(context.Background(), readonly.URL); err != nil || mode != "readonly" {
		t.Errorf("mode = (%q, %v), want (readonly, nil)", mode, err)
	}
	if mode, err := ProbeRegistryMode(context.Background(), "http://127.0.0.1:1"); err == nil || mode != "unknown" {
		t.Errorf("mode = (%q, %v), want (unknown, error)", mode, err)
	}
	if got := ModeName(Mode(42)); got != "unknown" {
		t.Errorf("ModeName(42) = %q, want unknown", got)
	}
}
