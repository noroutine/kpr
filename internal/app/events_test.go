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
	if r.Actor != "kpr-receiver" || r.MediaType == "" {
		t.Errorf("row = %+v, want the receiver actor and media type recorded", r)
	}
}

// The notification's actor name is nobody's business here: basic
// auth or anonymous, the row signs the recording component. If
// this fails, pusher names (or blanks) leak into a field that
// means "who recorded this".
func TestEventsHandlerActorNamesComponent(t *testing.T) {
	for _, body := range []string{
		`{"events":[{"action":"push","target":{"repository":"scratch","tag":"a","digest":"sha256:x"},"timestamp":"2026-09-27T12:00:00Z","actor":{"name":"dev"}}]}`,
		`{"events":[{"action":"push","target":{"repository":"scratch","tag":"b","digest":"sha256:y"},"timestamp":"2026-09-27T12:00:00Z"}]}`,
	} {
		s := store.NewMemStore()
		if rr := postEvents(t, s, body); rr.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rr.Code)
		}
		rows, err := s.All(context.Background())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(rows) != 1 || rows[0].Actor != "kpr-receiver" {
			t.Errorf("rows = %+v, want one row signed kpr-receiver", rows)
		}
	}
}

// Pulls and mounts are not ingest: recording them would create
// rows for tags nobody pushed (or re-anchor live ones). If this
// fails, read traffic pollutes the tracked set.
func TestEventsHandlerIgnoresNonPush(t *testing.T) {
	s := store.NewMemStore()
	body := `{"events":[
		{"action":"pull","target":{"repository":"app","tag":"v1","digest":"sha256:x"}},
		{"action":"mount","target":{"repository":"app","tag":"v1","digest":"sha256:x"}}
	]}`
	if rr := postEvents(t, s, body); rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	if rows, _ := s.All(context.Background()); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 (non-push ignored)", len(rows))
	}
}

// A registry delete notification drops the row: the tag is gone,
// so the plan must not reason about it anymore (no 7-day grace
// here — the registry said so, not a failed catalog read). If
// this fails, out-of-band deletes leave stale rows the sweep only
// converges on past the untagged grace.
func TestEventsHandlerDeleteDropsRow(t *testing.T) {
	s := store.NewMemStore()
	if rr := postEvents(t, s, pushEnvelope()); rr.Code != http.StatusAccepted {
		t.Fatalf("push status = %d, want 202", rr.Code)
	}
	body := `{"events":[{
		"action":"delete",
		"target":{"repository":"scratch","tag":"10m","digest":"sha256:abc"}
	}]}`
	if rr := postEvents(t, s, body); rr.Code != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202", rr.Code)
	}
	if rows, _ := s.All(context.Background()); len(rows) != 0 {
		t.Errorf("rows = %+v, want the deleted row dropped", rows)
	}
}

// A delete for an untracked tag is a no-op accept: idempotent,
// never a failure. If this fails, deletes race the receiver's
// own bookkeeping into errors.
func TestEventsHandlerDeleteUntrackedIsAccepted(t *testing.T) {
	s := store.NewMemStore()
	body := `{"events":[{
		"action":"delete",
		"target":{"repository":"app","tag":"v1","digest":"sha256:x"}
	}]}`
	if rr := postEvents(t, s, body); rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
}

// A digest-only push (no tag) has no TTL identity to track: it is
// accepted and skipped, not recorded as an empty-tag row. If this
// fails, blob pushes create phantom rows reap cannot reason about.
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

// failStore is a Store whose reads and writes always fail (redis
// down).
type failStore struct{ store.Store }

func (failStore) Record(context.Context, policy.Row) error { return errRecordKeep }

func (failStore) Get(context.Context, string, string) (policy.Row, bool, error) {
	return policy.Row{}, false, errRecordKeep
}

func (failStore) Delete(context.Context, string, string) error { return errRecordKeep }

// failDeleteStore reads fine but never drops: the delete path's
// own outage, past the guard read.
type failDeleteStore struct{ *store.MemStore }

func (failDeleteStore) Delete(context.Context, string, string) error { return errRecordKeep }

var errRecordKeep = errKeep("redis down")

// Redis down means the delete stays tracked — accepted and
// visible, not silent: 202 plus a log line naming the drop. If
// this fails, deletes during an outage read as processed while
// the stale row lingers.
func TestEventsHandlerDeleteErrorIsAcceptedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	body := `{"events":[{
		"action":"delete",
		"target":{"repository":"app","tag":"v1","digest":"sha256:x"}
	}]}`
	if rr := postEvents(t, failDeleteStore{store.NewMemStore()}, body); rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (accepted, drop logged)", rr.Code)
	}
	if !strings.Contains(buf.String(), "failed to drop") {
		t.Errorf("log = %q, want the visible drop", buf.String())
	}
}

// A failed guard read refuses the drop the same loud way: never
// delete blind when the row cannot be read. If this fails, a
// backend blip during the guard turns into an unchecked delete.
func TestEventsHandlerDeleteGuardErrorIsAcceptedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	body := `{"events":[{
		"action":"delete",
		"target":{"repository":"app","tag":"v1","digest":"sha256:x"}
	}]}`
	if rr := postEvents(t, failStore{}, body); rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (accepted, drop logged)", rr.Code)
	}
	if !strings.Contains(buf.String(), "failed to read") {
		t.Errorf("log = %q, want the visible guard failure", buf.String())
	}
}

// A delete naming a stale digest keeps the re-pushed row: the
// event belongs to the generation the tag no longer serves
// (replay or reorder). If this fails, a replayed delete erases
// live tracking.
func TestEventsHandlerStaleDeleteKeepsRepushedRow(t *testing.T) {
	s := store.NewMemStore()
	if rr := postEvents(t, s, pushEnvelope()); rr.Code != http.StatusAccepted {
		t.Fatalf("push status = %d, want 202", rr.Code)
	}
	body := `{"events":[{
		"action":"delete",
		"target":{"repository":"scratch","tag":"10m","digest":"sha256:stale"}
	}]}`
	if rr := postEvents(t, s, body); rr.Code != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202", rr.Code)
	}
	rows, _ := s.All(context.Background())
	if len(rows) != 1 || rows[0].Digest != "sha256:abc" {
		t.Errorf("rows = %+v, want the re-pushed row kept", rows)
	}
}

// Push and delete for one tag in one envelope apply in order:
// the batch records, then drops. If this fails, batch order is
// not the envelope order.
func TestEventsHandlerPushThenDeleteInOneEnvelope(t *testing.T) {
	s := store.NewMemStore()
	body := `{"events":[
		{"action":"push","target":{"repository":"scratch","tag":"10m","digest":"sha256:abc","mediaType":"application/vnd.oci.image.manifest.v1+json"},"timestamp":"2026-09-27T12:00:00Z"},
		{"action":"delete","target":{"repository":"scratch","tag":"10m","digest":"sha256:abc"}}
	]}`
	if rr := postEvents(t, s, body); rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	if rows, _ := s.All(context.Background()); len(rows) != 0 {
		t.Errorf("rows = %+v, want the pushed-then-deleted row dropped", rows)
	}
}
