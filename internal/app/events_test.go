package app

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

type errKeep string

func (e errKeep) Error() string { return string(e) }

var eventsNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func pushEnvelope() string {
	return `{"events":[{
		"id":"e1",
		"timestamp":"2026-09-27T12:00:00Z",
		"action":"push",
		"target":{"mediaType":"application/vnd.oci.image.manifest.v1+json",
			"digest":"sha256:abc","repository":"scratch","tag":"10m"},
		"actor":{"name":"dev"}
	}]}`
}

func postEvents(t *testing.T, s store.Store, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	rr := httptest.NewRecorder()
	EventsHandler(s)(rr, req)
	return rr
}

// A registry push notification must land as a tracked row carrying the
// receiver-stamped push time, digest, and actor — the generous ingest
// reap reasons about without manifest fetches. If this fails, pushes
// never enter redis and every policy reasons about nothing.
func TestEventsHandlerRecordsPush(t *testing.T) {
	s := store.NewMemStore()
	rr := postEvents(t, s, pushEnvelope())
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	rows, err := s.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Repo != "scratch" || r.Tag != "10m" || r.Digest != "sha256:abc" {
		t.Errorf("row = %+v, want scratch:10m@sha256:abc", r)
	}
	if !r.PushedAt.Equal(eventsNow) {
		t.Errorf("PushedAt = %v, want the event timestamp (push-time anchor)", r.PushedAt)
	}
	if r.Actor != "dev" || r.MediaType == "" {
		t.Errorf("row = %+v, want actor and media type recorded", r)
	}
}

// Pulls, deletes, and mounts are not ingest: recording them would mint
// rows for tags nobody pushed (or re-anchor live ones). If this fails,
// read traffic pollutes the tracked set.
func TestEventsHandlerIgnoresNonPush(t *testing.T) {
	s := store.NewMemStore()
	body := `{"events":[
		{"action":"pull","target":{"repository":"app","tag":"v1","digest":"sha256:x"}},
		{"action":"delete","target":{"repository":"app","tag":"v1","digest":"sha256:x"}},
		{"action":"mount","target":{"repository":"app","tag":"v1","digest":"sha256:x"}}
	]}`
	if rr := postEvents(t, s, body); rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	if rows, _ := s.All(context.Background()); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 (non-push ignored)", len(rows))
	}
}

// A digest-only push (no tag) has no TTL identity to track: it is
// accepted and skipped, not recorded as an empty-tag row. If this
// fails, blob pushes mint phantom rows reap cannot reason about.
func TestEventsHandlerSkipsTaglessPush(t *testing.T) {
	s := store.NewMemStore()
	body := `{"events":[{
		"action":"push",
		"target":{"repository":"app","digest":"sha256:blob"},
		"timestamp":"2026-09-27T12:00:00Z"
	}]}`
	if rr := postEvents(t, s, body); rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	if rows, _ := s.All(context.Background()); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 (tagless skipped)", len(rows))
	}
}

// Garbage into the endpoint is a 400, not a panic and not a silent
// accept. If this fails, a misconfigured registry webhook wedges or
// poisons the receiver.
func TestEventsHandlerRejectsBadJSON(t *testing.T) {
	s := store.NewMemStore()
	if rr := postEvents(t, s, "{nope"); rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Redis down means the push stays untracked — accepted and visible,
// not silent: 202 plus a log line naming the drop. If this fails, gap
// pushes vanish without a trace (or distribution retries pile up on a
// 500 while redis is away).
func TestEventsHandlerRecordErrorIsAcceptedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	if rr := postEvents(t, failStore{}, pushEnvelope()); rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (accepted, drop logged)", rr.Code)
	}
	if !strings.Contains(buf.String(), "failed to record") {
		t.Errorf("log = %q, want the visible drop", buf.String())
	}
}

// failStore is a Store whose writes always fail (redis down).
type failStore struct{ store.Store }

func (failStore) Record(context.Context, policy.Row) error { return errRecordKeep }

var errRecordKeep = errKeep("redis down")
